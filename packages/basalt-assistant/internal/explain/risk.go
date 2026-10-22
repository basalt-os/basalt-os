package explain

import (
	"fmt"
	"strings"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/i18n"
)

// Risk levels of a planned change.
const (
	RiskLow    = 1
	RiskMedium = 2
	RiskHigh   = 3
)

// RiskName is the word for a level (English).
func RiskName(level int) string { return RiskNameIn(i18n.Tr{}, level) }

// RiskNameIn is RiskName in the language of tr.
func RiskNameIn(tr i18n.Tr, level int) string {
	switch level {
	case RiskHigh:
		return tr.T("high")
	case RiskMedium:
		return tr.T("medium")
	}
	return tr.T("low")
}

// DataVolumes are the subvolumes a root rollback leaves alone (the
// installer's layout, kickstart/basalt-server.ks).
var DataVolumes = []string{"/home", "/srv", "/var/log", "/var/cache", "/var/tmp", "/var/spool",
	"/var/lib/containers", "/var/lib/libvirt", "/var/lib/pgsql", "/var/lib/mysql"}

// DataVolume returns the data subvolume a path lives on, or "".
func DataVolume(path string) string {
	for _, v := range DataVolumes {
		if path == v || strings.HasPrefix(path, v+"/") {
			return v
		}
	}
	return ""
}

func riskOf(tr i18n.Tr, a action.Action) (int, string) {
	p := a.Params
	switch a.Kind {
	case action.SELinuxRestorecon:
		return RiskLow, tr.T("only puts back the label the policy expects")
	case action.UnitRestart:
		return RiskLow, fmt.Sprintf(tr.T("%s is briefly unavailable while it restarts"), p["unit"])
	case action.DnfClean:
		return RiskLow, tr.T("cached packages are downloaded again when needed")
	case action.SELinuxFcontext:
		return RiskMedium, fmt.Sprintf(tr.T("adds a lasting SELinux rule for %s"), p["path"])
	case action.SELinuxPort:
		return RiskMedium, fmt.Sprintf(tr.T("adds a lasting SELinux label for %s port %s"), p["proto"], p["port"])
	case action.SELinuxBoolean:
		return RiskMedium, tr.T("changes what the SELinux policy allows, for every program the boolean covers")
	case action.FileRestore:
		return RiskMedium, fmt.Sprintf(tr.T("replaces the current %s"), p["path"])
	case action.JournalVacuum:
		return RiskMedium, tr.T("old log entries are deleted")
	case action.SnapshotRollback:
		return RiskHigh, tr.T("the whole system, but not your data, goes back to an earlier state at the next boot")
	case action.SnapshotDelete:
		return RiskHigh, fmt.Sprintf(tr.T("snapshot %s is gone for good and can no longer be rolled back to"), p["snapshot"])
	case action.DriverInstall:
		return RiskHigh, tr.T("the graphics driver changes at the next start; if it does not work there, the start after it uses nouveau again")
	case action.UpdateCheck:
		return RiskLow, tr.T("only the list of available packages is downloaded")
	case action.UpdateInstall:
		return RiskMedium, tr.T("programs and system parts are replaced by newer versions; some need a restart")
	case action.UpdateRollback:
		return RiskHigh, tr.T("the whole system, but not your data, goes back to how it was before the update at the next boot")
	case action.RepoEnable:
		if strings.HasSuffix(p["repo"], "-testing") {
			return RiskMedium, tr.T("updates from now on may include preview builds, which can break things")
		}
		return RiskLow, fmt.Sprintf(tr.T("updates from now on also come from %s"), p["repo"])
	case action.RepoDisable:
		return RiskLow, fmt.Sprintf(tr.T("no more updates come from %s; what is installed stays"), p["repo"])
	case action.SourceAdd:
		return RiskHigh, fmt.Sprintf(tr.T("software from %s can change the whole system: its signing key becomes trusted"), p["name"])
	case action.SourceRemove:
		return RiskLow, fmt.Sprintf(tr.T("no more software comes from %s; what is installed from it stays"), p["id"])
	case action.KeyboardSystem:
		return RiskLow, fmt.Sprintf(tr.T("the login screen and the console type with %s; people's own keyboard settings stay"), p["layouts"])
	case action.AuditRun:
		if p["install"] == "yes" {
			return RiskLow, tr.T("one package is installed; its tests run as their own unprivileged account and change none of your files")
		}
		return RiskLow, tr.T("the tests run as their own unprivileged account and change none of your files")
	case action.RiskAccept:
		return RiskLow, fmt.Sprintf(tr.T("Security and Activity stops counting %s as a warning; nothing else changes"), p["item"])
	case action.RiskReview:
		return RiskLow, fmt.Sprintf(tr.T("%s counts as a warning again; nothing else changes"), p["item"])
	}
	return RiskHigh, tr.T("unknown change")
}

