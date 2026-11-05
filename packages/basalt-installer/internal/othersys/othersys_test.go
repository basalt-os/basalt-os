package othersys

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func touch(t *testing.T, root string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestScan(t *testing.T) {
	root := t.TempDir()
	// FAT keeps whatever case the writer used.
	touch(t, root, "EFI/Microsoft/Boot/bootmgfw.efi", "EFI/fedora/shimx64.efi", "EFI/fedora/grubx64.efi",
		"EFI/ubuntu/GRUBX64.EFI", "EFI/BOOT/BOOTX64.EFI", "EFI/Dell/logs.txt")
	got := Scan(root, "/dev/nvme0n1p1", "ABCD-1234", nil)
	var names []string
	for _, l := range got {
		names = append(names, l.Name+"="+l.Path)
	}
	want := "Windows Boot Manager=/EFI/Microsoft/Boot/bootmgfw.efi,Fedora=/EFI/fedora/shimx64.efi,Ubuntu=/EFI/ubuntu/GRUBX64.EFI"
	if strings.Join(names, ",") != want {
		t.Fatalf("got %s", strings.Join(names, ","))
	}
	// Basalt OS's own directory on a shared ESP is skipped.
	got = Scan(root, "/dev/nvme0n1p1", "ABCD-1234", []string{"fedora"})
	for _, l := range got {
		if l.Name == "Fedora" {
			t.Fatal("skipped directory listed")
		}
	}
	if s := Script(Scan(root, "/dev/sda1", "ABCD-1234", nil)); !strings.Contains(s, "menuentry 'Fedora (/dev/sda1)' --class os {") {
		t.Errorf("one class for a Linux entry:\n%s", s)
	}
	s := Script(got)
	for _, w := range []string{"menuentry 'Windows Boot Manager (/dev/nvme0n1p1)' --class windows", "search --no-floppy --fs-uuid --set=root ABCD-1234",
		"chainloader /EFI/Microsoft/Boot/bootmgfw.efi", "cat <<'EOF'"} {
		if !strings.Contains(s, w) {
			t.Errorf("script lacks %q:\n%s", w, s)
		}
	}
}

func TestParseArg(t *testing.T) {
	p, err := ParseArg("/dev/sda1=ABCD-1234:fedora,BOOT")
	if err != nil || p.Dev != "/dev/sda1" || p.FSUUID != "ABCD-1234" || len(p.Skip) != 2 {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := ParseArg("sda1"); err == nil {
		t.Fatal("not a device")
	}
}
