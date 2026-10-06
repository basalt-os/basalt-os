package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/allowlist"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/audit"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/gateclient"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/ledger"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/profile"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/selinux"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/session"
)

// Persistent allowlist changes are proposals: basalt-agent shows exactly
// what changes and where, asks, and records the change. A user-scope
// change (your own profile override) needs only your confirmation; a
// system-scope change (/etc/basalt-agent/profiles, every user) needs
// administrator authentication through polkit. Agents cannot make either:
// SELinux keeps them out of your configuration, and no_new_privs keeps
// them away from pkexec.

const systemProfiles = "/etc/basalt-agent/profiles"

func cmdEgress(args []string) error {
	if len(args) == 1 {
		return showEgress(args[0])
	}
	if len(args) < 4 || args[0] != "propose" || (args[2] != "add" && args[2] != "remove") {
		return errors.New("usage: basalt-agent egress PROFILE | basalt-agent egress propose PROFILE add|remove ENTRY [--system] [--yes]")
	}
	name, op, raw := args[1], args[2], args[3]
	system, yes := false, false
	for _, a := range args[4:] {
		switch a {
		case "--system":
			system = true
		case "--yes", "-y":
			yes = true
		default:
			return fmt.Errorf("unknown option %q", a)
		}
	}
	if inAgentDomain() {
		return errors.New("refused: an agent session cannot change allowlists; a person runs this outside the session")
	}
	e, err := allowlist.ParseEntry(raw)
	if err != nil {
		return err
	}
	d, err := session.UserDirs()
	if err != nil {
		return err
	}
	paths := sharePaths(d)
	pr, err := paths.Load(name)
	if err != nil {
		return err
	}
	target := filepath.Join(d.Config, "profiles", name+".conf")
	scope := "user (your sessions only)"
	if system {
		target = filepath.Join(systemProfiles, name+".conf")
		scope = "system (every user; administrator authentication)"
	}
	after, err := changedEgress(paths, pr, op, e)
	if err != nil {
		return err
	}
	fmt.Printf("Proposal: %s %s to the network allowlist of profile %s\n", map[string]string{"add": "add", "remove": "remove"}[op], e.String(), name)
	fmt.Printf("Scope:    %s\nFile:     %s (based on %s)\n", scope, target, pr.Source)
	fmt.Println("Allowlist after the change:")
	before := map[string]bool{}
	for _, x := range pr.Egress {
		before[x.String()] = true
	}
	now := map[string]bool{}
	for _, x := range after {
		now[x.String()] = true
		mark := " "
		if !before[x.String()] {
			mark = "+"
		}
		fmt.Printf("  %s %s\n", mark, x.String())
	}
	for _, x := range pr.Egress {
		if !now[x.String()] {
			fmt.Printf("  - %s\n", x.String())
		}
	}
	fmt.Println("Running sessions keep their list; new sessions use the new one (basalt-agent grant widens a running session).")
	action := "agent.egress.change"
	if system {
		action = "agent.egress.system"
	}
	calls := []gateclient.Call{{Action: action, Args: map[string]any{"profile": name, "op": op, "entry": e.String()}}}
	g := dialGate()
	defer g.close()
	gateID := ""
	switch {
	case g.enforced():
		// The approval gate decides (the person approves here with their
		// password, or in the queue; --yes cannot approve for them).
		var err error
		if gateID, err = g.decide(calls, isTTY(0)); err != nil {
			return fmt.Errorf("not applied: %w", err)
		}
	case !yes:
		if !isTTY(0) {
			return errors.New("not confirmed (no terminal; pass --yes after reviewing the proposal)")
		}
		fmt.Print("Apply this change? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			g.observe(calls, "declined", "the person at the terminal")
			return errors.New("not applied")
		}
	}
	if !g.enforced() {
		by := "the person at the terminal"
		if yes {
			by = "--yes at the terminal"
		}
		g.observe(calls, "approved", by)
	}
	log := audit.Open(d.AuditLog())
	lc := ledger.New()
	defer lc.Flush(3 * time.Second)
	rec := func(outcome string, extra map[string]any) {
		data := map[string]any{"op": op, "entry": e.String(), "scope": map[bool]string{true: "system", false: "user"}[system], "file": target}
		for k, v := range extra {
			data[k] = v
		}
		r, err := log.Append(audit.Record{UID: os.Getuid(), Event: "egress.change", Outcome: outcome,
			Subject: audit.Subject{Profile: name}, Data: data})
		if err == nil {
			lc.Send(r)
		}
	}
	if system {
		cmd := exec.Command("pkexec", GrantHelper, "profile", name, op, e.String())
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			rec("denied", map[string]any{"error": err.Error(), "gate_id": gateID})
			g.result(gateID, err)
			return fmt.Errorf("not applied: %w", err)
		}
		rec("ok", map[string]any{"gate_id": gateID})
		g.result(gateID, nil)
		return nil
	}
	if err := editProfile(pr.Source, target, op, e); err != nil {
		rec("error", map[string]any{"error": err.Error(), "gate_id": gateID})
		g.result(gateID, err)
		return err
	}
	rec("ok", map[string]any{"gate_id": gateID})
	g.result(gateID, nil)
	fmt.Printf("applied: %s now holds the change\n", target)
	return nil
}

