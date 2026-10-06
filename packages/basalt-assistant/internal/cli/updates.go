package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/apply"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/runner"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/sources"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/updates"
)

// updatesSys is the real system for the updates report.
func (a *app) updatesSys() updates.Sys {
	return updates.Sys{R: runner.Exec{Timeout: 2 * time.Minute}, Store: a.store, AuditPath: a.cfg.AuditPath,
		CachePath: a.cfg.StateDir + "/updates.json", BootID: bootID,
		AppsDir: "/usr/share/applications", BootTime: bootTime,
		PreSnapshots: func() map[string]int {
			m := map[string]int{}
			for _, sn := range a.env.Snapshots(context.Background()) {
				if sn.Type == "pre" && sn.Userdata["basalt"] == "apply" && sn.Userdata["proposal"] != "" {
					m[sn.Userdata["proposal"]] = sn.Number
				}
			}
			return m
		}}
}

// bootID is this boot's id.
func bootID() string {
	b, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(b))
}

// bootTime reads btime from /proc/stat.
func bootTime() time.Time {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "btime "); ok {
			var n int64
			if _, err := fmt.Sscan(v, &n); err == nil {
				return time.Unix(n, 0)
			}
		}
	}
	return time.Time{}
}

// updates: `basalt updates` (Settings, Updates and channels; docs/updates.md).
//
//	basalt updates [--json]                   what the last check found, grouped; restart; history
//	basalt updates status [--json]            the running step, restart and history only (no dnf query)
//	basalt updates check [--json]             update.check: refresh the package lists and the report (root)
//	basalt updates install [--security]       the update.install proposal (root stores it)
//	basalt updates rollback                   the update.rollback proposal for the last update
func (a *app) updates(ctx context.Context) error {
	sub := ""
	if len(a.o.args) > 1 {
		sub = a.o.args[1]
	}
	s := a.updatesSys()
	switch sub {
	case "", "list":
		r := updates.Build(ctx, s)
		if a.o.json {
			return a.printJSON(r)
		}
		a.writeUpdates(r)
		return nil
	case "status":
		// The running step and the history only (no dnf query): the
		// desktop follows an update with it.
		r := updates.Progress(ctx, s)
		if a.o.json {
			return a.printJSON(r)
		}
		a.writeUpdates(r)
		return nil
	case "check":
		if !a.root {
			return errors.New("checking for updates refreshes the system's package lists: run it with sudo (or from Settings)")
		}
		chk := action.Action{Kind: action.UpdateCheck, Params: map[string]string{}}
		cmds, _ := chk.Commands()
		start := time.Now()
		res := runner.Exec{}.Run(ctx, cmds[0])
		ok := res.OK()
		checked := time.Time{}
		if ok {
			checked = time.Now()
		}
		updates.Refresh(ctx, s, checked)
		r := updates.Build(ctx, s)
		_, _ = a.audit.Append("check", fmt.Sprintf("update.check: %d updates available (%d security)", r.Counts["total"], r.Counts["security"]),
			map[string]any{"action": action.UpdateCheck, "ok": ok, "seconds": int(time.Since(start).Seconds()), "updates": r.Counts["total"],
				"security": r.Counts["security"], "output": clipOut(res.Out, 2000)})
		if !ok {
			r.Errors = append(r.Errors, "dnf makecache: "+clipOut(res.Out, 500))
		}
		if a.o.json {
			return a.printJSON(r)
		}
		a.writeUpdates(r)
		if !ok {
			return errors.New("the package lists could not be refreshed; the list above is from the last successful check")
		}
		return nil
	case "install":
		scope := "all"
		if a.o.security {
			scope = "security"
		}
		// Exactly what the last check found and the page shows (no dnf
		// query here: the desktop's read helper runs this).
		c := updates.ReadCache(s)
		if c.Checked == "" && len(c.Updates) == 0 {
			return errors.New("no check for updates yet: sudo basalt updates check (or Check for updates in Settings)")
		}
		p, err := updates.InstallProposal(c.Updates, "cli", scope)
		if err != nil {
			return err
		}
		return a.storeAndPresent(ctx, p)
	case "rollback":
		r := updates.Build(ctx, s)
		if r.Undo == nil {
			return errors.New("no update with a snapshot from before it is known on this system: nothing to undo here (basalt snapshots lists every snapshot)")
		}
		pre, _ := a.env.FindApplySnapshots(ctx, r.Undo.Proposal)
		if a.root && pre != r.Undo.Snapshot {
			return fmt.Errorf("snapshot %d, taken before %s, is no longer there", r.Undo.Snapshot, r.Undo.Proposal)
		}
		p, err := updates.RollbackProposal(*r.Undo, "cli")
		if err != nil {
			return err
		}
		return a.storeAndPresent(ctx, p)
	}
	return fmt.Errorf("unknown: basalt updates %s (basalt help)", sub)
}

