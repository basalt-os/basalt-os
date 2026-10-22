package explain

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/i18n"
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
// source (what people see), in the language of tr, or any of them with one
// (training data for the optional model, see basalt-eval render-data,
// always English). A wording is a format: its verbs take the values. The
// first wording is a catalog message (marked with i18n.M); the others are
// English only.
type phraser struct {
	r  *rand.Rand
	tr i18n.Tr
}

func (p phraser) pick(alts ...string) string {
	if p.r == nil || len(alts) == 1 {
		return p.tr.Marked(alts[0])
	}
	return alts[p.r.Intn(len(alts))]
}

// Write renders the facts and the planned changes as text, with the
// default wording, in English (the reference text: logs, the gate, the
// humanize layer's template).
func Write(f *Facts, changes []action.Action) Text { return write(f, changes, phraser{}) }

// WriteIn is Write in the language of tr (what a person reads).
func WriteIn(tr i18n.Tr, f *Facts, changes []action.Action) Text {
	return write(f, changes, phraser{tr: tr})
}

// WriteVariant is Write with wording picked by r (training data).
func WriteVariant(f *Facts, changes []action.Action, r *rand.Rand) Text {
	return write(f, changes, phraser{r: r})
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
		t.Next = joinSentences(fmt.Sprintf(p.tr.T("The diagnosis is incomplete (%s), so nothing is proposed."), strings.TrimRight(f.V("error"), ".")),
			p.pick(i18n.M("Run it again in a moment."), "Please run it again in a moment."))
	} else if f.Review && f.Has("confidence") && len(changes) > 0 {
		t.Next = joinSentences(t.Next, fmt.Sprintf(p.tr.T("The assistant is not sure about this one (confidence %s, threshold %s), so please read the evidence before applying."),
			f.V("confidence"), f.V("threshold")))
	}
	return t
}

// upperFirst capitalizes the first letter (of any script).
func upperFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}

