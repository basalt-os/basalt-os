#!/usr/bin/env python3
"""Confirm a pending MOK request in shim's MokManager over a lab VM's serial console.

Usage:
  mok-console.py VM --password-file FILE [--action enroll|delete] [--timeout SECONDS] [--log FILE]

`mokutil --import CERT` (or `--delete CERT`) only queues a request; shim
starts MokManager on the next boot, and a person at the console has to
confirm it with the one-time password chosen at import time. This script is
that person, for the lab: it waits for "Press any key to perform MOK
management", opens the menu, picks "Enroll MOK" (or "Delete MOK"), skips the
key view, answers "Yes", types the password (read from FILE, never printed)
and reboots. Start the VM right before calling it.

MokManager is driven with arrow keys sent as one write each (OVMF's
terminal driver collects the escape sequence) and Enter. --log writes the
screen text seen (ANSI codes removed) for debugging; it never contains the
password, which MokManager does not echo.
"""
import argparse
import os
import pty
import re
import select
import sys
import time

ANSI = re.compile(rb"\x1b\[[0-9;?]*[A-Za-z]|\x1b[()][A-Za-z0-9]|\x1b[=>]")
DOWN = b"\x1b[B"
ENTER = b"\r"


class Console:
    def __init__(self, vm, connect, logfile):
        self.pid, self.fd = pty.fork()
        if self.pid == 0:
            os.execvp("virsh", ["virsh", "-c", connect, "console", "--force", vm])
        self.buf = b""
        self.log = open(logfile, "ab") if logfile else None

    def read(self, seconds):
        end = time.time() + seconds
        while time.time() < end:
            r, _, _ = select.select([self.fd], [], [], 0.2)
            if self.fd in r:
                try:
                    data = os.read(self.fd, 4096)
                except OSError:
                    return
                if self.log:
                    self.log.write(ANSI.sub(b"", data))
                    self.log.flush()
                self.buf = (self.buf + data)[-131072:]

    def text(self):
        return ANSI.sub(b"", self.buf).decode("utf-8", "replace")

    def wait(self, pattern, seconds):
        rx = re.compile(pattern, re.I)
        end = time.time() + seconds
        while time.time() < end:
            if rx.search(self.text()):
                return True
            self.read(0.5)
        return bool(rx.search(self.text()))

    def send(self, data, pause=0.8):
        os.write(self.fd, data)
        time.sleep(pause)
        self.read(0.3)

    def clear(self):
        self.buf = b""

    def close(self):
        try:
            os.write(self.fd, b"\x1d")
        except OSError:
            pass
        try:
            os.waitpid(self.pid, 0)
        except ChildProcessError:
            pass


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("vm")
    ap.add_argument("--password-file", required=True)
    ap.add_argument("--action", choices=["enroll", "delete"], default="enroll")
    ap.add_argument("--timeout", type=int, default=180)
    ap.add_argument("--log")
    ap.add_argument("--connect", default="qemu:///system")
    args = ap.parse_args()
    with open(args.password_file, "rb") as f:
        password = f.read().strip()

    con = Console(args.vm, args.connect, args.log)
    try:
        if not con.wait(r"Press any key to perform MOK management", args.timeout):
            print("mok: MokManager prompt not seen (no pending request?)")
            return 1
        con.clear()
        con.send(b" ", 2)
        if not con.wait(r"Continue boot", 15):
            print("mok: MokManager menu not seen")
            return 1
        print("mok: MokManager menu open")
        # Main menu: Continue boot / Enroll MOK (or Delete MOK) / Enroll key from disk / ...
        target = "Enroll MOK" if args.action == "enroll" else "Delete MOK"
        # Items are drawn in boxes on one screen line; take their order from
        # the text after the last "Continue boot".
        text = con.text()
        menu = text[text.rfind("Continue boot"):]
        items = re.findall(r"Continue boot|Reset MOK|Enroll MOK|Delete MOK|Enroll key from disk|"
                           r"Enroll hash from disk|Change Secure Boot state|Set MOK password|"
                           r"Clear MOK password|Change SBAT state|Reboot", menu)
        if target not in items:
            print(f"mok: '{target}' not in the menu ({items})")
            return 1
        steps = items.index(target)
        con.clear()
        for _ in range(steps):
            con.send(DOWN, 0.6)
        con.send(ENTER, 2)
        # "[Enroll MOK]" / "[Delete MOK]": View key 0 / Continue
        if not con.wait(r"View key|Continue", 15):
            print("mok: key screen not seen")
            return 1
        con.clear()
        con.send(DOWN, 0.6)
        con.send(ENTER, 2)
        # "Enroll the key(s)?" / "Delete the key(s)?": No / Yes
        if not con.wait(r"the key\(s\)\?", 15):
            print("mok: confirmation question not seen")
            return 1
        con.clear()
        con.send(DOWN, 0.6)
        con.send(ENTER, 2)
        if not con.wait(r"Password", 15):
            print("mok: password prompt not seen")
            return 1
        con.clear()
        for ch in password:
            con.send(bytes([ch]), 0.05)
        con.send(ENTER, 3)
        # Back to the main menu with "Reboot" first.
        if not con.wait(r"Reboot", 20):
            print("mok: no Reboot item after the password (wrong password?)")
            return 1
        print(f"mok: {args.action} confirmed; rebooting")
        con.send(ENTER, 2)
        return 0
    finally:
        con.close()


if __name__ == "__main__":
    sys.exit(main())
