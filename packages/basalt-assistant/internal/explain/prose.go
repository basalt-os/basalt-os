package explain

import (
	"math/rand"
	"strings"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
)

// Text is the prose of one finding, sentence by sentence.
type Text struct {
	Headline string // what is wrong, one sentence
	Why      string // the main evidence, one sentence (may be empty)
	Change   string // what applying will do (empty when nothing is proposed)
	Next     string // what the person can do next, prose (commands go in fixed slots)
}

// Prose is the paragraph a model may rewrite: what is wrong, why, and what
// applying will do.
func (t Text) Prose() string {
	return joinSentences(t.Headline, t.Why, t.Change)
}

func joinSentences(ss ...string) string {
	var out []string
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, " ")
}

// phraser picks among equivalent wordings: the first one without a random
// source (what people see), any of them with one (training data for the
// optional model, see basalt-eval render-data).
type phraser struct{ r *rand.Rand }

func (p phraser) pick(alts ...string) string {
	if p.r == nil || len(alts) == 1 {
		return alts[0]
	}
	return alts[p.r.Intn(len(alts))]
}

// Write renders the facts and the planned changes as text, with the
// default wording.
func Write(f *Facts, changes []action.Action) Text { return write(f, changes, phraser{}) }

// WriteVariant is Write with wording picked by r (training data).
func WriteVariant(f *Facts, changes []action.Action, r *rand.Rand) Text {
	return write(f, changes, phraser{r})
}

func write(f *Facts, changes []action.Action, p phraser) Text {
	var t Text
	switch f.Kind {
	case "unit":
		t = unitText(f, p)
	case "selinux":
		t = selinuxText(f, p)
	case "disk":
		t = diskText(f, p)
	case "dnf":
		t = dnfText(f, p)
	case "snapshot":
		t = snapshotText(f, p)
	case "driver":
		t = driverText(f, p)
	default:
		t.Headline = f.V("report")
	}
	if len(changes) > 0 && !f.Incomplete {
		t.Change = changeSentence(changes, f.Hint, p)
	}
	if f.Incomplete {
		t.Next = joinSentences("The diagnosis is incomplete ("+strings.TrimRight(f.V("error"), ".")+"), so nothing is proposed.",
			p.pick("Run it again in a moment.", "Please run it again in a moment."))
	} else if f.Review && f.Has("confidence") && len(changes) > 0 {
		t.Next = joinSentences(t.Next, "The assistant is not sure about this one (confidence "+f.V("confidence")+
			", threshold "+f.V("threshold")+"), so please read the evidence before applying.")
	}
	return t
}

// --- units -----------------------------------------------------------------------

