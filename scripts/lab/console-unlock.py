#!/usr/bin/env python3
"""Watch a lab VM's serial console for the LUKS passphrase prompt.

Usage:
  console-unlock.py VM [--key-file FILE] [--now] [--timeout SECONDS]

Without --key-file it only reports whether the prompt appeared (the boot is
blocked waiting for a passphrase) and exits 0 if it did. With --key-file it
types the key (a recovery key or passphrase) at the prompt and waits for the
volume to unlock. The key is read from the file and never printed. --now types
the key right after attaching, for a prompt that was shown before attaching.
"""
import argparse
import os
import pty
import re
import select
import sys
import time

PROMPT = re.compile(rb"(Please enter passphrase for disk|Enter passphrase for|recovery key)", re.I)
UNLOCKED = re.compile(rb"Finished .*systemd-cryptsetup@|Reached target .*cryptsetup\.target|login:", re.I)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("vm")
    ap.add_argument("--key-file")
    ap.add_argument("--timeout", type=int, default=180)
    ap.add_argument("--now", action="store_true", help="the prompt is already on screen")
    ap.add_argument("--connect", default="qemu:///system")
    args = ap.parse_args()

    pid, fd = pty.fork()
    if pid == 0:
        os.execvp("virsh", ["virsh", "-c", args.connect, "console", "--force", args.vm])

    buf = b""
    deadline = time.time() + args.timeout
    prompted_at = None
    sent = False
    if args.now:
        buf = b"Please enter passphrase for disk (already shown)"
        time.sleep(2)
    try:
        while time.time() < deadline:
            r, _, _ = select.select([fd], [], [], 1.0)
            if fd in r:
                try:
                    data = os.read(fd, 4096)
                except OSError:
                    break
                buf = (buf + data)[-65536:]
            if prompted_at is None and PROMPT.search(buf):
                prompted_at = time.time()
                print("console: LUKS passphrase prompt is shown (automatic TPM2 unlock did not happen)")
                if not args.key_file:
                    return 0
                time.sleep(1)
                with open(args.key_file, "rb") as f:
                    key = f.read().strip()
                os.write(fd, key + b"\r")
                sent = True
                buf = b""
                print("console: recovery key typed at the prompt")
            if sent and UNLOCKED.search(buf):
                print("console: volume unlocked, boot continues")
                return 0
        if prompted_at is None:
            print("console: no passphrase prompt seen within the timeout")
        else:
            print("console: no unlock confirmation within the timeout")
        return 1
    finally:
        # Detach from the console (Ctrl-]) and reap virsh.
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
