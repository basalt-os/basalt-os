package explain

import (
	"strings"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
)

// Risk levels of a planned change.
const (
	RiskLow    = 1
	RiskMedium = 2
	RiskHigh   = 3
)

// RiskName is the word for a level.
func RiskName(level int) string {
	switch level {
	case RiskHigh:
		return "high"
	case RiskMedium:
		return "medium"
	}
	return "low"
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

func riskOf(a action.Action) (int, string) {
	switch a.Kind {
	case action.SELinuxRestorecon:
		return RiskLow, "only puts back the label the policy expects"
	case action.UnitRestart:
		return RiskLow, a.Params["unit"] + " is briefly unavailable while it restarts"
	case action.DnfClean:
		return RiskLow, "cached packages are downloaded again when needed"
	case action.SELinuxFcontext:
		return RiskMedium, "adds a lasting SELinux rule for " + a.Params["path"]
	case action.SELinuxPort:
		return RiskMedium, "adds a lasting SELinux label for " + a.Params["proto"] + " port " + a.Params["port"]
	case action.SELinuxBoolean:
		return RiskMedium, "changes what the SELinux policy allows, for every program the boolean covers"
	case action.FileRestore:
		return RiskMedium, "replaces the current " + a.Params["path"]
	case action.JournalVacuum:
		return RiskMedium, "old log entries are deleted"
	case action.SnapshotRollback:
		return RiskHigh, "the whole system, but not your data, goes back to an earlier state at the next boot"
	case action.SnapshotDelete:
		return RiskHigh, "snapshot " + a.Params["snapshot"] + " is gone for good and can no longer be rolled back to"
	case action.DriverInstall:
		return RiskHigh, "the graphics driver changes at the next start; if it does not work there, the start after it uses nouveau again"
	case action.UpdateCheck:
		return RiskLow, "only the list of available packages is downloaded"
	case action.UpdateInstall:
		return RiskMedium, "programs and system parts are replaced by newer versions; some need a restart"
	case action.UpdateRollback:
		return RiskHigh, "the whole system, but not your data, goes back to how it was before the update at the next boot"
	case action.RepoEnable:
		if strings.HasSuffix(a.Params["repo"], "-testing") {
			return RiskMedium, "updates from now on may include preview builds, which can break things"
		}
		return RiskLow, "updates from now on also come from " + a.Params["repo"]
	case action.RepoDisable:
		return RiskLow, "no more updates come from " + a.Params["repo"] + "; what is installed stays"
	case action.SourceAdd:
		return RiskHigh, "software from " + a.Params["name"] + " can change the whole system: its signing key becomes trusted"
	case action.SourceRemove:
		return RiskLow, "no more software comes from " + a.Params["id"] + "; what is installed from it stays"
	case action.KeyboardSystem:
		return RiskLow, "the login screen and the console type with " + a.Params["layouts"] + "; people's own keyboard settings stay"
	}
	return RiskHigh, "unknown change"
}

// Risk returns the highest level of the changes and why, one note per
// change at that level.
func Risk(acts []action.Action) (int, string) {
	level := 0
	var notes []string
	for _, a := range acts {
		l, n := riskOf(a)
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
// snapshots itself). The command to run is rendered separately.
func Undo(acts []action.Action) (text string, rollbackCmd bool) {
	toggles := len(acts) > 0
	for _, a := range acts {
		if (a.Kind != action.RepoEnable && a.Kind != action.RepoDisable) || a.Params["definition"] != "" {
			toggles = false
		}
	}
	if toggles {
		return "turn the channel back the other way in Settings, Updates and channels, or with basalt channels; no snapshot is needed for one setting, and every update takes its own.", false
	}
	if len(acts) == 1 && acts[0].Kind == action.KeyboardSystem {
		return "choose other layouts in Settings, Keyboard, or with basalt keyboard set; no snapshot is needed for one setting.", false
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
			caveats = append(caveats, "deleting snapshot "+a.Params["snapshot"]+" cannot be undone")
		case action.JournalVacuum:
			caveats = append(caveats, "deleted journal files cannot be brought back")
		case action.DnfClean:
			caveats = append(caveats, "removed package files are simply downloaded again when needed")
		case action.SELinuxFcontext, action.SELinuxRestorecon:
			if v := DataVolume(a.Params["path"]); v != "" {
				caveats = append(caveats, "the labels of files under "+v+" are on a separate volume and stay as they are")
			}
		}
	}
	if rollback {
		return "your current system stays as a snapshot, so you can return to it the same way (basalt snapshots lists them).", false
	}
	if !reversible {
		return strings.Join(caveats, "; ") + ".", false
	}
	text = "a snapshot is taken before and after, so you can go back to the state before the change; a rollback takes effect at the next boot."
	if len(caveats) > 0 {
		text += " Note: " + strings.Join(caveats, "; ") + "."
	}
	return text, true
}
