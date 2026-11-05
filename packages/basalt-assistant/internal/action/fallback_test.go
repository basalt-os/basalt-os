package action

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withFallback points desktop.fallback at a temporary tree; gnome and gdm
// say which of GNOME's session and GDM's unit exist, dm is the display
// manager link ("" for none).
func withFallback(t *testing.T, gnome, gdm bool, dm string) {
	t.Helper()
	d := t.TempDir()
	old := FallbackPaths
	FallbackPaths.GNOMESession = filepath.Join(d, "gnome.desktop")
	FallbackPaths.GDMUnits = []string{filepath.Join(d, "gdm.service")}
	FallbackPaths.DisplayManager = filepath.Join(d, "display-manager.service")
	t.Cleanup(func() { FallbackPaths = old })
	if gnome {
		_ = os.WriteFile(FallbackPaths.GNOMESession, nil, 0o644)
	}
	if gdm {
		_ = os.WriteFile(FallbackPaths.GDMUnits[0], nil, 0o644)
	}
	if dm != "" {
		if err := os.Symlink("/usr/lib/systemd/system/"+dm+".service", FallbackPaths.DisplayManager); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDesktopFallback(t *testing.T) {
	withFallback(t, true, true, "greetd")
	a := Action{Kind: DesktopFallback, Params: map[string]string{"target": "gnome"}}
	cmds, err := a.Commands()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range cmds {
		got = append(got, strings.Join(c.Argv, " "))
	}
	if strings.Join(got, "; ") != "systemctl disable greetd.service; systemctl enable --force gdm.service" {
		t.Fatalf("gnome: %v", got)
	}
	back := Action{Kind: DesktopFallback, Params: map[string]string{"target": "basalt"}}
	cmds, _ = back.Commands()
	if len(cmds) != 2 || strings.Join(cmds[0].Argv, " ") != "systemctl disable gdm.service" || strings.Join(cmds[1].Argv, " ") != "systemctl enable --force greetd.service" {
		t.Fatalf("basalt: %+v", cmds)
	}
	// The check reads the display-manager.service link.
	chk := a.Verify(time.Now())
	if len(chk) != 1 {
		t.Fatal(chk)
	}
	if ok, seen := chk[0].Func(context.Background(), nil); ok || seen != "greetd" {
		t.Errorf("before the switch: %v %s", ok, seen)
	}
	_ = os.Remove(FallbackPaths.DisplayManager)
	_ = os.Symlink("/usr/lib/systemd/system/gdm.service", FallbackPaths.DisplayManager)
	if ok, _ := chk[0].Func(context.Background(), nil); !ok {
		t.Error("after the switch the check fails")
	}
	for _, bad := range []map[string]string{{"target": "kde"}, {"target": "gnome", "unit": "sshd"}, {}} {
		if err := (Action{Kind: DesktopFallback, Params: bad}).Validate(); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

// Without basalt-desktop-gnome-fallback (GNOME's session or GDM missing)
// switching to GDM is refused; going back to Basalt is always possible.
func TestDesktopFallbackNeedsGNOME(t *testing.T) {
	for _, c := range []struct{ gnome, gdm bool }{{false, false}, {true, false}, {false, true}} {
		withFallback(t, c.gnome, c.gdm, "greetd")
		if err := (Action{Kind: DesktopFallback, Params: map[string]string{"target": "gnome"}}).Validate(); err == nil ||
			!strings.Contains(err.Error(), "basalt-desktop-gnome-fallback") {
			t.Errorf("gnome %v gdm %v: %v", c.gnome, c.gdm, err)
		}
		if err := (Action{Kind: DesktopFallback, Params: map[string]string{"target": "basalt"}}).Validate(); err != nil {
			t.Errorf("basalt refused: %v", err)
		}
	}
}
