"""The root shell on the lab live ISO's second serial port.

The lab ISO (tests/install-test.sh iso) boots with basalt.inst.debug-shell,
which logs root in on ttyS1 (and tty2); QEMU exposes the port as a Unix
socket. The shell runs in unconfined_t, so the installer started from it
runs in install_t like the services. The drivers use it to read the
installer's status and the live system's SELinux state. A release ISO has
no such shell.
"""
import json
import re
import socket
import time

ANSI = re.compile(r"\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)")


class Shell:
    def __init__(self, path):
        self.s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        for _ in range(100):
            try:
                self.s.connect(path)
                break
            except OSError:
                time.sleep(0.3)
        else:
            raise SystemExit(f"cannot connect to {path}")
        self.s.settimeout(0.5)
        self.n = 0

    def run(self, command, timeout=20):
        """Run command; return (exit status, output) or (-1, raw text)."""
        self.n += 1
        tag = f"__END{self.n}__"
        self.s.sendall(f"{command} 2>&1; echo {tag} $?\n".encode())
        buf, end = b"", time.time() + timeout
        while time.time() < end:
            try:
                buf += self.s.recv(65536)
            except socket.timeout:
                pass
            text = ANSI.sub("", buf.decode("utf-8", "replace")).replace("\r", "")
            # The shell may echo the command line more than once (typed, then
            # redrawn with the prompt); the output starts after the last echo
            # and ends at the tag followed by the exit status.
            m = re.search(rf"(?<!echo ){tag} (\d+)", text)
            if m:
                head = text[: m.start()]
                cuts = list(re.finditer(rf"echo\s+{tag}\s+\$\?", head))
                body = head[cuts[-1].end():] if cuts else head
                return int(m.group(1)), body.lstrip("\n")
        return -1, buf.decode("utf-8", "replace")

    def json(self, command):
        rc, out = self.run(command)
        if rc != 0:
            return None
        m = re.search(r"\{.*\}", out, re.S)
        try:
            return json.loads(m.group(0)) if m else None
        except ValueError:
            return None

    def selinux_report(self, path):
        """Write the live system's SELinux mode and AVC denials to path.

        Run at the end of an installation, before the end action powers the
        live system off; tests/vm-install-test.sh checks the file.
        """
        _, mode = self.run("getenforce")
        _, ctx = self.run("ps -eo label,args | grep -E '[b]asalt-installer|[c]age |[q]uickshell'")
        _, avc = self.run("journalctl -b -o cat --no-pager | grep 'avc: *[d]enied' || true", timeout=60)
        denials = [line for line in avc.splitlines() if "denied" in line]
        with open(path, "w", encoding="utf-8") as f:
            f.write(f"mode {mode.strip().splitlines()[-1] if mode.strip() else 'unknown'}\n")
            f.write(f"denials {len(denials)}\n")
            for line in ctx.splitlines():
                if line.strip():
                    f.write(f"process {line.strip()}\n")
            for line in denials:
                f.write(f"avc {line}\n")
        print(f"--> live SELinux: {mode.strip()}, {len(denials)} AVC denial(s)", flush=True)
