#!/usr/bin/env bash
# Exit 0 when /dev/kvm exists and answers KVM_GET_API_VERSION, 1 otherwise,
# with the reason on stdout. CI runs the boot test only when this passes.
set -euo pipefail

if [[ ! -c /dev/kvm ]]; then
  echo "no /dev/kvm on this runner"
  exit 1
fi
# KVM_GET_API_VERSION is _IO(0xAE, 0x00) and returns 12 on every current kernel.
if ! out="$(sudo python3 -c '
import fcntl, os
fd = os.open("/dev/kvm", os.O_RDWR)
print(fcntl.ioctl(fd, 0xAE00))
' 2>&1)"; then
  echo "/dev/kvm is present but not usable: $out"
  exit 1
fi
if [[ "$out" != 12 ]]; then
  echo "/dev/kvm reports KVM API version $out (expected 12)"
  exit 1
fi
echo "/dev/kvm usable (KVM API version $out)"