func unitText(f *Facts, p phraser) Text {
	u := f.Subject
	var t Text
	if f.OK {
		t.Headline = u + p.pick(" is running normally.", " is up and running.", " is running fine.")
		t.Next = p.pick("Nothing to do.", "There is nothing to fix.")
		return t
	}
	switch f.Cause {
	case "not_found":
		t.Headline = p.pick("There is no unit called "+u+" on this system.", "This system has no unit named "+u+".")
		t.Next = "Check the name; systemctl list-units shows the units that exist."
	case "config_error":
		where := "its configuration"
		if f.Has("file") {
			where = f.V("file")
			if f.Has("line") {
				where += " (line " + f.V("line") + ")"
			}
		}
		t.Headline = p.pick(u+" is not running because of a mistake in "+where+".",
			u+" stopped: there is an error in "+where+".",
			u+" cannot start because "+where+" has an error.")
		if f.Has("checker") && f.Has("checker_msg") {
			t.Why = p.pick("The configuration check "+f.V("checker")+" says: "+f.V("checker_msg")+".",
				f.V("checker")+" reports: "+f.V("checker_msg")+".")
		} else if f.Has("journal") {
			t.Why = p.pick("The service log says: "+f.V("journal")+".", "Its log shows: "+f.V("journal")+".")
		}
		switch {
		case f.Hint:
			t.Next = "The background service cannot look inside snapshots, so confirm it as root: the assistant then tests the snapshot copy with the configuration check before proposing it."
		case f.Has("snapshot"):
			// The change sentence says what is restored.
		default:
			t.Next = p.pick("No snapshot has a working copy of the file, so fix it by hand and then restart "+u+".",
				"No snapshot holds a good copy, so please correct the file by hand and restart "+u+".")
		}
	case "selinux_denial":
		t.Headline = p.pick(u+" is not running because SELinux blocked something it needs.",
			u+" stopped because SELinux denied it access to something it needs.")
		t.Why = denialWhy(f, p)
		if f.Review {
			t.Next = p.pick("There is no safe automatic fix for at least one denial, so it needs a person to review it.",
				"At least one denial has no safe automatic fix; please have someone review it.")
		}
	case "port_conflict":
		t.Headline = p.pick(u+" cannot start because another program is already using its port.",
			u+" did not start: its port is taken by another program.")
		if f.Has("port") && f.Has("port_owner") {
			t.Why = "Port " + f.V("port") + " is held by " + f.V("port_owner") + "."
		} else if f.Has("port") {
			t.Why = "Port " + f.V("port") + " is already in use."
		}
		t.Next = p.pick("Stop the other program or give "+u+" a different port. Nothing is changed automatically.",
			"Either stop the other program or move "+u+" to another port. The assistant does not change this on its own.")
	case "dependency_failed":
		t.Headline = p.pick(u+" did not start because a unit it depends on failed.",
			u+" could not start: something it needs failed first.")
		if f.Has("dep") {
			if st := orWord(f.V("dep_state"), "failed"); st == "failed" {
				t.Why = f.V("dep") + " has failed."
			} else {
				t.Why = f.V("dep") + " is " + st + "."
			}
			t.Next = p.pick("Fix "+f.V("dep")+" first; "+u+" can start once it runs.", "Look at "+f.V("dep")+" first.")
		}
	case "disk_full":
		t.Headline = p.pick(u+" failed while the disk was full.", u+" stopped because it ran out of disk space.")
		if f.Has("fs_path") {
			t.Why = f.V("fs_path") + " is " + f.V("fs_pct") + " % full."
		}
		t.Next = "Free some space first; the disk report shows what is using it."
	case "missing_file":
		t.Headline = p.pick(u+" failed because a file it needs does not exist.", u+" stopped: a file it reads is missing.")
		if f.Has("journal") {
			t.Why = "Its log says: " + f.V("journal") + "."
		}
		t.Next = p.pick("Create or restore the missing file, then start "+u+" again.",
			"Put the missing file back (or fix the setting that names it), then start "+u+" again.")
	case "oom":
		t.Headline = p.pick(u+" ran out of memory and was stopped by the kernel.", u+" used too much memory and was killed.")
		switch {
		case f.Has("memory_max"):
			t.Why = "It reached its memory limit (MemoryMax=" + f.V("memory_max") + ")."
		case f.V("oom_scope") == "system":
			t.Why = "The whole system ran out of memory at the time."
		case f.V("oom_scope") == "cgroup":
			t.Why = "It hit a memory limit of its control group or slice."
		}
		t.Next = p.pick("A plain restart would most likely end the same way, so none is proposed. Check how much memory it uses and its limits; raise the limit if it is too low, otherwise find out why it needs so much.",
			"Restarting it would probably hit the same wall, so nothing is proposed. Compare its memory use with its limits, then raise the limit or find out why it needs that much.")
	case "crashed":
		t.Headline = p.pick(u+" crashed.", u+" stopped unexpectedly.")
		if f.Has("result") {
			t.Why = "systemd recorded the result " + f.V("result") + "."
		}
		if f.Has("ran_for") {
			t.Why = joinSentences(t.Why, "It had been running for "+f.V("ran_for")+" and has not crashed like this before.")
		}
		t.Next = "A restart is proposed to bring it back, but the crash itself still needs a look."
	case "crash_loop":
		t.Headline = p.pick(u+" keeps crashing.", u+" crashes every time it runs.")
		var why []string
		if f.Has("crashes") {
			why = append(why, "it ended the same way ("+f.V("exit")+") "+f.V("crashes")+" times in the last "+f.V("window"))
		}
		if f.Has("restarts") {
			why = append(why, "systemd already restarted it "+f.V("restarts")+" times")
		}
		if f.V("start_limit") == "yes" {
			why = append(why, "systemd gave up restarting it after too many failures")
		}
		if f.V("at_start") == "yes" {
			why = append(why, "it crashed "+f.V("ran_for")+" after it started")
		}
		if len(why) > 0 {
			t.Why = strings.ToUpper(why[0][:1]) + why[0][1:]
			if len(why) > 1 {
				t.Why += ", and " + strings.Join(why[1:], ", and ")
			}
			t.Why += "."
		}
		t.Next = p.pick("A restart would most likely crash the same way, so none is proposed. Look at its log and core dump to find the cause, fix it, then start it again.",
			"Restarting it would probably end in the same crash, so nothing is proposed. Find the cause in its log and core dump first, then start it again.")
	case "permission":
		t.Headline = p.pick(u+" cannot use a file it needs because of the file permissions.",
			u+" failed with \"Permission denied\" from the file permissions, not from SELinux.")
		if f.Has("dac_path") {
			t.Why = f.V("dac_path") + " has mode " + f.V("dac_mode") + " and belongs to " + f.V("dac_owner") +
				", but the service runs as " + f.V("dac_user") + ", which may not " + f.V("dac_need") + " it."
		}
		t.Next = p.pick("Fix the owner or mode of that path, or the User= and Group= of the unit. Nothing is changed automatically.",
			"Change the owner or mode of that path, or the user the unit runs as. The assistant does not change this on its own.")
	default:
		t.Headline = p.pick(u+" failed, and the assistant could not find out why.", u+" failed for a reason the assistant does not recognize.")
		if f.Has("journal") {
			t.Why = "The last relevant log line is: " + f.V("journal") + "."
		}
		t.Next = "Read the log lines in the evidence below."
	}
	return t
}

