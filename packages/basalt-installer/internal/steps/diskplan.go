package steps

import (
	"fmt"
	"strings"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/plan"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/probe"
)

// DiskPlan says, disk by disk and partition by partition, what the
// installation creates, formats, shares or leaves alone: the first part of
// the review, so the person checks it against the disks' models and serial
// numbers before typing the confirmation.
func DiskPlan(r Resolved) string {
	var b strings.Builder
	b.WriteString("What happens to each disk:\n")
	d := r.Disk
	if r.Plan.Target.Wipe {
		fmt.Fprintf(&b, "  %s: ERASED, then partitioned for Basalt OS\n", plan.DescribeDisk(d))
		for _, h := range d.Holds {
			fmt.Fprintf(&b, "      lost: %s\n", h)
		}
		for _, s := range d.Systems {
			fmt.Fprintf(&b, "      lost: the system %q\n", s)
		}
	} else {
		fmt.Fprintf(&b, "  %s: keeps its partitions; only what is listed as new or formatted changes\n", plan.DescribeDisk(d))
	}
	for _, np := range r.NewParts {
		size := probe.HumanSize(np.Bytes)
		if np.Bytes == 0 {
			size = "the rest" // sgdisk's size 0 on an erased disk
		}
		fmt.Fprintf(&b, "      new        %-16s %10s  %s (formatted)\n", np.Dev, size, partRole(np.Role))
	}
	if !r.Plan.Target.Wipe {
		for _, part := range d.Partitions {
			switch {
			case reused(r, part.Path):
				role := "system"
				if part.Path == r.BootDev {
					role = "/boot"
				}
				fmt.Fprintf(&b, "      FORMATTED  %-16s %10s  %s, chosen: was %s\n", part.Path, probe.HumanSize(part.Bytes), role, orNone(part.FSType))
			case r.ESPShared && part.Path == r.ESPDev:
				fmt.Fprintf(&b, "      shared     %-16s %10s  EFI system: Basalt OS adds EFI/fedora, nothing else changes\n", part.Path, probe.HumanSize(part.Bytes))
			case r.Existing != nil && part.Path == r.Existing.Device:
				fmt.Fprintf(&b, "      adopted    %-16s %10s  existing /home: opened and mounted, never formatted\n", part.Path, probe.HumanSize(part.Bytes))
			default:
				fmt.Fprintf(&b, "      untouched  %s\n", part.Describe())
			}
		}
	}
	for _, od := range r.OtherDisks {
		what := "empty"
		if len(od.Holds) > 0 {
			what = strings.Join(od.Holds, "; ")
		}
		line := fmt.Sprintf("  %s: untouched (%s)", plan.DescribeDisk(od), what)
		if r.Existing != nil {
			for _, part := range od.Partitions {
				if part.Path == r.Existing.Device {
					line = fmt.Sprintf("  %s: untouched except the existing /home %s (opened and mounted, never formatted)", plan.DescribeDisk(od), part.Path)
				}
			}
		}
		b.WriteString(line + "\n")
	}
	if r.DualBoot() {
		var esps []string
		for _, e := range r.OtherESPs {
			esps = append(esps, e.Path)
		}
		fmt.Fprintf(&b, "  Boot menu: the other systems' boot loaders on %s are listed (read-only search); the firmware's boot entries are kept, Basalt OS is added first\n",
			strings.Join(esps, ", "))
	}
	return b.String()
}

func reused(r Resolved, path string) bool {
	for _, p := range r.Reused {
		if p.Path == path {
			return true
		}
	}
	return false
}

func orNone(s string) string {
	if s == "" {
		return "no file system"
	}
	return s
}
