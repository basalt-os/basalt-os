#!/usr/bin/env python3
"""Drive the Basalt OS text installer over a VM's serial console.

Usage:
  drive-tui.py --socket SERIAL.sock --disk vda --key-out FILE [--log FILE]
               [--shell SHELL.sock --selinux-out FILE]

Connects to QEMU's serial chardev (a Unix socket), walks the wizard like a
person would (every screen, the review, the typed disk name), reads the
recovery key off the screen, stores it in --key-out (mode 0400), types its
first group to acknowledge it and confirms the end action. Exits 0 when the
installer reports the system installed and the end action ran.
"""
import argparse
import os
import re
import socket
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from liveshell import Shell  # noqa: E402

ANSI = re.compile(rb"\x1b\[[0-9;?<>=]*[ -/]*[@-~]|\x1b[()][A-Za-z0-9]|\x1b[=>78]|\x1b\][^\x07]*\x07")

ENTER, DOWN, TAB, ESC = b"\r", b"\x1b[B", b"\t", b"\x1b"


class Console:
    def __init__(self, path, log):
        self.s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        for _ in range(100):
            try:
                self.s.connect(path)
                break
            except OSError:
                time.sleep(0.3)
        else:
            raise SystemExit(f"cannot connect to {path}")
        self.s.setblocking(False)
        self.buf = b""
        self.log = open(log, "ab") if log else None

    def pump(self, seconds):
        end = time.time() + seconds
        while time.time() < end:
            try:
                data = self.s.recv(65536)
                if not data:
                    raise SystemExit("serial console closed")
                self.buf = (self.buf + data)[-400000:]
                if self.log:
                    self.log.write(data)
                    self.log.flush()
            except BlockingIOError:
                time.sleep(0.05)

    def text(self):
        return ANSI.sub(b"", self.buf).replace(b"\r", b"").decode("utf-8", "replace")

    def mark(self):
        self.buf = b""

    def wait(self, pattern, timeout, what=None):
        rx = re.compile(pattern, re.S)
        end = time.time() + timeout
        while time.time() < end:
            self.pump(0.5)
            m = rx.search(self.text())
            if m:
                return m
        tail = self.text()[-3000:]
        raise SystemExit(f"timeout waiting for {what or pattern!r}; screen tail:\n{tail}")

    def send(self, data, pause=0.6):
        if isinstance(data, str):
            data = data.encode()
        self.s.sendall(data)
        self.pump(pause)


def step(con, title, pattern, keys, timeout=60):
    print(f"--> {title}", flush=True)
    con.wait(pattern, timeout, title)
    con.mark()
    for k in keys:
        con.send(k)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--socket", required=True)
    ap.add_argument("--disk", default="vda")
    ap.add_argument("--key-out", required=True)
    ap.add_argument("--log")
    ap.add_argument("--boot-timeout", type=int, default=600)
    ap.add_argument("--install-timeout", type=int, default=3600)
    ap.add_argument("--shell", help="the lab ISO's root shell socket (second serial port)")
    ap.add_argument("--selinux-out", help="write the live system's SELinux mode and denials here")
    a = ap.parse_args()
    con = Console(a.socket, a.log)

    # The GRUB menu boots the default entry after its timeout; the live
    # system starts the text installer on this serial console.
    step(con, "welcome", r"Welcome.*enter start", [ENTER], a.boot_timeout)
    # The disk the plan names is in the list (and preselected).
    step(con, "disk", r"Where should Basalt OS be installed\?.*/dev/" + re.escape(a.disk), [ENTER], 30)
    step(con, "partitioning", r"Partitioning", [ENTER])
    step(con, "encryption", r"Disk encryption \(LUKS2\)", [ENTER])
    step(con, "system", r"Host name", [ENTER, ENTER])
    step(con, "packages", r"Package profile", [ENTER])
    # Root's SSH key comes from the plan; no administrator user.
    step(con, "accounts", r"Accounts.*Root SSH key", [ENTER, ENTER, ENTER, ENTER, ENTER])
    step(con, "network", r"Network.*DHCP", [ENTER])
    step(con, "repositories", r"Repositories.*tui-tools", [ENTER])
    step(con, "repository URL", r"Basalt repository.*Install from", [ENTER, ENTER])
    step(con, "review", r"Review: \d+ steps, exactly what will run.*lines 1-\d+ of \d+", [], 120)
    con.send(ENTER)
    step(con, "confirm", r"Type " + re.escape(a.disk) + r" to", [])
    con.send(a.disk)
    con.send(ENTER)
    print("--> installing", flush=True)
    t0 = time.time()

    m = con.wait(r"Recovery key: write it down now.*?Type the first group", a.install_timeout, "the recovery key")
    screen = con.text()[m.start():m.end()]
    groups = re.findall(r"\b[cbdefghijklnrtuv]{8}\b", screen)
    if len(groups) < 8:
        raise SystemExit(f"could not read the recovery key from the screen:\n{screen}")
    key = "-".join(groups[:8])
    fd = os.open(a.key_out, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o400)
    os.write(fd, (key + "\n").encode())
    os.close(fd)
    print(f"--> recovery key read from the screen ({len(key)} characters), stored in {a.key_out}", flush=True)
    con.mark()
    con.send("wrong!!")
    con.send(ENTER)
    con.wait(r"not the first group", 10, "the refusal of a wrong acknowledgement")
    con.send(groups[0])
    con.send(ENTER)

    m = con.wait(r"(Basalt OS is installed on /dev/\S+|The installation failed)", a.install_timeout, "the end of the installation")
    if "failed" in m.group(1):
        print(con.text()[-4000:])
        raise SystemExit("the installation failed")
    print(f"--> installed in {int(time.time() - t0)}s", flush=True)
    if a.shell and a.selinux_out:
        Shell(a.shell).selinux_report(a.selinux_out)
    con.wait(r"enter (poweroff|reboot) now", 10, "the end action")
    try:
        con.send(ENTER, 2)
    except SystemExit:
        pass  # the live system powered off before the pause ended: that is the end action
    print("--> end action confirmed", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