func denialWhy(f *Facts, p phraser) string {
	if !f.Has("perm") {
		return ""
	}
	obj := f.V("object")
	if f.Has("target_type") {
		obj += " (labeled " + f.V("target_type") + ")"
	}
	s := "SELinux denied " + f.V("domain") + " the " + f.V("perm") + " permission on " + obj
	if n := f.V("count"); n != "" && n != "1" {
		s += ", " + n + " times"
	}
	if n := f.V("denials"); n != "" && n != "1" {
		s += "; there are " + n + " kinds of denial in all"
	}
	return s + "."
}

// --- SELinux ---------------------------------------------------------------------

func selinuxText(f *Facts, p phraser) Text {
	d, obj := f.V("domain"), f.V("object")
	var t Text
	switch f.Cause {
	case "mislabeled":
		t.Headline = p.pick("SELinux blocked "+d+" from using "+obj+" because it has the wrong label.",
			"SELinux stopped "+d+" from using "+obj+": the file carries the wrong label.")
		if f.Has("default_type") {
			t.Why = "The policy default there is " + f.V("default_type") + "; the file was probably moved or created with the wrong label."
		}
	case "missing_fcontext":
		t.Headline = p.pick("SELinux blocked "+d+" from using "+obj+" because no rule labels that place for it.",
			"SELinux stopped "+d+" from using "+obj+": nothing in the policy labels that location for this service.")
		if f.Has("want_type") {
			t.Why = "It is labeled " + f.V("target_type") + ", a type " + d + " may not use; " + f.V("want_type") + " is a type it may use."
		}
	case "port":
		t.Headline = p.pick("SELinux blocked "+d+" from using "+f.V("proto")+" port "+f.V("port")+" because the port is not labeled for it.",
			"SELinux stopped "+d+" from using "+f.V("proto")+" port "+f.V("port")+": the port does not carry a label it may use.")
		if f.Has("port_type_now") && f.V("port_type_now") != f.V("target_type") {
			t.Why = "The port is labeled " + f.V("port_type_now") + " now."
		}
	case "boolean":
		if f.Has("boolean") {
			t.Headline = p.pick("SELinux blocked "+d+" because the boolean "+f.V("boolean")+" that allows this is off.",
				"SELinux stopped "+d+": the switch that allows this, the boolean "+f.V("boolean")+", is off.")
		} else {
			t.Headline = "SELinux blocked " + d + " because a boolean that would allow this is off."
		}
	case "suspicious":
		t.Headline = "SELinux stopped " + d + " from touching " + obj + ", which is security sensitive. This looks like the policy doing its job."
		t.Next = p.pick("Do not loosen the policy. Find out why the program tried this.",
			"Leave the policy as it is and find out why the program tried this.")
	default:
		t.Headline = p.pick("SELinux blocked "+d+" from using "+obj+", and there is no known safe fix.",
			"SELinux denied "+d+" access to "+obj+", and no known fix applies.")
		t.Next = p.pick("Please have someone review it before changing the policy. The assistant never writes policy modules on its own.",
			"It needs a person to review it. The assistant never generates policy modules by itself.")
	}
	if t.Why == "" {
		t.Why = denialWhy(f, p)
	} else {
		t.Why = joinSentences(denialWhy(f, p), t.Why)
	}
	if f.Review && f.Cause != "suspicious" && f.Cause != "unknown" && t.Next == "" {
		t.Next = "The assistant is not confident enough to propose a change, so it needs a person to review it."
	}
	return t
}

