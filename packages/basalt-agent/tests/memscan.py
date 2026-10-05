#!/usr/bin/env python3
"""Lab attack helper (never shipped): try to read another process's memory.

    memscan.py PID            print every readable memory region of PID
                              (through /proc/PID/maps and /proc/PID/mem)
    memscan.py --ptrace PID   exit 0 when PTRACE_ATTACH succeeds
    memscan.py --readv PID    exit 0 when process_vm_readv is permitted
"""
import ctypes
import errno
import os
import re
import sys

libc = ctypes.CDLL(None, use_errno=True)


def scan(pid):
    with open(f"/proc/{pid}/maps") as maps, open(f"/proc/{pid}/mem", "rb", 0) as mem:
        for line in maps:
            m = re.match(r"([0-9a-f]+)-([0-9a-f]+) (\S+)", line)
            if not m or "r" not in m.group(3):
                continue
            lo, hi = int(m.group(1), 16), int(m.group(2), 16)
            try:
                mem.seek(lo)
                sys.stdout.buffer.write(mem.read(min(hi - lo, 64 << 20)))
            except OSError:
                pass


def ptrace(pid):
    PTRACE_ATTACH, PTRACE_DETACH = 16, 17
    libc.ptrace.argtypes = [ctypes.c_long, ctypes.c_long, ctypes.c_void_p, ctypes.c_void_p]
    if libc.ptrace(PTRACE_ATTACH, pid, None, None) != 0:
        print("ptrace:", os.strerror(ctypes.get_errno()))
        return 1
    os.waitpid(pid, 0)
    libc.ptrace(PTRACE_DETACH, pid, None, None)
    return 0


def readv(pid):
    class iovec(ctypes.Structure):
        _fields_ = [("base", ctypes.c_void_p), ("len", ctypes.c_size_t)]
    buf = ctypes.create_string_buffer(16)
    local = iovec(ctypes.cast(buf, ctypes.c_void_p), 16)
    remote = iovec(ctypes.c_void_p(0x400000), 16)
    n = libc.process_vm_readv(pid, ctypes.byref(local), 1, ctypes.byref(remote), 1, 0)
    e = ctypes.get_errno()
    print("process_vm_readv:", n, os.strerror(e) if n < 0 else "")
    # EFAULT means the permission check passed (only the address was bad).
    return 0 if n >= 0 or e == errno.EFAULT else 1


if sys.argv[1] == "--ptrace":
    sys.exit(ptrace(int(sys.argv[2])))
if sys.argv[1] == "--readv":
    sys.exit(readv(int(sys.argv[2])))
scan(int(sys.argv[1]))
