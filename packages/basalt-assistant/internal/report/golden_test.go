package report

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/decide"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/diag"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/explain"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/selinux"
)

// go test ./internal/report -update rewrites testdata/golden/*.txt.
var update = flag.Bool("update", false, "rewrite the golden files")

func dec(q, top string, p, th float64) decide.Decision {
	return decide.Decision{Question: decide.Question{ID: q}, Answer: decide.Answer{Top: top, Confidence: p, Backend: "rules/v1"},
		Threshold: th, Confident: p >= th}
}

func act(kind string, kv ...string) action.Action {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return action.Action{Kind: kind, Params: m}
}

func avc(stype, ttype, class string, perms []string, name string, port int, count int) selinux.Group {
	return selinux.Group{AVC: selinux.AVC{Perms: perms, SContext: "system_u:system_r:" + stype + ":s0",
		TContext: "system_u:object_r:" + ttype + ":s0", Class: class, Name: name, Port: port,
		Raw: "avc:  denied  { " + strings.Join(perms, " ") + " } for comm=\"nginx\" scontext=system_u:system_r:" + stype + ":s0"}, Count: count}
}

func unitRep(unit, cause string, conf float64) *diag.UnitReport {
	return &diag.UnitReport{Unit: unit, Cause: cause, State: map[string]string{"LoadState": "loaded", "ActiveState": "failed", "Result": "exit-code"},
		Features: map[string]bool{"unit_failed": true}, Decision: dec("unit.cause", cause, conf, 0.75),
		Evidence: []string{"state: failed/failed, result exit-code, main process exited status 1, restarts 0"}}
}