// --- disk --------------------------------------------------------------------------

func diskText(f *Facts, p phraser) Text {
	var t Text
	pct := f.V("used_pct")
	if f.OK {
		t.Headline = "The root file system is " + pct + " % full."
		t.Next = p.pick("Nothing to do.", "That is fine; nothing to do.")
		return t
	}
	if f.V("level") == "critical" {
		t.Headline = p.pick("The root file system is "+pct+" % full, which is critical.",
			"The root file system is critically full: "+pct+" % used.")
	} else {
		t.Headline = p.pick("The root file system is "+pct+" % full.", "The root file system is getting full: "+pct+" % used.")
	}
	switch f.Cause {
	case "snapshots":
		if f.Has("snapshot") {
			t.Why = "Snapshot " + f.V("snapshot") + " (" + f.V("snapshot_date") + ", \"" + f.V("snapshot_desc") + "\") alone holds " + f.V("snapshot_size") + "."
		} else {
			t.Why = "Snapshots hold " + orWord(f.V("snapshots_size"), "most of the space") + "."
		}
	case "journal":
		t.Why = "The system journal uses " + f.V("journal_size") + "."
	case "package_cache":
		t.Why = "Cached packages use " + f.V("cache_size") + "."
	default:
		t.Why = p.pick("Most of the space is data, not snapshots, logs or cached packages.",
			"The space is used by data, not by snapshots, logs or the package cache.")
		t.Next = "Nothing is deleted automatically. Look for large files you no longer need."
		if f.Has("confined") {
			t.Next += " Run the disk report as root to measure the space each snapshot holds."
		}
	}
	if f.Has("days_to_full") {
		if d := f.V("days_to_full"); d == "1" {
			t.Why = joinSentences(t.Why, "At the current rate it will be full within about a day.")
		} else {
			t.Why = joinSentences(t.Why, "At the current rate it will be full in about "+d+" days.")
		}
	}
	if f.Has("nothing_large") {
		t.Next = joinSentences(t.Next, "No single item is large enough to free on its own.")
	}
	return t
}

