#!/usr/bin/env python3
"""Pick a GRUB menu entry over a lab VM's serial console.

Usage:
  grub-console.py VM --last-then INDEX [--timeout SECONDS] [--expect TEXT]
                     [--expect-snapshot N]
  grub-console.py VM --top INDEX [--expect TEXT]

Starts watching the serial console (start the VM right before or after
calling this), waits for the GRUB menu, stops the countdown, opens the
"Basalt OS snapshots" submenu, moves down INDEX entries (0 = first, the
newest snapshot) and boots it.

Only single-byte keys are sent, so nothing depends on how the serial line
splits a multi-byte escape sequence (an arrow key split in two reads as a
lone ESC, which opens GRUB's command line):
  s       the submenu's hotkey (basalt-snapshot-boot adds --hotkey=s)
  Ctrl-N  next entry (GRUB's emacs-style keys; Ctrl-E is the last entry)
  Ctrl-F  open or boot the highlighted entry (same as Enter or Right)
If the hotkey does not open the submenu (an older menu without it), the
script falls back to Ctrl-E (the last top-level entry) and Ctrl-F.

With --expect, the entry text must be on the screen before booting. With
--expect-snapshot N, GRUB must then print "Booting snapshot N" (the echo in
each entry), so a wrong selection fails instead of booting another snapshot.
Exit status 0 only when the expected entry was booted.

--top INDEX boots top-level entry INDEX (0 = the first) instead, e.g. an
older kernel; --expect then checks that TEXT is on the menu screen.
"""
import argparse
import os
import pty
import re
import select
import sys
import time

MENU = re.compile(rb"(Use the .* keys to (select|change)|Press enter to boot|Basalt OS snapshots|will be executed automatically)", re.I)
SUBMENU = re.compile(rb"Snapshot [0-9]+,", re.I)
GRUB_PROMPT = re.compile(rb"grub>")
ANSI = re.compile(rb"\x1b\[[0-9;?]*[A-Za-z]|\x1b[()][A-Za-z0-9]|\x1b[=>]")
CTRL_N = b"\x0e"
CTRL_E = b"\x05"
CTRL_F = b"\x06"
HOTKEY = b"s"


def clean(buf):
    return ANSI.sub(b"", buf)


def read_until(fd, pattern, deadline, buf=b""):
    while time.time() < deadline:
        r, _, _ = select.select([fd], [], [], 0.3)
        if fd in r:
            try:
                data = os.read(fd, 4096)
            except OSError:
                return False, buf
            buf = (buf + data)[-262144:]
            if pattern.search(clean(buf)):
                return True, buf
    return False, buf


def drain(fd, seconds, buf=b""):
    _, buf = read_until(fd, re.compile(rb"\Z\A"), time.time() + seconds, buf)
    return buf


def send(fd, data, pause=0.3):
    os.write(fd, data)
    time.sleep(pause)


def open_submenu(fd):
    """Open the snapshot submenu; True when its entries are on the screen."""
    # The hotkey first. A stray ESC from the countdown, if any, is closed by
    # "normal" (GRUB prompt -> menu).
    for attempt in range(3):
        send(fd, HOTKEY, 0.5)
        ok, buf = read_until(fd, re.compile(SUBMENU.pattern + rb"|" + GRUB_PROMPT.pattern), time.time() + 8)
        text = clean(buf)
        if ok and SUBMENU.search(text):
            return True
        if GRUB_PROMPT.search(text):
            print("grub: GRUB prompt seen; back to the menu")
            send(fd, b"normal\r", 2)
            read_until(fd, MENU, time.time() + 20)
            continue
        # No hotkey support: walk to the last top-level entry.
        print("grub: hotkey did not open the submenu; walking to the last entry")
        send(fd, CTRL_E, 0.5)
        send(fd, CTRL_F, 1.0)
        ok, buf = read_until(fd, SUBMENU, time.time() + 8)
        if ok:
            return True
    return False


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("vm")
    grp = ap.add_mutually_exclusive_group(required=True)
    grp.add_argument("--last-then", type=int, dest="index")
    grp.add_argument("--top", type=int)
    ap.add_argument("--timeout", type=int, default=240)
    ap.add_argument("--expect")
    ap.add_argument("--expect-snapshot", type=int)
    ap.add_argument("--connect", default="qemu:///system")
    args = ap.parse_args()

    pid, fd = pty.fork()
    if pid == 0:
        os.execvp("virsh", ["virsh", "-c", args.connect, "console", "--force", args.vm])

    deadline = time.time() + args.timeout
    try:
        ok, _ = read_until(fd, MENU, deadline)
        if not ok:
            print("grub: no GRUB menu seen on the serial console")
            return 1
        if args.top is not None:
            send(fd, CTRL_N, 0.3)          # stops the countdown
            send(fd, b"\x10", 0.3)        # Ctrl-P: back to the first entry
            for _ in range(args.top):
                send(fd, CTRL_N, 0.3)
            buf = drain(fd, 1.5)
            if args.expect and args.expect not in clean(buf).decode("utf-8", "replace"):
                print(f"grub: '{args.expect}' not on the menu screen")
                return 1
            send(fd, CTRL_F, 0.5)
            print(f"grub: booting top-level entry {args.top}")
            return 0
        if not open_submenu(fd):
            print("grub: could not open the snapshot submenu")
            return 1
        print("grub: snapshot submenu open")
        for _ in range(args.index):
            send(fd, CTRL_N, 0.3)
        buf = drain(fd, 1.5)
        text = clean(buf).decode("utf-8", "replace")
        entries = re.findall(r"Snapshot [0-9]+,[^\r\n|]*", text)
        if args.expect and entries and not any(args.expect in e for e in entries):
            print(f"grub: expected entry '{args.expect}' not shown; entries seen: {sorted(set(entries))[:10]}")
            return 1
        send(fd, CTRL_F, 0.5)
        want = rb"Booting snapshot " + (str(args.expect_snapshot).encode() + rb"\b" if args.expect_snapshot is not None else rb"[0-9]+")
        ok, buf = read_until(fd, re.compile(want + rb"|Booting snapshot [0-9]+"), time.time() + 20)
        seen = re.search(rb"Booting snapshot ([0-9]+)", clean(buf))
        if not seen:
            print("grub: no 'Booting snapshot' line seen")
            return 1 if args.expect_snapshot is not None else 0
        n = int(seen.group(1))
        print(f"grub: booting snapshot {n} (submenu entry {args.index})")
        if args.expect_snapshot is not None and n != args.expect_snapshot:
            print(f"grub: WRONG entry: expected snapshot {args.expect_snapshot}")
            return 1
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