// scenarios cover every kind and cause the templates know.
func scenarios() map[string]*proposal.Proposal {
	ps := map[string]*proposal.Proposal{}

	r := unitRep("nginx.service", "config_error", 0.97)
	r.ConfigFile, r.ConfigLine = "/etc/nginx/nginx.conf", 47
	r.ConfigCheck = &diag.CheckResult{Command: "nginx -t", Output: "nginx: [emerg] unknown directive \"bogus_directive\" in /etc/nginx/nginx.conf:47\nnginx: configuration file /etc/nginx/nginx.conf test failed"}
	r.Journal = []string{"10:02:11 nginx: [emerg] unknown directive \"bogus_directive\" in /etc/nginx/nginx.conf:47", "10:02:11 nginx.service: Failed with result 'exit-code'."}
	r.Restore = &diag.RestoreCandidate{Path: "/etc/nginx/nginx.conf", Snapshot: 27, Date: "2026-10-03 18:20:01", How: "content", Checked: "nginx -t"}
	r.Actions = []action.Action{act(action.FileRestore, "path", "/etc/nginx/nginx.conf", "snapshot", "27"), act(action.UnitRestart, "unit", "nginx.service")}
	ps["unit-config-restore"] = FromUnit("cli", r)

	r = unitRep("nginx.service", "config_error", 0.97)
	r.ConfigFile, r.ConfigLine = "/etc/nginx/nginx.conf", 47
	r.Journal = []string{"10:02:11 nginx: [emerg] unknown directive \"bogus_directive\" in /etc/nginx/nginx.conf:47"}
	r.Restore = &diag.RestoreCandidate{Path: "/etc/nginx/nginx.conf", Snapshot: 27, Date: "2026-10-03 18:20:01", How: "size/mtime"}
	r.Hints = []action.Hint{{Reason: "snapshot 27 holds a different copy of /etc/nginx/nginx.conf (size/mtime)",
		Actions: []action.Action{act(action.FileRestore, "path", "/etc/nginx/nginx.conf", "snapshot", "27"), act(action.UnitRestart, "unit", "nginx.service")}}}
	r.Skipped = []string{"config checker (needs root)"}
	ps["unit-config-hint"] = FromUnit("daemon", r)

	r = unitRep("basalt-lab-appconf.service", "config_error", 0.9)
	r.Journal = []string{"19:05:37 config error: invalid value maybe for option debug in /etc/basalt-lab/app.conf line 3", "19:05:37 basalt-lab-appconf.service: Failed with result 'exit-code'."}
	ps["unit-config-by-hand"] = FromUnit("cli", r)

	r = unitRep("nginx.service", "selinux_denial", 0.95)
	r.Domain = "httpd_t"
	fx := selinux.Fix{Group: avc("httpd_t", "user_home_t", "file", []string{"read"}, "index.html", 0, 3), Path: "/srv/site/index.html",
		Actions: []action.Action{act(action.SELinuxFcontext, "path", "/srv/site", "type", "httpd_sys_content_t")}}
	r.AVCs = []selinux.Fix{fx}
	r.AVCDecision = []decide.Decision{dec("avc.class", "missing_fcontext", 0.9, 0.75)}
	r.Actions = append(fx.Actions, act(action.UnitRestart, "unit", "nginx.service"))
	ps["unit-selinux"] = FromUnit("cli", r)

	r = unitRep("nginx.service", "port_conflict", 0.96)
	r.Ports = []diag.PortInfo{{Port: 80, Owner: "httpd (pid 812)"}}
	ps["unit-port-conflict"] = FromUnit("cli", r)

	r = unitRep("app.service", "dependency_failed", 0.93)
	r.Deps = []diag.DepState{{Unit: "postgresql.service", Active: "failed", Result: "exit-code", Load: "loaded"}}
	ps["unit-dependency"] = FromUnit("cli", r)

	r = unitRep("postgresql.service", "disk_full", 0.9)
	r.Disks = []diag.FSStat{{Path: "/var/lib/pgsql", Total: 100 << 30, Free: 1 << 28}}
	ps["unit-disk-full"] = FromUnit("cli", r)

	r = unitRep("basalt-lab-noconf.service", "missing_file", 0.88)
	r.Journal = []string{"19:05:49 cat: /etc/basalt-lab/missing.conf: No such file or directory"}
	ps["unit-missing-file"] = FromUnit("cli", r)

	r = unitRep("worker.service", "crashed", 0.9)
	r.Features["oom_killed"] = true
	r.OOM = &diag.OOMInfo{Constraint: "CONSTRAINT_MEMCG", Limits: map[string]string{"MemoryMax": "268435456"}}
	ps["unit-oom"] = FromUnit("cli", r)

	r = unitRep("worker.service", "crashed", 0.85)
	r.State["Result"] = "core-dump"
	r.Actions = []action.Action{act(action.UnitRestart, "unit", "worker.service")}
	ps["unit-crashed"] = FromUnit("cli", r)

	r = unitRep("basalt-lab-segv.service", "crashed", 0.85)
	r.State["Result"] = "core-dump"
	r.Crash = &diag.CrashInfo{Code: "dumped", Status: "11/SEGV", Count: 3, Window: "24 hours", RanFor: "0 s", Repeating: true,
		Reasons: []string{"it ended the same way (dumped 11/SEGV) 3 times in the last 24 hours", "it crashed 0 s after it started, on its way up"}}
	ps["unit-crash-loop"] = FromUnit("cli", r)

	r = unitRep("basalt-lab-dac.service", "unknown", 0.5)
	r.Features["dac_denied"] = true
	r.DAC = []diag.DACFinding{{Path: "/srv/lab/data", Mode: "0700", Owner: "root", User: "nobody (uid 65534)", Need: "search"}}
	ps["unit-permission"] = FromUnit("cli", r)

	r = unitRep("strange.service", "unknown", 0.4)
	r.Journal = []string{"11:00:01 strange: giving up after 3 attempts"}
	ps["unit-unknown"] = FromUnit("cli", r)

	r = unitRep("nginx.service", "unknown", 0.9)
	r.Healthy, r.State["ActiveState"], r.Features = true, "active", map[string]bool{}
	ps["unit-healthy"] = FromUnit("cli", r)

	r = unitRep("ngnix.service", "unknown", 0.9)
	r.State["LoadState"] = "not-found"
	ps["unit-not-found"] = FromUnit("cli", r)

	r = unitRep("nginx.service", "selinux_denial", 0.95)
	r.Errors = []string{"sesearch: policy busy after 10 attempts"}
	ps["unit-incomplete"] = FromUnit("cli", r)

	r = unitRep("nginx.service", "crashed", 0.55)
	r.Actions = []action.Action{act(action.UnitRestart, "unit", "nginx.service")}
	ps["unit-low-confidence"] = FromUnit("cli", r)

	sel := func(class string, fx selinux.Fix, conf float64) *proposal.Proposal {
		fx.Class = class // the analyzer's own classification, as on a machine
		return FromSELinux("cli", diag.SELinuxItem{Fix: fx, Decision: dec("avc.class", class, conf, 0.75)})
	}
	ps["selinux-mislabeled"] = sel(selinux.ClassMislabeled, selinux.Fix{Group: avc("httpd_t", "admin_home_t", "file", []string{"read"}, "index.html", 0, 1),
		Path: "/usr/share/nginx/html/index.html", DefaultType: "httpd_sys_content_t",
		Actions: []action.Action{act(action.SELinuxRestorecon, "path", "/usr/share/nginx/html/index.html")}}, 0.97)
	ps["selinux-fcontext"] = sel(selinux.ClassMissingFcontext, selinux.Fix{Group: avc("httpd_t", "var_t", "dir", []string{"write"}, "logs", 0, 4),
		Path: "/srv/app/logs", Actions: []action.Action{act(action.SELinuxFcontext, "path", "/srv/app/logs", "type", "httpd_log_t")}}, 0.9)
	ps["selinux-port"] = sel(selinux.ClassPort, selinux.Fix{Group: avc("httpd_t", "unreserved_port_t", "tcp_socket", []string{"name_bind"}, "", 8085, 2),
		Facts: map[string]string{"port_type_now": "unreserved_port_t"}, Actions: []action.Action{act(action.SELinuxPort, "port", "8085", "proto", "tcp", "type", "http_port_t", "mode", "add")}}, 0.95)
	ps["selinux-boolean"] = sel(selinux.ClassBoolean, selinux.Fix{Group: avc("httpd_t", "http_port_t", "tcp_socket", []string{"name_connect"}, "", 8080, 6),
		Actions: []action.Action{act(action.SELinuxBoolean, "name", "httpd_can_network_relay", "value", "on")}}, 0.9)
	ps["selinux-suspicious"] = sel(selinux.ClassSuspicious, selinux.Fix{Group: avc("httpd_t", "shadow_t", "file", []string{"read"}, "shadow", 0, 1),
		Path: "/etc/shadow"}, 0.92)
	ps["selinux-unknown"] = sel(selinux.ClassUnknown, selinux.Fix{Group: avc("myapp_t", "proc_t", "file", []string{"getattr"}, "kcore", 0, 1)}, 0.6)

	disk := func(top string, pct float64, crit bool) *diag.DiskReport {
		return &diag.DiskReport{UsedPct: pct, Features: map[string]bool{"disk_warn": true, "disk_crit": crit}, Decision: dec("disk.cause", top, 0.9, 0.75),
			Journal: 1610 << 20, PkgCache: 920 << 20, SnapshotTotal: 3 << 30, Forecast: diag.Forecast{Samples: 12, SpanHours: 6, DaysToFull: 3},
			Evidence: []string{"/ is 96 % full"}, Snapshots: []diag.SnapSpace{{Snapshot: diag.Snapshot{Number: 12, Date: "2026-09-30 08:00:01", Type: "single", Description: "before upgrade"}, Exclusive: 2900 << 20}}}
	}
	d := disk("snapshots", 96, true)
	d.Actions = []action.Action{act(action.SnapshotDelete, "snapshot", "12")}
	ps["disk-snapshots-critical"] = FromDisk("cli", d)
	d = disk("journal", 88, false)
	d.Actions = []action.Action{act(action.JournalVacuum, "size", "200M")}
	ps["disk-journal"] = FromDisk("cli", d)
	d = disk("package_cache", 87, false)
	d.Actions = []action.Action{act(action.DnfClean)}
	ps["disk-package-cache"] = FromDisk("cli", d)
	d = disk("other_data", 91, false)
	d.Skipped, d.Snapshots, d.Forecast = []string{"space per snapshot (btrfs ioctls need root)"}, nil, diag.Forecast{}
	ps["disk-data-confined"] = FromDisk("daemon", d)
	d = disk("other_data", 42, false)
	d.Features = map[string]bool{}
	ps["disk-ok"] = FromDisk("cli", d)

	dn := &diag.DnfReport{Command: "dnf install basalt-lab-postfail", Failed: []string{"basalt-lab-postfail-1-1.noarch"},
		Pre: &diag.Snapshot{Number: 41, Date: "2026-10-04 09:12:00"}, Decision: dec("dnf.next", "rollback", 0.9, 0.75),
		Packages:    diag.PackageDiff{Added: []string{"basalt-lab-postfail-1-1.noarch"}},
		LogLines:    []string{"2026-10-04T09:12:03-0300 [rpm] WARNING scriptlet failed, exit status 1"},
		Actions:     []action.Action{act(action.SnapshotRollback, "snapshot", "41")},
		Explanation: "The transaction failed after changing packages."}
	ps["dnf-rollback"] = FromDnf("cli", dn)
	dh := *dn
	dh.Actions, dh.Packages = nil, diag.PackageDiff{}
	dh.Hints = []action.Hint{{Reason: "snapshot 41 was taken just before the failed transaction", Actions: []action.Action{act(action.SnapshotRollback, "snapshot", "41")}}}
	ps["dnf-rollback-hint"] = FromDnf("daemon", &dh)
	di := &diag.DnfReport{Command: "dnf install basalt-lab-broken", Failed: []string{"basalt-lab-broken-1-1.noarch"}, Pre: &diag.Snapshot{Number: 43},
		Decision: dec("dnf.next", "investigate", 0.85, 0.75), LogLines: []string{"error: %prein(basalt-lab-broken-1-1.noarch) scriptlet failed, exit status 1"}}
	ps["dnf-investigate"] = FromDnf("cli", di)

	rb := &diag.RollbackPlan{Target: diag.Snapshot{Number: 52, Date: "2026-10-04 10:40:12", Description: "basalt apply p-9f8e7d"},
		Packages: diag.PackageDiff{}, Files: diag.FileDiff{Total: 3, Etc: []string{"c..... /etc/nginx/nginx.conf"}},
		Actions: []action.Action{act(action.SnapshotRollback, "snapshot", "52")}}
	ps["rollback-before"] = FromRollback("cli", rb, "Undo p-9f8e7d: return the root to snapshot 52, taken just before it was applied.", "p-9f8e7d")

	// A proposal stored before facts existed falls back to its report.
	old := FromUnit("cli", unitRep("old.service", "crashed", 0.9))
	old.Facts, old.Report, old.Actions = nil, "old.service was killed (signal); a restart is proposed.", []action.Action{act(action.UnitRestart, "unit", "old.service")}
	ps["legacy-no-facts"] = old

	applied := FromUnit("cli", unitRep("worker.service", "crashed", 0.9))
	applied.Actions = []action.Action{act(action.UnitRestart, "unit", "worker.service")}
	applied.Status = proposal.Applied
	applied.Result = &proposal.Result{OK: true, PreSnapshot: 60, PostSnapshot: 61}
	ps["applied"] = applied

	// Additional drivers: the NVIDIA driver on a laptop with two GPUs, and
	// compute only on a server.
	for name, v := range map[string]string{"driver-install-hybrid": "display", "driver-install-compute": "compute"} {
		f := explain.New("driver", "install", "nvidia")
		f.Set("gpu", "NVIDIA GeForce RTX 4060 Laptop GPU").Set("version", "595.104.02").Set("variant", v).Set("kernel", "7.2.8-200.fc44.x86_64")
		if v == "display" {
			f.Set("primary", "Intel Corporation Raptor Lake-P [Iris Xe Graphics]")
		}
		dp := base("cli", "driver", "nvidia", "driver:nvidia:install:"+v)
		dp.Title, dp.Facts, dp.Severity = "install the NVIDIA driver 595.104.02 for NVIDIA GeForce RTX 4060 Laptop GPU", f, 1
		dp.Actions = []action.Action{act(action.DriverInstall, "driver", "nvidia", "variant", v, "kernel", "7.2.8-200.fc44.x86_64",
			"license", action.NvidiaLicenseSHA256)}
		dp.Evidence = []string{"GPU 0000:01:00.0: NVIDIA GeForce RTX 4060 Laptop GPU (10de:28a0, 3d), kernel driver now: nouveau"}
		ps[name] = dp
	}

	t0 := time.Date(2026, 10, 4, 10, 15, 0, 0, time.UTC)
	for _, p := range ps {
		p.ID, p.Created = "p-1a2b3c", t0
	}
	return ps
}