// --- package transactions ------------------------------------------------------------

func dnfText(f *Facts, p phraser) Text {
	var t Text
	cmd := f.V("command")
	if f.OK {
		t.Headline = "No failed package transaction was found."
		return t
	}
	what := "A package transaction"
	if cmd != "" {
		what = "The package transaction \"" + cmd + "\""
	}
	switch f.Cause {
	case "rollback":
		t.Headline = p.pick(what+" failed after it had already changed packages.",
			what+" failed halfway: packages were already changed.")
		if f.Has("snapshot") {
			parts := nonZero([][3]string{{f.V("added"), "added 1 package", "added %s packages"},
				{f.V("removed"), "removed 1 package", "removed %s packages"}, {f.V("changed"), "changed 1 package", "changed %s packages"}})
			if len(parts) > 0 {
				t.Why = "Since snapshot " + f.V("snapshot") + ", taken just before it, the transaction " + list(parts) + "."
			} else {
				t.Why = "Snapshot " + f.V("snapshot") + " was taken just before it."
			}
		}
		if f.Has("failed") {
			t.Why = joinSentences(t.Why, "The package that failed is "+f.V("failed")+".")
		}
		if f.Hint {
			t.Next = "The background service cannot compare the packages inside snapshots, so confirm it as root first: the assistant then shows exactly what a rollback changes."
		}
	default:
		t.Headline = p.pick(what+" failed, but no package was changed.", what+" failed before it changed any package.")
		if f.Has("failed") {
			t.Why = "The package that failed is " + f.V("failed") + "."
		}
		t.Next = p.pick("Nothing needs undoing. Read the log lines, fix the cause and try again.",
			"There is nothing to undo. Check the log lines, fix the cause and run it again.")
	}
	return t
}

// --- rollback ----------------------------------------------------------------------

func snapshotText(f *Facts, p phraser) Text {
	var t Text
	n := f.V("snapshot")
	t.Headline = p.pick("This returns the system to snapshot "+n+" ("+f.V("snapshot_date")+", \""+f.V("snapshot_desc")+"\").",
		"This takes the system back to snapshot "+n+" ("+f.V("snapshot_date")+", \""+f.V("snapshot_desc")+"\").")
	if f.Has("before") {
		t.Headline = "This undoes " + f.V("before") + ": it returns the system to snapshot " + n + ", taken just before " + f.V("before") + " was applied."
	}
	if f.Has("added") {
		parts := nonZero([][3]string{{f.V("added"), "brings back 1 package", "brings back %s packages"},
			{f.V("removed"), "removes 1 package", "removes %s packages"}, {f.V("changed"), "changes the version of 1 package", "changes the version of %s packages"}})
		if len(parts) > 0 {
			t.Why = "Compared with now, it " + list(parts)
		} else {
			t.Why = "Compared with now, the packages are the same"
		}
		switch f.V("files") {
		case "", "0":
			t.Why += "."
		case "1":
			t.Why += "; 1 file differs."
		default:
			t.Why += "; " + f.V("files") + " files differ."
		}
	}
	return t
}

// --- third-party drivers -----------------------------------------------------------

func driverText(f *Facts, p phraser) Text {
	var t Text
	gpu, ver := f.V("gpu"), f.V("version")
	switch f.Cause {
	case "install":
		t.Headline = p.pick(gpu+" can use the NVIDIA driver "+ver+" instead of nouveau.",
			"The NVIDIA driver "+ver+" can replace nouveau for "+gpu+".")
		switch {
		case f.V("variant") == "compute":
			t.Why = "Only the compute part is installed (CUDA, NVML, OpenCL and nvidia-smi), no display packages."
		case f.Has("primary"):
			t.Why = f.V("primary") + " stays the display GPU; programs run on " + gpu +
				" when they ask for it (PRIME offload, basalt-nvidia-run), and CUDA works on it."
		default:
			t.Why = "The NVIDIA driver then drives the display."
		}
		t.Next = "Restart the computer after applying it. The first start checks the driver and goes back to nouveau if it fails, " +
			"and the snapshot taken before the change brings the system back. The driver is proprietary: applying it means you accept " +
			"the NVIDIA Driver License Agreement (basalt drivers license nvidia)."
	default:
		t.Headline = f.V("report")
	}
	return t
}