// storeAndPresent stores a proposal (root) and prints it, or its stored
// reference with --json (the desktop's read helper asks for that).
func (a *app) storeAndPresent(ctx context.Context, p *proposal.Proposal) error {
	p = a.keep(p)
	if a.o.json {
		return a.printJSON(storedRef(p, a.root))
	}
	return a.present(ctx, p)
}

func clipOut(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

func (a *app) writeUpdates(r updates.Report) {
	say := func(f string, args ...any) { fmt.Fprintf(a.out, f+"\n", args...) }
	if r.Checked == "" {
		say("Never checked for updates here: sudo basalt updates check")
	} else {
		say("Last check: %s", r.Checked)
	}
	if r.Running != nil {
		say("Installing now (%s): step %d of %d, %s", r.Running.Proposal, r.Running.Step, r.Running.Steps, r.Running.What)
	}
	if len(r.Updates) == 0 {
		say("No updates are waiting.")
	} else {
		say("%d updates, %s to download: %d security, %d Basalt OS, %d apps, %d system.", r.Counts["total"], humanSize(r.Download),
			r.Counts["security"], r.Counts["basalt"], r.Counts["apps"], r.Counts["system"])
		for _, g := range []string{updates.GroupSecurity, updates.GroupBasalt, updates.GroupApps, updates.GroupSystem} {
			first := true
			for _, u := range r.Updates {
				if u.Group != g {
					continue
				}
				if first {
					say("\n  %s", strings.ToUpper(g[:1])+g[1:])
					first = false
				}
				adv := ""
				if u.Advisory != "" {
					adv = "  " + u.Advisory
					if u.Severity != "" && u.Severity != "None" {
						adv += " (" + u.Severity + ")"
					}
				}
				say("    %-34s %s -> %s  %s%s", u.Name, orNew(u.From), u.To, humanSize(u.Download), adv)
			}
		}
		say("\nInstall them: sudo basalt updates install --apply   (security only: --security)")
	}
	if r.Restart.Needed {
		say("\nA restart is needed: %s", strings.Join(r.Restart.Reasons, ", "))
	}
	if r.Undo != nil {
		say("Undo the last update (%s, back to snapshot %d): sudo basalt updates rollback --apply", r.Undo.Proposal, r.Undo.Snapshot)
	}
	for _, e := range r.Errors {
		say("Note: %s", e)
	}
}

func orNew(s string) string {
	if s == "" {
		return "new"
	}
	return s
}

func humanSize(n int64) string {
	switch {
	// Decimal units, as the desktop shows them.
	case n >= 1e9:
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%d kB", n/1000)
	}
	return fmt.Sprintf("%d B", n)
}

// channels: `basalt channels` (Settings, Updates and channels).
//
//	basalt channels [--json]                        channels, added sources, other repositories, the catalog
//	basalt channels enable|disable NAME [--consent preview-builds-1]
//	basalt channels add ENTRY                       a catalog entry (pinned URL and key)
//	basalt channels add copr --id OWNER/PROJECT     a COPR project
//	basalt channels add custom --repo-url URL       or --baseurl URL --key-url URL --name NAME
//	basalt channels remove SOURCE                   a source added through Basalt (all repositories of it)
func (a *app) channels(ctx context.Context) error {
	sub := ""
	if len(a.o.args) > 1 {
		sub = a.o.args[1]
	}
	paths := action.SourcePaths
	switch sub {
	case "", "list":
		r := sources.Build(paths)
		if a.o.json {
			return a.printJSON(r)
		}
		a.writeChannels(r)
		return nil
	case "enable", "disable":
		if len(a.o.args) < 3 {
			return fmt.Errorf("usage: basalt channels %s NAME", sub)
		}
		repo := a.o.args[2]
		kind := action.RepoEnable
		if sub == "disable" {
			kind = action.RepoDisable
		}
		params := map[string]string{"repo": repo}
		if kind == action.RepoEnable {
			if sources.Testing(repo) {
				params["consent"] = a.o.kv["--consent"]
			}
			if repo == sources.ChanNonfreeTesting && !sources.Build(paths).NonfreeTestingDefined {
				params["definition"] = "install"
			}
		}
		act := action.Action{Kind: kind, Params: params}
		if err := act.Validate(); err != nil {
			return err
		}
		p := newProposal("channel", repo, "channel:"+sub+":"+repo, fmt.Sprintf("turn the %s channel %s", repo, map[string]string{"enable": "on", "disable": "off"}[sub]))
		p.Actions = []action.Action{act}
		p.Report = channelReport(repo, sub)
		return a.storeAndPresent(ctx, p)
	case "add":
		if len(a.o.args) < 3 {
			return errors.New("usage: basalt channels add ENTRY | copr --id OWNER/PROJECT | custom --repo-url URL")
		}
		p, err := a.addSource(ctx, a.o.args[2])
		if err != nil {
			return err
		}
		return a.storeAndPresent(ctx, p)
	case "remove":
		if len(a.o.args) < 3 {
			return errors.New("usage: basalt channels remove SOURCE")
		}
		name := a.o.args[2]
		recs := sources.Group(paths, name)
		if len(recs) == 0 && sources.HasRecord(paths, name) {
			for _, r := range sources.Records(paths) {
				if r.ID == name {
					recs = append(recs, r)
				}
			}
		}
		if len(recs) == 0 {
			return fmt.Errorf("%s is not a source added through Basalt (basalt channels lists them)", name)
		}
		p := newProposal("source", name, "source:remove:"+name, "remove the software source "+recs[0].Name)
		for _, r := range recs {
			p.Actions = append(p.Actions, action.Action{Kind: action.SourceRemove, Params: map[string]string{"id": r.ID}})
			p.Evidence = append(p.Evidence, fmt.Sprintf("%s: %s, key %s (%s), added %s", r.ID, r.URL, sources.Spaced(r.Fingerprint), r.KeyOwner, r.Added))
		}
		p.Report = "Remove " + recs[0].Name + ": no more software comes from it, and its signing key is no longer trusted. " +
			"Programs already installed from it stay; they no longer get updates."
		return a.storeAndPresent(ctx, p)
	}
	return fmt.Errorf("unknown: basalt channels %s (basalt help)", sub)
}

func newProposal(kind, subject, key, title string) *proposal.Proposal {
	now := time.Now().UTC()
	return &proposal.Proposal{ID: proposal.NewID(), Created: now, Updated: now, LastSeen: now, Seen: 1, Source: "cli",
		Kind: kind, Subject: subject, Key: key, Status: proposal.Pending, Title: title, Severity: 1}
}

func channelReport(repo, sub string) string {
	if sub == "disable" {
		return "Turn the " + repo + " channel off: no more updates come from it. What is installed from it stays."
	}
	switch repo {
	case sources.ChanTesting:
		return "Turn the basalt-testing channel on: preview builds of Basalt OS components, before they reach everyone. " +
			"Preview builds can break things. A snapshot is taken before each update so you can go back."
	case sources.ChanNonfreeTesting:
		return "Turn the basalt-nonfree-testing channel on: preview builds of the additional drivers (the NVIDIA driver), " +
			"for testers with that hardware. Preview builds can break things. A snapshot is taken before each update so you can go back."
	}
	return "Turn the " + repo + " channel on: updates from now on also come from it. Its packages are signed with the OpenBasalt release key."
}

var reSlug = regexp.MustCompile(`[^a-z0-9]+`)

// addSource builds the source.add proposal: for a catalog entry its pinned
// values, for COPR and custom sources what the person gave. The key is
// downloaded now, to show its fingerprint and owner, and must match the
// pin; `basalt __source add` downloads and checks it again when it runs.
func (a *app) addSource(ctx context.Context, which string) (*proposal.Proposal, error) {
	paths := action.SourcePaths
	repos := sources.ReadRepos(paths)
	var ps []sources.Params
	var title, name string
	var pinned bool
	switch which {
	case sources.CatalogCopr:
		project := a.o.kv["--id"]
		url, key, err := sources.CoprURLs(project)
		if err != nil {
			return nil, err
		}
		id := sources.CoprID(project)
		name = "COPR " + project
		ps = []sources.Params{{ID: id, Name: name, Kind: sources.KindRPM, URLType: sources.URLBase, URL: url, KeyURL: key,
			GPGCheck: "1", RepoGPGCheck: "0", Catalog: sources.CatalogCopr, Group: id}}
	case sources.CatalogCustom:
		p := sources.Params{Kind: sources.KindRPM, GPGCheck: "1", RepoGPGCheck: "1", Catalog: sources.CatalogCustom}
		if u := a.o.kv["--repo-url"]; u != "" {
			b, err := sources.HTTPFetch(ctx, u)
			if err != nil {
				return nil, err
			}
			rf, err := sources.ParseRepoFile(b)
			if err != nil {
				return nil, err
			}
			if !rf.RepoGPGCheck {
				return nil, errors.New("the repository does not sign its package lists (repo_gpgcheck=0): Basalt adds custom sources only when they do")
			}
			p.ID, p.Name, p.URLType, p.URL, p.KeyURL = rf.ID, rf.Name, rf.URLType, rf.URL, rf.KeyURL
		} else {
			p.Name, p.URL, p.KeyURL, p.URLType = a.o.kv["--name"], a.o.kv["--baseurl"], a.o.kv["--key-url"], sources.URLBase
			if p.URL == "" || p.KeyURL == "" {
				return nil, errors.New("a custom source needs --repo-url URL, or --baseurl URL with --key-url URL (its signing key)")
			}
			if p.Name == "" {
				p.Name = p.URL
			}
			p.ID = a.o.kv["--id"]
			if p.ID == "" {
				p.ID = strings.Trim(reSlug.ReplaceAllString(strings.ToLower(p.Name), "-"), "-")
				if len(p.ID) > 40 {
					p.ID = strings.Trim(p.ID[:40], "-")
				}
				if p.ID == "" || p.ID[0] < 'a' || p.ID[0] > 'z' {
					p.ID = "custom-" + p.ID
				}
			}
		}
		p.Group = p.ID
		name = p.Name
		ps = []sources.Params{p}
	default:
		e, ok := sources.Lookup(which)
		if !ok {
			return nil, fmt.Errorf("%q is not in the catalog (basalt channels lists it); use custom or copr", which)
		}
		pinned, name = true, e.Name
		for _, r := range e.Repos {
			rg := "0"
			if e.RepoGPGCheck {
				rg = "1"
			}
			ps = append(ps, sources.Params{ID: r.ID, Name: r.Name, Kind: e.Kind, URLType: r.URLType, URL: r.URL, KeyURL: e.KeyURL,
				Fingerprint: e.Fingerprint, GPGCheck: "1", RepoGPGCheck: rg, Catalog: e.ID, Group: e.ID})
		}
	}
	// The key, now: its fingerprint and owner are what the person sees.
	k, err := sources.FetchKey(ctx, sources.HTTPFetch, ps[0].Kind, ps[0].URL, ps[0].KeyURL)
	if err != nil {
		return nil, fmt.Errorf("the signing key of %s: %w", name, err)
	}
	for i := range ps {
		if pinned && k.Fingerprint != ps[i].Fingerprint {
			return nil, fmt.Errorf("the key published for %s has fingerprint %s, not %s, which Basalt pins: nothing is proposed",
				name, sources.Spaced(k.Fingerprint), sources.Spaced(ps[i].Fingerprint))
		}
		ps[i].Fingerprint = k.Fingerprint
		if err := ps[i].Validate(); err != nil {
			return nil, err
		}
		if _, exists := repos[ps[i].ID]; exists || sources.HasRecord(paths, ps[i].ID) {
			return nil, fmt.Errorf("a repository named %s is already defined on this system", ps[i].ID)
		}
	}
	title = "add the software source " + name
	p := newProposal("source", ps[0].Group, "source:add:"+ps[0].Group+":"+k.Fingerprint, title)
	p.Severity = 3
	p.NeedsReview = true
	for _, sp := range ps {
		p.Actions = append(p.Actions, action.Action{Kind: action.SourceAdd, Params: sp.Map()})
	}
	who := k.Owner
	if who == "" {
		who = "no name in the key"
	}
	trust := "Basalt pins this key for " + name + "."
	if !pinned {
		trust = "Basalt does not know this source: check the fingerprint with its publisher before you trust it."
	}
	meta := "Its packages and its package lists are signed."
	if ps[0].RepoGPGCheck != "1" {
		meta = "Its packages are signed; its package lists are not (the source does not sign them), so they come from a metalink over HTTPS."
	}
	p.Report = fmt.Sprintf("Add %s. Software from this source can change your whole system: only add sources you trust. "+
		"Its signing key, %s, belongs to %s. %s %s", name, sources.Spaced(k.Fingerprint), who, trust, meta)
	for _, sp := range ps {
		p.Evidence = append(p.Evidence, fmt.Sprintf("%s: %s %s", sp.ID, sp.URLType, sp.URL))
	}
	p.Evidence = append(p.Evidence, "key: "+ps[0].KeyURL, "fingerprint: "+sources.Spaced(k.Fingerprint), "owner: "+who,
		"the network: dnf runs as the system, not in an agent session, so no agent allowlist changes (docs/updates.md)")
	return p, nil
}

func (a *app) writeChannels(r sources.Report) {
	say := func(f string, args ...any) { fmt.Fprintf(a.out, f+"\n", args...) }
	say("Basalt channels (signed with the OpenBasalt release key %s)", r.ShortKey)
	for _, c := range r.Channels {
		state := "off"
		switch {
		case !c.Defined:
			state = "not installed (" + c.Definition + ")"
		case c.Enabled:
			state = "on"
		}
		sig := "signature: " + c.Signature.Short
		if c.Signature.OpenBasalt {
			sig = "signed with the OpenBasalt key " + c.Signature.Short
		}
		if c.Signature.Problem != "" {
			sig += " (" + c.Signature.Problem + ")"
		}
		if !c.Defined {
			sig = ""
		}
		say("  %-24s %-6s %s", c.ID, state, sig)
	}
	if len(r.Sources) > 0 {
		say("\nSources added through Basalt")
		for _, s := range r.Sources {
			state := map[bool]string{true: "on", false: "off"}[s.Enabled]
			say("  %-24s %-6s %s, key %s (%s), added %s %s", s.ID, state, s.Name, s.Short, s.KeyOwner, s.Added, s.By)
		}
	}
	if len(r.Other) > 0 {
		say("\nOther repositories (not added through Basalt; shown only)")
		for _, o := range r.Other {
			say("  %-24s %-6s %s", o.ID, map[bool]string{true: "on", false: "off"}[o.Enabled], o.File)
		}
	}
	say("\nCatalog (basalt channels add ENTRY)")
	for _, c := range r.Catalog {
		mark := ""
		if c.Added {
			mark = " (added)"
		}
		say("  %-20s %s, %s; key %s%s", c.ID, c.Name, c.Description, c.Short, mark)
	}
	say("  copr                 basalt channels add copr --id OWNER/PROJECT")
	say("  custom               basalt channels add custom --repo-url URL")
	say("\nHow updates are verified: %s", r.Docs)
}

// sourceParams reads the arguments of `basalt __source add` (as
// sources.Params.Argv writes them; --kind is the shared option).
func sourceParams(o opts) sources.Params {
	kv := o.kv
	return sources.Params{ID: kv["--id"], Name: kv["--name"], Kind: o.kind, URLType: kv["--url-type"], URL: kv["--url"],
		KeyURL: kv["--key-url"], Fingerprint: kv["--fingerprint"], GPGCheck: "1", RepoGPGCheck: kv["--repo-gpgcheck"],
		Catalog: kv["--catalog"], Group: kv["--group"]}
}

// sourceHelper is `basalt __source add|remove`, run by a confirmed
// source.add or source.remove (never by hand: its arguments are the
// action's, rebuilt and checked again here).
func (a *app) sourceHelper(ctx context.Context) error {
	if !a.root {
		return errors.New("__source runs as root, from a confirmed proposal")
	}
	w := sources.Writer{Paths: action.SourcePaths, Fetch: sources.HTTPFetch, Run: sources.RealRun, Now: time.Now}
	if len(a.o.args) < 2 {
		return errors.New("usage: basalt __source add|remove")
	}
	kv := a.o.kv
	switch a.o.args[1] {
	case "add":
		p := sourceParams(a.o)
		if err := w.Add(ctx, p); err != nil {
			return err
		}
		fmt.Fprintf(a.out, "added %s (key %s)\n", p.ID, sources.Spaced(p.Fingerprint))
		return nil
	case "remove":
		if err := w.Remove(ctx, kv["--id"]); err != nil {
			return err
		}
		fmt.Fprintf(a.out, "removed %s\n", kv["--id"])
		return nil
	}
	return errors.New("usage: basalt __source add|remove")
}

// offlineFinish is `basalt __offline-finish` (basalt-offline-finish.service,
// at the start after an offline update): the after snapshot, the checks,
// the record.
func (a *app) offlineFinish(ctx context.Context) error {
	if !a.root {
		return errors.New("__offline-finish runs as root, from basalt-offline-finish.service")
	}
	ap := a.applier()
	defer ap.Gate.Close()
	err := ap.FinishOffline(ctx)
	if errors.Is(err, apply.ErrNoPending) {
		return nil
	}
	return err
}