func TestGolden(t *testing.T) {
	for name, p := range scenarios() {
		got := Render(p)
		fn := filepath.Join("testdata", "golden", name+".txt")
		if *update {
			if err := os.MkdirAll(filepath.Dir(fn), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fn, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(fn)
		if err != nil {
			t.Fatalf("%s: %v (run go test -update)", name, err)
		}
		if got != string(want) {
			t.Errorf("%s differs from %s:\n%s", name, fn, got)
		}
	}
}

// Every template prose passes the faithfulness check against its own
// facts: the check that guards a model never rejects the templates.
func TestTemplatesAreFaithful(t *testing.T) {
	for name, p := range scenarios() {
		parts := RenderWith(p, Options{})
		if parts.Facts == nil {
			continue
		}
		al := explain.NewAllowed(parts.Facts, parts.Changes)
		if bad := al.Check(parts.Text.Prose(), 0); len(bad) > 0 {
			t.Errorf("%s: template prose fails the check: %v\n%s", name, bad, parts.Text.Prose())
		}
	}
}

// Fixed slots: commands are rendered verbatim, a hint never offers apply,
// a report never shows a command block, MCP text never carries the code.
func TestRenderSlots(t *testing.T) {
	ps := scenarios()
	hint := Render(ps["unit-config-hint"])
	for _, want := range []string{"Suggested change (a hint", "$ cp --preserve=mode,ownership,timestamps /.snapshots/27/snapshot/etc/nginx/nginx.conf /etc/nginx/nginx.conf",
		"sudo basalt confirm p-1a2b3c"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint: missing %q", want)
		}
	}
	if strings.Contains(hint, "basalt apply") {
		t.Errorf("a hint offers apply:\n%s", hint)
	}
	full := Render(ps["unit-config-restore"])
	for _, want := range []string{"What will run", "$ systemctl restart nginx.service", "Risk: medium (", "Undo: a snapshot is taken",
		"$ sudo basalt snapshots rollback --before p-1a2b3c", "Apply it:   sudo basalt apply p-1a2b3c", "--yes --confirm "} {
		if !strings.Contains(full, want) {
			t.Errorf("proposal: missing %q", want)
		}
	}
	if mcp := RenderWith(ps["unit-config-restore"], Options{NoCode: true}).String(); strings.Contains(mcp, "--confirm") {
		t.Errorf("MCP text carries the confirmation code:\n%s", mcp)
	}
	if rep := Render(ps["unit-port-conflict"]); strings.Contains(rep, "What will run") || !strings.Contains(rep, "Nothing will be changed") {
		t.Errorf("report:\n%s", rep)
	}
	if ok := Render(ps["unit-healthy"]); strings.Count(ok, "\n") > 6 {
		t.Errorf("a healthy unit should be short:\n%s", ok)
	}
	if !strings.Contains(Line(ps["unit-config-hint"]), "[hint]") {
		t.Errorf("line %q", Line(ps["unit-config-hint"]))
	}
}