// --- planned changes -----------------------------------------------------------------

// Effect describes what one action does, in words.
func Effect(a action.Action) string {
	p := a.Params
	switch a.Kind {
	case action.SELinuxFcontext:
		return "add a rule that labels " + p["path"] + " and everything below it as " + p["type"] + " (relabeling the files already there)"
	case action.SELinuxRestorecon:
		return "reset the SELinux label of " + p["path"] + " to the policy default"
	case action.SELinuxPort:
		return "label " + p["proto"] + " port " + p["port"] + " as " + p["type"]
	case action.SELinuxBoolean:
		return "turn the SELinux boolean " + p["name"] + " " + p["value"]
	case action.UnitRestart:
		return "restart " + p["unit"]
	case action.FileRestore:
		return "put back the copy of " + p["path"] + " from snapshot " + p["snapshot"]
	case action.SnapshotRollback:
		return "make snapshot " + p["snapshot"] + " the root from the next boot"
	case action.SnapshotDelete:
		return "delete snapshot " + p["snapshot"] + ", which frees the space only it holds"
	case action.JournalVacuum:
		return "shrink the archived journal files to " + p["size"]
	case action.DnfClean:
		return "remove the cached package files"
	case action.DriverInstall:
		return "turn on the basalt-nonfree repository, install the NVIDIA driver with the signed kernel module for kernel " +
			p["kernel"] + " and turn nouveau off from the next start"
	case action.UpdateCheck:
		return "download the newest list of packages"
	case action.UpdateInstall:
		if p["scope"] == "security" {
			return "install " + p["count"] + " security updates, exactly the versions shown"
		}
		return "install " + p["count"] + " updates, exactly the versions shown"
	case action.UpdateRollback:
		return "make snapshot " + p["snapshot"] + ", taken just before the update " + p["proposal"] + ", the root from the next boot"
	case action.RepoEnable:
		return "turn the " + p["repo"] + " channel on"
	case action.RepoDisable:
		return "turn the " + p["repo"] + " channel off"
	case action.SourceAdd:
		return "add the software source " + p["name"] + " (" + p["url"] + ") and trust its signing key " + p["fingerprint"]
	case action.SourceRemove:
		return "remove the software source " + p["id"]
	case action.KeyboardSystem:
		return "make " + p["layouts"] + " the keyboard layouts of the login screen, the text console (" + p["keymap"] + ") and new accounts"
	}
	return a.Kind
}

func changeSentence(acts []action.Action, hint bool, p phraser) string {
	var es []string
	for _, a := range acts {
		es = append(es, Effect(a))
	}
	list := es[0]
	if len(es) == 2 {
		list = es[0] + " and then " + es[1]
	} else if len(es) > 2 {
		list = strings.Join(es[:len(es)-1], ", ") + " and then " + es[len(es)-1]
	}
	if hint {
		return "Once confirmed, applying it would " + list + "."
	}
	return p.pick("If you apply it, the assistant will "+list+".", "Applying it will "+list+".")
}

func orWord(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// nonZero keeps the phrases whose count is not zero: {count, singular,
// plural with %s for the count}.
func nonZero(xs [][3]string) []string {
	var out []string
	for _, x := range xs {
		switch x[0] {
		case "", "0":
		case "1":
			out = append(out, x[1])
		default:
			out = append(out, strings.Replace(x[2], "%s", x[0], 1))
		}
	}
	return out
}

func list(xs []string) string {
	switch len(xs) {
	case 0:
		return ""
	case 1:
		return xs[0]
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}