// Risk returns the highest level of the changes and why, one note per
// change at that level (English).
func Risk(acts []action.Action) (int, string) { return RiskIn(i18n.Tr{}, acts) }

// RiskIn is Risk in the language of tr.
func RiskIn(tr i18n.Tr, acts []action.Action) (int, string) {
	level := 0
	var notes []string
	for _, a := range acts {
		l, n := riskOf(tr, a)
		switch {
		case l > level:
			level, notes = l, []string{n}
		case l == level:
			notes = append(notes, n)
		}
	}
	return level, strings.Join(notes, "; ")
}

// Undo explains how to undo the changes after `basalt apply`, which takes a
// snapshot before and after (except for a rollback, which works on
// snapshots itself). The command to run is rendered separately. English.
func Undo(acts []action.Action) (text string, rollbackCmd bool) { return UndoIn(i18n.Tr{}, acts) }

// UndoIn is Undo in the language of tr.
func UndoIn(tr i18n.Tr, acts []action.Action) (text string, rollbackCmd bool) {
	toggles := len(acts) > 0
	for _, a := range acts {
		if (a.Kind != action.RepoEnable && a.Kind != action.RepoDisable) || a.Params["definition"] != "" {
			toggles = false
		}
	}
	if toggles {
		return tr.T("turn the channel back the other way in Settings, Updates and channels, or with basalt channels; no snapshot is needed for one setting, and every update takes its own."), false
	}
	if len(acts) == 1 && acts[0].Kind == action.KeyboardSystem {
		return tr.T("choose other layouts in Settings, Keyboard, or with basalt keyboard set; no snapshot is needed for one setting."), false
	}
	if len(acts) == 1 && acts[0].Kind == action.RiskAccept {
		return tr.T("choose Review again in Security and Activity; no snapshot is needed for one setting."), false
	}
	if len(acts) == 1 && acts[0].Kind == action.RiskReview {
		return tr.T("accept the risk again in Security and Activity; no snapshot is needed for one setting."), false
	}
	var caveats []string
	rollback, reversible := false, false
	for _, a := range acts {
		switch a.Kind {
		case action.SnapshotDelete, action.JournalVacuum, action.DnfClean:
		default:
			reversible = true
		}
		switch a.Kind {
		case action.SnapshotRollback, action.UpdateRollback:
			rollback = true
		case action.SnapshotDelete:
			caveats = append(caveats, fmt.Sprintf(tr.T("deleting snapshot %s cannot be undone"), a.Params["snapshot"]))
		case action.JournalVacuum:
			caveats = append(caveats, tr.T("deleted journal files cannot be brought back"))
		case action.DnfClean:
			caveats = append(caveats, tr.T("removed package files are simply downloaded again when needed"))
		case action.SELinuxFcontext, action.SELinuxRestorecon:
			if v := DataVolume(a.Params["path"]); v != "" {
				caveats = append(caveats, fmt.Sprintf(tr.T("the labels of files under %s are on a separate volume and stay as they are"), v))
			}
		}
	}
	if rollback {
		return tr.T("your current system stays as a snapshot, so you can return to it the same way (basalt snapshots lists them)."), false
	}
	if !reversible {
		return strings.Join(caveats, "; ") + ".", false
	}
	text = tr.T("a snapshot is taken before and after, so you can go back to the state before the change; a rollback takes effect at the next boot.")
	if len(caveats) > 0 {
		text += " " + fmt.Sprintf(tr.T("Note: %s."), strings.Join(caveats, "; "))
	}
	return text, true
}
