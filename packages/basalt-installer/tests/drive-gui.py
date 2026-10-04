#!/usr/bin/env python3
"""Drive the Basalt OS graphical installer in a VM and take screenshots.

Usage:
  drive-gui.py --qmp QMP.sock --shell SHELL.sock --disk vda --shots DIR --key-out FILE

Input goes through QEMU's QMP socket (absolute pointer on a USB tablet,
keyboard keys); screenshots are QMP screendumps converted to PNG. The
installer's state is read with `basalt-installer client status` on the lab
ISO's root shell (systemd.debug_shell on the second serial port), which is
also where the recovery key is read, so the driver can type its first group
into the GUI like a person who copied it down.

The page layout is fixed (window 1280x800 by default in cage): the bottom
bar's main button, the review page's confirmation field and button, and the
recovery dialog's field and button are clicked at the positions below,
scaled to the screen size of the first screenshot.
"""
import argparse
import json
import os
import re
import socket
import struct
import sys
import time
import zlib

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from liveshell import Shell  # noqa: E402

# Click targets as fractions of the screen (x, y), measured on 1280x800.
TARGETS = {
    "primary": (1180 / 1280, 764 / 800),      # Continue / Review the plan / Power off
    "confirm_field": (760 / 1280, 567 / 800),  # review: "Type vda to confirm"
    "erase": (394 / 1280, 613 / 800),          # review: "Erase and install"
    "proof_field": (640 / 1280, 498 / 800),    # recovery dialog: first group
    "ack": (385 / 1280, 548 / 800),            # recovery dialog: "I stored the recovery key"
}


class QMP:
    def __init__(self, path):
        self.s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        for _ in range(100):
            try:
                self.s.connect(path)
                break
            except OSError:
                time.sleep(0.3)
        self.f = self.s.makefile("rwb")
        self._read()
        self.cmd("qmp_capabilities")

    def _read(self):
        while True:
            line = self.f.readline()
            if not line:
                raise SystemExit("QMP closed")
            msg = json.loads(line)
            if "event" in msg:
                continue
            return msg

    def cmd(self, name, **args):
        self.f.write(json.dumps({"execute": name, "arguments": args}).encode() + b"\n")
        self.f.flush()
        r = self._read()
        if "error" in r:
            raise RuntimeError(f"{name}: {r['error']}")
        return r.get("return")


def ppm_to_png(ppm, png):
    with open(ppm, "rb") as f:
        data = f.read()
    m = re.match(rb"P6\s+(\d+)\s+(\d+)\s+(\d+)\s", data)
    w, h = int(m.group(1)), int(m.group(2))
    px = data[m.end():]
    raw = b"".join(b"\x00" + px[y * w * 3:(y + 1) * w * 3] for y in range(h))

    def chunk(t, d):
        c = struct.pack(">I", len(d)) + t + d
        return c + struct.pack(">I", zlib.crc32(t + d) & 0xFFFFFFFF)
    with open(png, "wb") as f:
        f.write(b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0))
                + chunk(b"IDAT", zlib.compress(raw, 6)) + chunk(b"IEND", b""))
    os.remove(ppm)
    return w, h


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--qmp", required=True)
    ap.add_argument("--shell", required=True)
    ap.add_argument("--disk", default="vda")
    ap.add_argument("--shots", required=True)
    ap.add_argument("--key-out", required=True)
    ap.add_argument("--install-timeout", type=int, default=3600)
    ap.add_argument("--selinux-out", help="write the live system's SELinux mode and denials here")
    a = ap.parse_args()
    os.makedirs(a.shots, exist_ok=True)
    q = QMP(a.qmp)
    size = [1280, 800]
    count = [0]

    def shot(name):
        count[0] += 1
        ppm = os.path.join(a.shots, f"{count[0]:02d}-{name}.ppm")
        q.cmd("screendump", filename=ppm)
        size[0], size[1] = ppm_to_png(ppm, ppm[:-4] + ".png")
        print(f"--> screenshot {count[0]:02d}-{name}.png ({size[0]}x{size[1]})", flush=True)

    def click(target, pause=1.5):
        fx, fy = TARGETS[target]
        x, y = int(fx * 32767), int(fy * 32767)
        q.cmd("input-send-event", events=[
            {"type": "abs", "data": {"axis": "x", "value": x}},
            {"type": "abs", "data": {"axis": "y", "value": y}}])
        time.sleep(0.2)
        for down in (True, False):
            q.cmd("input-send-event", events=[{"type": "btn", "data": {"down": down, "button": "left"}}])
            time.sleep(0.1)
        time.sleep(pause)

    def type_text(text):
        for ch in text:
            q.cmd("send-key", keys=[{"type": "qcode", "data": ch}])
            time.sleep(0.08)

    sh = Shell(a.shell)
    print("--> waiting for the engine and the GUI", flush=True)
    end = time.time() + 600
    while time.time() < end:
        st = sh.json("basalt-installer client status")
        rc, _ = sh.run("pgrep -x quickshell >/dev/null")
        if st is not None and rc == 0:
            break
        time.sleep(3)
    else:
        raise SystemExit("the graphical installer did not start")
    time.sleep(8)
    shot("welcome")
    for name in ("disk", "options", "accounts"):
        click("primary")
        shot(name)
    click("primary", pause=4)  # Review the plan: the engine makes the preview
    shot("review")
    click("confirm_field", pause=0.5)
    type_text(a.disk)
    time.sleep(1)
    shot("review-confirmed")
    click("erase", pause=3)

    t0 = time.time()
    key = None
    progress_shot = False
    while time.time() - t0 < a.install_timeout:
        st = sh.json("basalt-installer client status") or {}
        if not progress_shot and st.get("state") == "installing" and time.time() - t0 > 30:
            shot("installing")
            progress_shot = True
        if st.get("recovery_key_pending"):
            for _ in range(5):
                k = sh.json("basalt-installer client recovery_key") or {}
                key = k.get("key")
                if key:
                    break
                time.sleep(2)
            break
        if st.get("state") == "failed":
            shot("failed")
            raise SystemExit(f"the installation failed: {st.get('error')}")
        time.sleep(5)
    if not key:
        raise SystemExit("no recovery key")
    fd = os.open(a.key_out, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o400)
    os.write(fd, (key + "\n").encode())
    os.close(fd)
    time.sleep(2)
    shot("recovery-key")
    click("proof_field", pause=0.5)
    type_text(key.split("-")[0])
    time.sleep(0.5)
    click("ack", pause=2)
    st = sh.json("basalt-installer client status") or {}
    if not st.get("recovery_key_acknowledged"):
        shot("ack-failed")
        raise SystemExit("the acknowledgement typed into the GUI was not accepted")
    print("--> recovery key acknowledged in the GUI", flush=True)
    while time.time() - t0 < a.install_timeout:
        st = sh.json("basalt-installer client status") or {}
        if st.get("state") in ("succeeded", "failed"):
            break
        time.sleep(10)
    time.sleep(3)
    shot("done" if st.get("state") == "succeeded" else "failed")
    if st.get("state") != "succeeded":
        raise SystemExit(f"the installation failed: {st.get('error')}")
    print(f"--> installed in {int(time.time() - t0)}s", flush=True)
    if a.selinux_out:
        sh.selinux_report(a.selinux_out)
    click("primary", pause=2)  # Power off
    print("--> end action clicked", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
