#!/usr/bin/env python3
"""Pick a GRUB menu entry over a lab VM's serial console.

Usage:
  grub-console.py VM --last-then INDEX [--timeout SECONDS] [--expect TEXT]

Starts watching the serial console (start the VM right before or after
calling this), waits for the GRUB menu, stops the countdown, moves to the
last top-level entry (the "Basalt OS snapshots" submenu), opens it, moves
down INDEX entries (0 = first, the newest snapshot) and boots it. With
--expect, the selected entry's text must appear on the console after the
submenu opens, otherwise nothing is booted and the exit status is 1.
"""
import argparse
import os
import pty
import re
import select
import sys
import time

MENU = re.compile(rb"(Use the .* keys to (select|change)|Press enter to boot|Basalt OS snapshots)", re.I)
SUBMENU = re.compile(rb"Snapshot [0-9]+", re.I)
ANSI = re.compile(rb"\x1b\[[0-9;?]*[A-Za-z]")
DOWN = b"\x1b[B"


def read_until(fd, pattern, deadline, buf=b""):
    while time.time() < deadline:
        r, _, _ = select.select([fd], [], [], 0.5)
        if fd in r:
            try:
                data = os.read(fd, 4096)
            except OSError:
                return False, buf
            buf = (buf + data)[-262144:]
            if pattern.search(ANSI.sub(b"", buf)):
                return True, buf
    return False, buf


def send(fd, data, pause=0.4):
    os.write(fd, data)
    time.sleep(pause)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("vm")
    ap.add_argument("--last-then", type=int, required=True, dest="index")
    ap.add_argument("--timeout", type=int, default=240)
    ap.add_argument("--expect")
    ap.add_argument("--connect", default="qemu:///system")
    args = ap.parse_args()

    pid, fd = pty.fork()
    if pid == 0:
        os.execvp("virsh", ["virsh", "-c", args.connect, "console", "--force", args.vm])

    deadline = time.time() + args.timeout
    try:
        ok, buf = read_until(fd, MENU, deadline)
        if not ok:
            print("grub: no GRUB menu seen on the serial console")
            return 1
        # Any key stops the countdown; many "down" presses reach the last entry.
        # An escape sequence split by the serial line reads as a lone ESC,
        # which at the top level opens GRUB's command line; "normal" goes
        # back to the menu, then try again.
        for attempt in range(3):
            for _ in range(30):
                send(fd, DOWN, 0.15)
            time.sleep(1)
            send(fd, b"\r", 1.5)
            ok, buf = read_until(fd, re.compile(rb"Snapshot [0-9]+|grub>"), time.time() + 20)
            if ok and SUBMENU.search(ANSI.sub(b"", buf)):
                break
            print("grub: not in the snapshot submenu (GRUB prompt?); returning to the menu")
            send(fd, b"normal\r", 2)
            read_until(fd, MENU, time.time() + 20)
            buf = b""
        else:
            print("grub: the last entry did not open the snapshot submenu")
            return 1
        print("grub: snapshot submenu open")
        for _ in range(args.index):
            send(fd, DOWN, 0.3)
        time.sleep(1)
        # Collect what the console shows now, to report the entry.
        _, buf = read_until(fd, re.compile(rb"\Z\A"), time.time() + 2, buf)
        text = ANSI.sub(b"", buf).decode("utf-8", "replace")
        entries = re.findall(r"Snapshot [0-9]+[^\r\n|]*", text)
        if args.expect and not any(args.expect in e for e in entries):
            print(f"grub: expected entry '{args.expect}' not shown; entries seen: {sorted(set(entries))[:10]}")
            return 1
        send(fd, b"\r", 1)
        print(f"grub: booting submenu entry {args.index}" + (f" ({args.expect})" if args.expect else ""))
        ok, _ = read_until(fd, re.compile(rb"Booting snapshot [0-9]+"), time.time() + 20)
        print("grub: kernel loading" if ok else "grub: no confirmation line seen (continuing)")
        return 0
    finally:
        try:
            os.write(fd, b"\x1d")
        except OSError:
            pass
        try:
            os.waitpid(pid, 0)
        except ChildProcessError:
            pass


if __name__ == "__main__":
    sys.exit(main())
