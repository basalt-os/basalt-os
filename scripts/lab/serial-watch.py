#!/usr/bin/env python3
"""Watch a lab VM's serial console for a pattern.

Usage:
  serial-watch.py VM --pattern REGEX [--timeout SECONDS] [--log FILE]

Exit status 0 when REGEX (case-insensitive) appears on the console within
the timeout, 1 otherwise. Prints the matching line. Start the VM right
before or after calling it. Read-only: nothing is typed.
"""
import argparse
import os
import pty
import re
import select
import sys
import time

ANSI = re.compile(rb"\x1b\[[0-9;?=]*[A-Za-z]|\x1b[()][A-Za-z0-9]|\x1b[=>]")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("vm")
    ap.add_argument("--pattern", required=True)
    ap.add_argument("--timeout", type=int, default=120)
    ap.add_argument("--log")
    ap.add_argument("--connect", default="qemu:///system")
    args = ap.parse_args()
    rx = re.compile(args.pattern.encode(), re.I)
    pid, fd = pty.fork()
    if pid == 0:
        os.execvp("virsh", ["virsh", "-c", args.connect, "console", "--force", args.vm])
    log = open(args.log, "ab") if args.log else None
    buf = b""
    end = time.time() + args.timeout
    try:
        while time.time() < end:
            r, _, _ = select.select([fd], [], [], 0.5)
            if fd not in r:
                continue
            try:
                data = os.read(fd, 4096)
            except OSError:
                break
            if log:
                log.write(ANSI.sub(b"", data))
            buf = (buf + data)[-65536:]
            text = ANSI.sub(b"", buf)
            m = rx.search(text)
            if m:
                line = text[max(0, text.rfind(b"\n", 0, m.start()) + 1):m.end() + 80].split(b"\n")[0]
                print("serial: " + line.decode("utf-8", "replace").strip())
                return 0
        print(f"serial: pattern not seen within {args.timeout}s")
        return 1
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