func showEgress(name string) error {
	d, err := session.UserDirs()
	if err != nil {
		return err
	}
	pr, err := sharePaths(d).Load(name)
	if err != nil {
		return err
	}
	fmt.Printf("profile %s (%s), loopback %v\n", name, pr.Source, pr.Loopback)
	for _, e := range pr.Egress {
		fmt.Println("  " + e.String())
	}
	return nil
}

// changedEgress computes the effective list after the change.
func changedEgress(paths profile.Paths, pr *profile.Profile, op string, e allowlist.Entry) ([]allowlist.Entry, error) {
	cp := *pr
	switch op {
	case "add":
		cp.Allow = append(append([]allowlist.Entry(nil), pr.Allow...), e)
	case "remove":
		cp.Allow = nil
		found := false
		for _, a := range pr.Allow {
			if a.Pattern == e.Pattern {
				found = true
				continue
			}
			cp.Allow = append(cp.Allow, a)
		}
		if !found {
			return nil, fmt.Errorf("%s is not one of the profile's own entries (it may come from an include = list)", e.Pattern)
		}
	}
	if err := paths.Resolve(&cp); err != nil {
		return nil, err
	}
	return cp.Egress, nil
}

// editProfile writes target from source with the allow line added or
// removed in its [egress] section.
func editProfile(source, target, op string, e allowlist.Entry) error {
	b, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	var out []string
	section, egressEnd := "", -1
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			section = strings.TrimSpace(t[1 : len(t)-1])
		}
		if section == "egress" && op == "remove" {
			if k, v, ok := strings.Cut(t, "="); ok && strings.TrimSpace(k) == "allow" {
				if x, err := allowlist.ParseEntry(strings.TrimSpace(v)); err == nil && x.Pattern == e.Pattern {
					continue
				}
			}
		}
		out = append(out, l)
		if section == "egress" {
			egressEnd = len(out)
		}
	}
	if op == "add" {
		line := "allow = " + e.String()
		if egressEnd < 0 {
			out = append(out, "", "[egress]", line)
		} else {
			out = append(out[:egressEnd], append([]string{line}, out[egressEnd:]...)...)
		}
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	tmp := target + ".new"
	if err := os.WriteFile(tmp, []byte(strings.Join(out, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	if _, err := profile.ParseFile(tmp); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("the changed profile does not parse: %w", err)
	}
	return os.Rename(tmp, target)
}

func inAgentDomain() bool {
	cur, err := selinux.Current()
	if err != nil {
		return false
	}
	c, err := selinux.Parse(cur)
	return err == nil && strings.HasPrefix(c.Type, "basalt_agent")
}

// grantProfile is the root helper's system-scope profile change.
func grantProfile(uid int, args []string) error {
	if len(args) != 3 || !profile.ValidName(args[0]) || (args[1] != "add" && args[1] != "remove") {
		return errors.New("bad arguments")
	}
	e, err := allowlist.ParseEntry(args[2])
	if err != nil {
		return err
	}
	share := profile.Paths{ProfileDirs: []string{systemProfiles, "/usr/share/basalt-agent/profiles"},
		EgressDirs: []string{"/etc/basalt-agent/egress", "/usr/share/basalt-agent/egress"}}
	src, err := share.Find(args[0])
	if err != nil {
		return err
	}
	target := filepath.Join(systemProfiles, args[0]+".conf")
	if err := editProfile(src, target, args[1], e); err != nil {
		return err
	}
	_ = exec.Command("restorecon", target).Run()
	_ = exec.Command("logger", "-p", "authpriv.notice", "-t", "basalt-agent-grant",
		"granted: uid "+strconv.Itoa(uid)+" profile "+args[0]+" "+args[1]+" "+strconv.Quote(e.String())+" (system scope)").Run()
	fmt.Printf("applied: %s now holds the change (system scope)\n", target)
	return nil
}