// count reads a count of the facts (0 when absent or not a number).
func count(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// duration translates the durations of internal/diag ("24 hours", "3
// days"); "5 s", "12 min" and "3 h" read the same in every language we
// ship.
func duration(tr i18n.Tr, s string) string {
	if s == "24 hours" {
		return tr.T("24 hours")
	}
	if n, ok := strings.CutSuffix(s, " days"); ok {
		return fmt.Sprintf(tr.T("%s days"), n)
	}
	return s
}

// --- units -----------------------------------------------------------------------

func unitText(f *Facts, p phraser) Text {
	u := f.Subject
	var t Text
	if f.OK {
		t.Headline = fmt.Sprintf(p.pick(i18n.M("%s is running normally."), "%s is up and running.", "%s is running fine."), u)
		t.Next = p.pick(i18n.M("Nothing to do."), "There is nothing to fix.")
		return t
	}
	switch f.Cause {
	case "not_found":
		t.Headline = fmt.Sprintf(p.pick(i18n.M("There is no unit called %s on this system."), "This system has no unit named %s."), u)
		t.Next = p.tr.T("Check the name; systemctl list-units shows the units that exist.")
	case "config_error":
		where := p.tr.T("its configuration")
		if f.Has("file") {
			where = f.V("file")
			if f.Has("line") {
				where = fmt.Sprintf(p.tr.T("%s (line %s)"), where, f.V("line"))
			}
		}
		t.Headline = fmt.Sprintf(p.pick(i18n.M("%s is not running because of a mistake in %s."),
			"%s stopped: there is an error in %s.",
			"%s cannot start because %s has an error."), u, where)
		if f.Has("checker") && f.Has("checker_msg") {
			t.Why = fmt.Sprintf(p.pick(i18n.M("The configuration check %s says: %s."), "%s reports: %s."), f.V("checker"), f.V("checker_msg"))
		} else if f.Has("journal") {
			t.Why = fmt.Sprintf(p.pick(i18n.M("The service log says: %s."), "Its log shows: %s."), f.V("journal"))
		}
		switch {
		case f.Hint:
			t.Next = p.tr.T("The background service cannot look inside snapshots, so confirm it as root: the assistant then tests the snapshot copy with the configuration check before proposing it.")
		case f.Has("snapshot"):
			// The change sentence says what is restored.
		default:
			t.Next = fmt.Sprintf(p.pick(i18n.M("No snapshot has a working copy of the file, so fix it by hand and then restart %s."),
				"No snapshot holds a good copy, so please correct the file by hand and restart %s."), u)
		}
	case "selinux_denial":
		t.Headline = fmt.Sprintf(p.pick(i18n.M("%s is not running because SELinux blocked something it needs."),
			"%s stopped because SELinux denied it access to something it needs."), u)
		t.Why = denialWhy(f, p)
		if f.Review {
			t.Next = p.pick(i18n.M("There is no safe automatic fix for at least one denial, so it needs a person to review it."),
				"At least one denial has no safe automatic fix; please have someone review it.")
		}
	case "port_conflict":
		t.Headline = fmt.Sprintf(p.pick(i18n.M("%s cannot start because another program is already using its port."),
			"%s did not start: its port is taken by another program."), u)
		if f.Has("port") && f.Has("port_owner") {
			t.Why = fmt.Sprintf(p.tr.T("Port %s is held by %s."), f.V("port"), f.V("port_owner"))
		} else if f.Has("port") {
			t.Why = fmt.Sprintf(p.tr.T("Port %s is already in use."), f.V("port"))
		}
		t.Next = fmt.Sprintf(p.pick(i18n.M("Stop the other program or give %s a different port. Nothing is changed automatically."),
			"Either stop the other program or move %s to another port. The assistant does not change this on its own."), u)
	case "dependency_failed":
		t.Headline = fmt.Sprintf(p.pick(i18n.M("%s did not start because a unit it depends on failed."),
			"%s could not start: something it needs failed first."), u)
		if f.Has("dep") {
			dep := f.V("dep")
			switch st := orWord(f.V("dep_state"), "failed"); st {
			case "failed":
				t.Why = fmt.Sprintf(p.tr.T("%s has failed."), dep)
			case "inactive":
				t.Why = fmt.Sprintf(p.tr.T("%s is inactive."), dep)
			case "activating":
				t.Why = fmt.Sprintf(p.tr.T("%s is activating."), dep)
			case "deactivating":
				t.Why = fmt.Sprintf(p.tr.T("%s is deactivating."), dep)
			default:
				t.Why = fmt.Sprintf(p.tr.T("%s is %s."), dep, st)
			}
			t.Next = fmt.Sprintf(p.pick(i18n.M("Fix %s first; %s can start once it runs."), "Look at %[1]s first."), dep, u)
		}
	case "disk_full":
		t.Headline = fmt.Sprintf(p.pick(i18n.M("%s failed while the disk was full."), "%s stopped because it ran out of disk space."), u)
		if f.Has("fs_path") {
			t.Why = fmt.Sprintf(p.tr.T("%s is %s %% full."), f.V("fs_path"), f.V("fs_pct"))
		}
		t.Next = p.tr.T("Free some space first; the disk report shows what is using it.")
	case "missing_file":
		t.Headline = fmt.Sprintf(p.pick(i18n.M("%s failed because a file it needs does not exist."), "%s stopped: a file it reads is missing."), u)
		if f.Has("journal") {
			t.Why = fmt.Sprintf(p.tr.T("Its log says: %s."), f.V("journal"))
		}
		t.Next = fmt.Sprintf(p.pick(i18n.M("Create or restore the missing file, then start %s again."),
			"Put the missing file back (or fix the setting that names it), then start %s again."), u)
	case "oom":
		t.Headline = fmt.Sprintf(p.pick(i18n.M("%s ran out of memory and was stopped by the kernel."), "%s used too much memory and was killed."), u)
		switch {
		case f.Has("memory_max"):
			t.Why = fmt.Sprintf(p.tr.T("It reached its memory limit (MemoryMax=%s)."), f.V("memory_max"))
		case f.V("oom_scope") == "system":
			t.Why = p.tr.T("The whole system ran out of memory at the time.")
		case f.V("oom_scope") == "cgroup":
			t.Why = p.tr.T("It hit a memory limit of its control group or slice.")
		}
		t.Next = p.pick(i18n.M("A plain restart would most likely end the same way, so none is proposed. Check how much memory it uses and its limits; raise the limit if it is too low, otherwise find out why it needs so much."),
			"Restarting it would probably hit the same wall, so nothing is proposed. Compare its memory use with its limits, then raise the limit or find out why it needs that much.")
	case "crashed":
		t.Headline = fmt.Sprintf(p.pick(i18n.M("%s crashed."), "%s stopped unexpectedly."), u)
		if f.Has("result") {
			t.Why = fmt.Sprintf(p.tr.T("systemd recorded the result %s."), f.V("result"))
		}
		if f.Has("ran_for") {
			t.Why = joinSentences(t.Why, fmt.Sprintf(p.tr.T("It had been running for %s and has not crashed like this before."), duration(p.tr, f.V("ran_for"))))
		}
		t.Next = p.tr.T("A restart is proposed to bring it back, but the crash itself still needs a look.")
	case "crash_loop":
		t.Headline = fmt.Sprintf(p.pick(i18n.M("%s keeps crashing."), "%s crashes every time it runs."), u)
		var why []string
		if f.Has("crashes") {
			why = append(why, fmt.Sprintf(p.tr.T("it ended the same way (%s) %s times in the last %s"), f.V("exit"), f.V("crashes"), duration(p.tr, f.V("window"))))
		}
		if f.Has("restarts") {
			why = append(why, fmt.Sprintf(p.tr.T("systemd already restarted it %s times"), f.V("restarts")))
		}
		if f.V("start_limit") == "yes" {
			why = append(why, p.tr.T("systemd gave up restarting it after too many failures"))
		}
		if f.V("at_start") == "yes" {
			why = append(why, fmt.Sprintf(p.tr.T("it crashed %s after it started"), duration(p.tr, f.V("ran_for"))))
		}
		if len(why) > 0 {
			s := why[0]
			for _, w := range why[1:] {
				s = fmt.Sprintf(p.tr.T("%s, and %s"), s, w)
			}
			t.Why = upperFirst(s) + "."
		}
		t.Next = p.pick(i18n.M("A restart would most likely crash the same way, so none is proposed. Look at its log and core dump to find the cause, fix it, then start it again."),
			"Restarting it would probably end in the same crash, so nothing is proposed. Find the cause in its log and core dump first, then start it again.")
	case "permission":
		t.Headline = fmt.Sprintf(p.pick(i18n.M("%s cannot use a file it needs because of the file permissions."),
			"%s failed with \"Permission denied\" from the file permissions, not from SELinux."), u)
		if f.Has("dac_path") {
			args := []any{f.V("dac_path"), f.V("dac_mode"), f.V("dac_owner"), f.V("dac_user")}
			switch f.V("dac_need") {
			case "read":
				t.Why = fmt.Sprintf(p.tr.T("%s has mode %s and belongs to %s, but the service runs as %s, which may not read it."), args...)
			case "write":
				t.Why = fmt.Sprintf(p.tr.T("%s has mode %s and belongs to %s, but the service runs as %s, which may not write it."), args...)
			case "search":
				t.Why = fmt.Sprintf(p.tr.T("%s has mode %s and belongs to %s, but the service runs as %s, which may not search it."), args...)
			case "create files in":
				t.Why = fmt.Sprintf(p.tr.T("%s has mode %s and belongs to %s, but the service runs as %s, which may not create files in it."), args...)
			default:
				t.Why = fmt.Sprintf(p.tr.T("%s has mode %s and belongs to %s, but the service runs as %s, which may not %s it."), append(args, f.V("dac_need"))...)
			}
		}
		t.Next = p.pick(i18n.M("Fix the owner or mode of that path, or the User= and Group= of the unit. Nothing is changed automatically."),
			"Change the owner or mode of that path, or the user the unit runs as. The assistant does not change this on its own.")
	default:
		t.Headline = fmt.Sprintf(p.pick(i18n.M("%s failed, and the assistant could not find out why."), "%s failed for a reason the assistant does not recognize."), u)
		if f.Has("journal") {
			t.Why = fmt.Sprintf(p.tr.T("The last relevant log line is: %s."), f.V("journal"))
		}
		t.Next = p.tr.T("Read the log lines in the evidence below.")
	}
	return t
}

// object is what a denial was about: a path, or a port ("tcp port 8080")
// in the reader's words.
func object(f *Facts, p phraser) string {
	o := f.V("object")
	if proto, port, ok := strings.Cut(o, " port "); ok && !strings.Contains(proto, "/") && count(port) > 0 {
		return fmt.Sprintf(p.tr.T("%s port %s"), proto, port)
	}
	return o
}

func denialWhy(f *Facts, p phraser) string {
	if !f.Has("perm") {
		return ""
	}
	obj := object(f, p)
	if f.Has("target_type") {
		obj = fmt.Sprintf(p.tr.T("%s (labeled %s)"), obj, f.V("target_type"))
	}
	s := fmt.Sprintf(p.tr.T("SELinux denied %s the %s permission on %s"), f.V("domain"), f.V("perm"), obj)
	if n := f.V("count"); n != "" && n != "1" {
		s = fmt.Sprintf(p.tr.T("%s, %s times"), s, n)
	}
	if n := f.V("denials"); n != "" && n != "1" {
		s = fmt.Sprintf(p.tr.T("%s; there are %s kinds of denial in all"), s, n)
	}
	return s + "."
}

// --- SELinux ---------------------------------------------------------------------

func selinuxText(f *Facts, p phraser) Text {
	d, obj := f.V("domain"), object(f, p)
	var t Text
	switch f.Cause {
	case "mislabeled":
		t.Headline = fmt.Sprintf(p.pick(i18n.M("SELinux blocked %s from using %s because it has the wrong label."),
			"SELinux stopped %s from using %s: the file carries the wrong label."), d, obj)
		if f.Has("default_type") {
			t.Why = fmt.Sprintf(p.tr.T("The policy default there is %s; the file was probably moved or created with the wrong label."), f.V("default_type"))
		}
	case "missing_fcontext":
		t.Headline = fmt.Sprintf(p.pick(i18n.M("SELinux blocked %s from using %s because no rule labels that place for it."),
			"SELinux stopped %s from using %s: nothing in the policy labels that location for this service."), d, obj)
		if f.Has("want_type") {
			t.Why = fmt.Sprintf(p.tr.T("It is labeled %s, a type %s may not use; %s is a type it may use."), f.V("target_type"), d, f.V("want_type"))
		}
	case "port":
		t.Headline = fmt.Sprintf(p.pick(i18n.M("SELinux blocked %s from using %s port %s because the port is not labeled for it."),
			"SELinux stopped %s from using %s port %s: the port does not carry a label it may use."), d, f.V("proto"), f.V("port"))
		if f.Has("port_type_now") && f.V("port_type_now") != f.V("target_type") {
			t.Why = fmt.Sprintf(p.tr.T("The port is labeled %s now."), f.V("port_type_now"))
		}
	case "boolean":
		if f.Has("boolean") {
			t.Headline = fmt.Sprintf(p.pick(i18n.M("SELinux blocked %s because the boolean %s that allows this is off."),
				"SELinux stopped %s: the switch that allows this, the boolean %s, is off."), d, f.V("boolean"))
		} else {
			t.Headline = fmt.Sprintf(p.tr.T("SELinux blocked %s because a boolean that would allow this is off."), d)
		}
	case "suspicious":
		t.Headline = fmt.Sprintf(p.tr.T("SELinux stopped %s from touching %s, which is security sensitive. This looks like the policy doing its job."), d, obj)
		t.Next = p.pick(i18n.M("Do not loosen the policy. Find out why the program tried this."),
			"Leave the policy as it is and find out why the program tried this.")
	default:
		// Plain language first: what happened and whether the person must
		// act; the technical sentence follows, the raw denial is evidence.
		t.Headline = p.tr.T("The security rules (SELinux) stopped a program from doing something they do not allow.") + " " +
			p.tr.T("Nothing needs your action now: this is a note for whoever looks after this computer.") + " " +
			fmt.Sprintf(p.pick(i18n.M("SELinux blocked %s from using %s, and there is no known safe fix."),
				"SELinux denied %s access to %s, and no known fix applies."), d, obj)
		t.Next = p.pick(i18n.M("Please have someone review it before changing the policy. The assistant never writes policy modules on its own."),
			"It needs a person to review it. The assistant never generates policy modules by itself.")
	}
	if t.Why == "" {
		t.Why = denialWhy(f, p)
	} else {
		t.Why = joinSentences(denialWhy(f, p), t.Why)
	}
	if f.Review && f.Cause != "suspicious" && f.Cause != "unknown" && t.Next == "" {
		t.Next = p.tr.T("The assistant is not confident enough to propose a change, so it needs a person to review it.")
	}
	return t
}

// --- disk --------------------------------------------------------------------------

func diskText(f *Facts, p phraser) Text {
	var t Text
	pct := f.V("used_pct")
	if f.OK {
		t.Headline = fmt.Sprintf(p.tr.T("The root file system is %s %% full."), pct)
		t.Next = p.pick(i18n.M("Nothing to do."), "That is fine; nothing to do.")
		return t
	}
	if f.V("level") == "critical" {
		t.Headline = fmt.Sprintf(p.pick(i18n.M("The root file system is %s %% full, which is critical."),
			"The root file system is critically full: %s %% used."), pct)
	} else {
		t.Headline = fmt.Sprintf(p.pick(i18n.M("The root file system is %s %% full."), "The root file system is getting full: %s %% used."), pct)
	}
	switch f.Cause {
	case "snapshots":
		if f.Has("snapshot") {
			t.Why = fmt.Sprintf(p.tr.T("Snapshot %s (%s, \"%s\") alone holds %s."), f.V("snapshot"), f.V("snapshot_date"), f.V("snapshot_desc"), f.V("snapshot_size"))
		} else {
			t.Why = fmt.Sprintf(p.tr.T("Snapshots hold %s."), orWord(f.V("snapshots_size"), p.tr.T("most of the space")))
		}
	case "journal":
		t.Why = fmt.Sprintf(p.tr.T("The system journal uses %s."), f.V("journal_size"))
	case "package_cache":
		t.Why = fmt.Sprintf(p.tr.T("Cached packages use %s."), f.V("cache_size"))
	default:
		t.Why = p.pick(i18n.M("Most of the space is data, not snapshots, logs or cached packages."),
			"The space is used by data, not by snapshots, logs or the package cache.")
		t.Next = p.tr.T("Nothing is deleted automatically. Look for large files you no longer need.")
		if f.Has("confined") {
			t.Next += " " + p.tr.T("Run the disk report as root to measure the space each snapshot holds.")
		}
	}
	if f.Has("days_to_full") {
		if d := f.V("days_to_full"); d == "1" {
			t.Why = joinSentences(t.Why, p.tr.T("At the current rate it will be full within about a day."))
		} else {
			t.Why = joinSentences(t.Why, fmt.Sprintf(p.tr.T("At the current rate it will be full in about %s days."), d))
		}
	}
	if f.Has("nothing_large") {
		t.Next = joinSentences(t.Next, p.tr.T("No single item is large enough to free on its own."))
	}
	return t
}

// --- package transactions ------------------------------------------------------------

func dnfText(f *Facts, p phraser) Text {
	var t Text
	cmd := f.V("command")
	if f.OK {
		t.Headline = p.tr.T("No failed package transaction was found.")
		return t
	}
	what := p.tr.T("A package transaction")
	if cmd != "" {
		what = fmt.Sprintf(p.tr.T("The package transaction \"%s\""), cmd)
	}
	switch f.Cause {
	case "rollback":
		t.Headline = fmt.Sprintf(p.pick(i18n.M("%s failed after it had already changed packages."),
			"%s failed halfway: packages were already changed."), what)
		if f.Has("snapshot") {
			var parts []string
			if n := count(f.V("added")); n > 0 {
				parts = append(parts, fmt.Sprintf(p.tr.N("added %d package", "added %d packages", n), n))
			}
			if n := count(f.V("removed")); n > 0 {
				parts = append(parts, fmt.Sprintf(p.tr.N("removed %d package", "removed %d packages", n), n))
			}
			if n := count(f.V("changed")); n > 0 {
				parts = append(parts, fmt.Sprintf(p.tr.N("changed %d package", "changed %d packages", n), n))
			}
			if len(parts) > 0 {
				t.Why = fmt.Sprintf(p.tr.T("Since snapshot %s, taken just before it, the transaction %s."), f.V("snapshot"), list(p.tr, parts))
			} else {
				t.Why = fmt.Sprintf(p.tr.T("Snapshot %s was taken just before it."), f.V("snapshot"))
			}
		}
		if f.Has("failed") {
			t.Why = joinSentences(t.Why, fmt.Sprintf(p.tr.T("The package that failed is %s."), f.V("failed")))
		}
		if f.Hint {
			t.Next = p.tr.T("The background service cannot compare the packages inside snapshots, so confirm it as root first: the assistant then shows exactly what a rollback changes.")
		}
	default:
		t.Headline = fmt.Sprintf(p.pick(i18n.M("%s failed, but no package was changed."), "%s failed before it changed any package."), what)
		if f.Has("failed") {
			t.Why = fmt.Sprintf(p.tr.T("The package that failed is %s."), f.V("failed"))
		}
		t.Next = p.pick(i18n.M("Nothing needs undoing. Read the log lines, fix the cause and try again."),
			"There is nothing to undo. Check the log lines, fix the cause and run it again.")
	}
	return t
}

// --- rollback ----------------------------------------------------------------------

func snapshotText(f *Facts, p phraser) Text {
	var t Text
	n := f.V("snapshot")
	t.Headline = fmt.Sprintf(p.pick(i18n.M("This returns the system to snapshot %s (%s, \"%s\")."),
		"This takes the system back to snapshot %s (%s, \"%s\")."), n, f.V("snapshot_date"), f.V("snapshot_desc"))
	if f.Has("before") {
		t.Headline = fmt.Sprintf(p.tr.T("This undoes %s: it returns the system to snapshot %s, taken just before %s was applied."), f.V("before"), n, f.V("before"))
	}
	if f.Has("added") {
		var parts []string
		if c := count(f.V("added")); c > 0 {
			parts = append(parts, fmt.Sprintf(p.tr.N("brings back %d package", "brings back %d packages", c), c))
		}
		if c := count(f.V("removed")); c > 0 {
			parts = append(parts, fmt.Sprintf(p.tr.N("removes %d package", "removes %d packages", c), c))
		}
		if c := count(f.V("changed")); c > 0 {
			parts = append(parts, fmt.Sprintf(p.tr.N("changes the version of %d package", "changes the version of %d packages", c), c))
		}
		if len(parts) > 0 {
			t.Why = fmt.Sprintf(p.tr.T("Compared with now, it %s"), list(p.tr, parts))
		} else {
			t.Why = p.tr.T("Compared with now, the packages are the same")
		}
		if c := count(f.V("files")); c > 0 {
			t.Why = fmt.Sprintf(p.tr.T("%s; %s."), t.Why, fmt.Sprintf(p.tr.N("%d file differs", "%d files differ", c), c))
		} else {
			t.Why += "."
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
		t.Headline = fmt.Sprintf(p.pick(i18n.M("%s can use the NVIDIA driver %s instead of nouveau."),
			"The NVIDIA driver %[2]s can replace nouveau for %[1]s."), gpu, ver)
		switch {
		case f.V("variant") == "compute":
			t.Why = p.tr.T("Only the compute part is installed (CUDA, NVML, OpenCL and nvidia-smi), no display packages.")
		case f.Has("primary"):
			t.Why = fmt.Sprintf(p.tr.T("%s stays the display GPU; programs run on %s when they ask for it (PRIME offload, basalt-nvidia-run), and CUDA works on it."), f.V("primary"), gpu)
		default:
			t.Why = p.tr.T("The NVIDIA driver then drives the display.")
		}
		t.Next = p.tr.T("Restart the computer after applying it. The first start checks the driver and goes back to nouveau if it fails, and the snapshot taken before the change brings the system back. The driver is proprietary: applying it means you accept the NVIDIA Driver License Agreement (basalt drivers license nvidia).")
	default:
		t.Headline = f.V("report")
	}
	return t
}

// --- planned changes -----------------------------------------------------------------

// Effect describes what one action does, in words (English).
func Effect(a action.Action) string { return EffectIn(i18n.Tr{}, a) }

// EffectIn is Effect in the language of tr.
func EffectIn(tr i18n.Tr, a action.Action) string {
	p := a.Params
	switch a.Kind {
	case action.SELinuxFcontext:
		return fmt.Sprintf(tr.T("add a rule that labels %s and everything below it as %s (relabeling the files already there)"), p["path"], p["type"])
	case action.SELinuxRestorecon:
		return fmt.Sprintf(tr.T("reset the SELinux label of %s to the policy default"), p["path"])
	case action.SELinuxPort:
		return fmt.Sprintf(tr.T("label %s port %s as %s"), p["proto"], p["port"], p["type"])
	case action.SELinuxBoolean:
		if p["value"] == "on" {
			return fmt.Sprintf(tr.T("turn the SELinux boolean %s on"), p["name"])
		}
		if p["value"] == "off" {
			return fmt.Sprintf(tr.T("turn the SELinux boolean %s off"), p["name"])
		}
		return fmt.Sprintf(tr.T("turn the SELinux boolean %s %s"), p["name"], p["value"])
	case action.UnitRestart:
		return fmt.Sprintf(tr.T("restart %s"), p["unit"])
	case action.FileRestore:
		return fmt.Sprintf(tr.T("put back the copy of %s from snapshot %s"), p["path"], p["snapshot"])
	case action.SnapshotRollback:
		return fmt.Sprintf(tr.T("make snapshot %s the root from the next boot"), p["snapshot"])
	case action.SnapshotDelete:
		return fmt.Sprintf(tr.T("delete snapshot %s, which frees the space only it holds"), p["snapshot"])
	case action.JournalVacuum:
		return fmt.Sprintf(tr.T("shrink the archived journal files to %s"), p["size"])
	case action.DnfClean:
		return tr.T("remove the cached package files")
	case action.DriverInstall:
		return fmt.Sprintf(tr.T("turn on the basalt-nonfree repository, install the NVIDIA driver with the signed kernel module for kernel %s and turn nouveau off from the next start"), p["kernel"])
	case action.UpdateCheck:
		return tr.T("download the newest list of packages")
	case action.UpdateInstall:
		if p["scope"] == "security" {
			return fmt.Sprintf(tr.T("install %s security updates, exactly the versions shown"), p["count"])
		}
		return fmt.Sprintf(tr.T("install %s updates, exactly the versions shown"), p["count"])
	case action.UpdateRollback:
		return fmt.Sprintf(tr.T("make snapshot %s, taken just before the update %s, the root from the next boot"), p["snapshot"], p["proposal"])
	case action.RepoEnable:
		return fmt.Sprintf(tr.T("turn the %s channel on"), p["repo"])
	case action.RepoDisable:
		return fmt.Sprintf(tr.T("turn the %s channel off"), p["repo"])
	case action.SourceAdd:
		return fmt.Sprintf(tr.T("add the software source %s (%s) and trust its signing key %s"), p["name"], p["url"], p["fingerprint"])
	case action.SourceRemove:
		return fmt.Sprintf(tr.T("remove the software source %s"), p["id"])
	case action.KeyboardSystem:
		return fmt.Sprintf(tr.T("make %s the keyboard layouts of the login screen, the text console (%s) and new accounts"), p["layouts"], p["keymap"])
	case action.AuditRun:
		if p["install"] == "yes" {
			return tr.T("install the AI audit suite from the Basalt repositories and run it on this computer")
		}
		return tr.T("run the AI audit suite on this computer")
	case action.RiskAccept:
		return fmt.Sprintf(tr.T("record that %s accepts the risk of %s on this computer"), p["by"], p["item"])
	case action.RiskReview:
		return fmt.Sprintf(tr.T("show %s as a warning again"), p["item"])
	}
	return a.Kind
}

func changeSentence(acts []action.Action, hint bool, p phraser) string {
	var es []string
	for _, a := range acts {
		es = append(es, EffectIn(p.tr, a))
	}
	list := es[0]
	if len(es) > 1 {
		list = fmt.Sprintf(p.tr.T("%s and then %s"), strings.Join(es[:len(es)-1], ", "), es[len(es)-1])
	}
	if hint {
		return fmt.Sprintf(p.tr.T("Once confirmed, applying it would %s."), list)
	}
	return fmt.Sprintf(p.pick(i18n.M("If you apply it, the assistant will %s."), "Applying it will %s."), list)
}

func orWord(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// list joins phrases: "a", "a and b", "a, b and c".
func list(tr i18n.Tr, xs []string) string {
	switch len(xs) {
	case 0:
		return ""
	case 1:
		return xs[0]
	}
	return fmt.Sprintf(tr.T("%s and %s"), strings.Join(xs[:len(xs)-1], ", "), xs[len(xs)-1])
}
