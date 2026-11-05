package probe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseBootEntries(t *testing.T) {
	out := "BootCurrent: 0001\nTimeout: 0 seconds\nBootOrder: 0001,0000,0002\n" +
		"Boot0000* Windows Boot Manager\tHD(1,GPT,0F3C1A2B-1111-2222-3333-444455556666,0x800,0x82000)/\\EFI\\Microsoft\\Boot\\bootmgfw.efi\n" +
		"Boot0001* Fedora\tHD(1,GPT,0f3c1a2b-1111-2222-3333-444455556666,0x800,0x82000)/File(\\EFI\\fedora\\shimx64.efi)\n" +
		"Boot0002* UEFI PXEv4 (MAC:525400123456)\tPciRoot(0x0)/Pci(0x2,0x0)/MAC(525400123456,1)/IPv4(0.0.0.0)\n"
	got := ParseBootEntries(out)
	if len(got) != 2 || got[0].Name != "Windows Boot Manager" || got[0].PartUUID != "0f3c1a2b-1111-2222-3333-444455556666" ||
		got[1].Name != "Fedora" || !strings.Contains(got[1].Loader, `\EFI\fedora\shimx64.efi`) {
		t.Fatalf("%+v", got)
	}
}

func TestReadSecureBootDB(t *testing.T) {
	dir := t.TempDir()
	if ReadSecureBootDB(dir).Read {
		t.Fatal("no variable, nothing read")
	}
	data := append([]byte{7, 0, 0, 0}, []byte("....CN=Microsoft Windows Production PCA 2011....CN=Microsoft Corporation UEFI CA 2011")...)
	if err := os.WriteFile(filepath.Join(dir, dbVar), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if db := ReadSecureBootDB(dir); !db.Read || !db.WindowsCA || !db.UEFICA {
		t.Fatalf("%+v", db)
	}
	if err := os.WriteFile(filepath.Join(dir, dbVar), append([]byte{7, 0, 0, 0}, []byte("CN=Basalt OS lab db")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if db := ReadSecureBootDB(dir); !db.Read || db.WindowsCA || db.UEFICA {
		t.Fatalf("custom db: %+v", db)
	}
}

func TestDescribeHolds(t *testing.T) {
	empty := Disk{Path: "/dev/vda", Contents: "empty"}
	DescribeHolds(&empty, nil)
	if empty.Caution != "" || len(empty.Holds) != 0 {
		t.Fatalf("empty: %+v", empty)
	}
	luks := Disk{Path: "/dev/nvme0n1", Contents: "1 partition: crypto_LUKS", Partitions: []Partition{{Path: "/dev/nvme0n1p1", FSType: "crypto_LUKS"}}}
	DescribeHolds(&luks, nil)
	if !strings.Contains(luks.Caution, "encrypted volume") || !strings.Contains(strings.Join(luks.Holds, ";"), "LUKS encrypted volume /dev/nvme0n1p1") {
		t.Fatalf("luks: %+v", luks)
	}
	win := Disk{Path: "/dev/nvme1n1", Contents: "4 partitions", Partitions: []Partition{
		{Path: "/dev/nvme1n1p1", Type: TypeESP, FSType: "vfat", PartUUID: "aaaa"},
		{Path: "/dev/nvme1n1p2", Type: TypeMSR},
		{Path: "/dev/nvme1n1p3", Type: TypeWindows, FSType: "ntfs"},
		{Path: "/dev/nvme1n1p4", FSType: "btrfs", Label: "fedora"}}}
	DescribeHolds(&win, []BootEntry{{Name: "Windows Boot Manager", PartUUID: "AAAA"}, {Name: "Fedora", PartUUID: "aaaa"}})
	if strings.Join(win.Systems, ",") != "Windows Boot Manager,Fedora" || !strings.Contains(win.Caution, "another operating system") {
		t.Fatalf("windows: %+v", win)
	}
	// Windows partition types without a boot entry still name Windows.
	win2 := Disk{Path: "/dev/sda", Partitions: []Partition{{Path: "/dev/sda1", Type: TypeMSR}, {Path: "/dev/sda2", FSType: "BitLocker"}}}
	DescribeHolds(&win2, nil)
	if len(win2.Systems) != 1 || win2.Systems[0] != "Windows" {
		t.Fatalf("bitlocker: %+v", win2)
	}
}

func TestReadLive(t *testing.T) {
	root := t.TempDir()
	for p, c := range map[string]string{
		"etc/locale.conf":                      "LANG=pt_BR.UTF-8\n",
		"etc/vconsole.conf":                    "KEYMAP=br-abnt2\n",
		"etc/X11/xorg.conf.d/00-keyboard.conf": "Section \"InputClass\"\n\tOption \"XkbLayout\" \"br\"\nEndSection\n",
	} {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755)
		if err := os.WriteFile(filepath.Join(root, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../usr/share/zoneinfo/America/Sao_Paulo", filepath.Join(root, "etc/localtime")); err != nil {
		t.Fatal(err)
	}
	l := Prober{Root: root}.readLive()
	if l.Locale != "pt_BR.UTF-8" || l.Keymap != "br-abnt2" || l.Layout != "br" || l.Timezone != "America/Sao_Paulo" {
		t.Fatalf("%+v", l)
	}
}
