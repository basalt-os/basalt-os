package main

// render-data writes fact -> prose pairs for the optional humanize model,
// and humanize measures a model (any OpenAI-compatible endpoint) on them.
//
// The facts are generated here from pools of unit names, paths, SELinux
// types and numbers, through the same fact keys the assistant's report
// package fills; the prose is the assistant's own template wording with a
// random choice among its equivalent phrasings (explain.WriteVariant). The
// training and held-out splits draw from disjoint pools of names and
// paths, so a model that copies its training values fails the
// faithfulness check on the held-out set. No third-party text.

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/explain"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/llm"
)

// RenderGenerator names this generator in the provenance of every pair.
const RenderGenerator = "basalt-eval render-data/1"

type pools struct {
	units    []string
	files    map[string]string // unit -> config file
	checkers map[string]string
	dirs     []string // data directories for SELinux cases
	users    []string
	domains  [][2]string // unit stem -> domain
	commands []string    // package names for dnf cases
}

var trainPools = pools{
	units: []string{"nginx", "httpd", "postgresql", "mariadb", "redis", "sshd", "chronyd", "named", "postfix", "dovecot",
		"haproxy", "squid", "smb", "crond", "grafana-server", "prometheus", "myapp", "api", "worker", "backup", "gitea", "php-fpm"},
	files: map[string]string{"nginx": "/etc/nginx/nginx.conf", "httpd": "/etc/httpd/conf/httpd.conf", "sshd": "/etc/ssh/sshd_config",
		"named": "/etc/named.conf", "postfix": "/etc/postfix/main.cf", "haproxy": "/etc/haproxy/haproxy.cfg", "squid": "/etc/squid/squid.conf",
		"smb": "/etc/samba/smb.conf", "dovecot": "/etc/dovecot/dovecot.conf", "redis": "/etc/redis/redis.conf", "php-fpm": "/etc/php-fpm.d/www.conf"},
	checkers: map[string]string{"nginx": "nginx -t", "httpd": "httpd -t", "sshd": "sshd -t", "named": "named-checkconf",
		"postfix": "postfix check", "haproxy": "haproxy -c -f /etc/haproxy/haproxy.cfg", "squid": "squid -k parse", "smb": "testparm -s",
		"dovecot": "doveconf -n"},
	dirs:  []string{"/srv/site", "/srv/www/shop", "/var/www/blog", "/opt/app/data", "/srv/static", "/srv/app/logs", "/var/www/html/uploads"},
	users: []string{"nginx", "apache", "postgres", "nobody", "app", "redis", "named"},
	domains: [][2]string{{"nginx", "httpd_t"}, {"httpd", "httpd_t"}, {"postgresql", "postgresql_t"}, {"named", "named_t"},
		{"postfix", "postfix_master_t"}, {"redis", "redis_t"}, {"sshd", "sshd_t"}, {"squid", "squid_t"}, {"smb", "smbd_t"}},
	commands: []string{"htop", "nodejs", "php", "python3-flask", "basalt-lab-postfail", "zabbix-agent", "golang", "ruby"},
}

var heldoutPools = pools{
	units: []string{"caddy", "unbound", "mosquitto", "memcached", "rabbitmq-server", "jenkins", "minio", "netdata", "tomcat",
		"vaultwarden", "keycloak", "traefik", "coturn", "synapse", "lighttpd", "exim", "kea-dhcp4", "openvpn-server@main"},
	files: map[string]string{"caddy": "/etc/caddy/Caddyfile", "unbound": "/etc/unbound/unbound.conf", "mosquitto": "/etc/mosquitto/mosquitto.conf",
		"lighttpd": "/etc/lighttpd/lighttpd.conf", "exim": "/etc/exim/exim.conf", "kea-dhcp4": "/etc/kea/kea-dhcp4.conf",
		"tomcat": "/etc/tomcat/server.xml", "traefik": "/etc/traefik/traefik.toml", "coturn": "/etc/coturn/turnserver.conf"},
	checkers: map[string]string{"caddy": "caddy validate --config /etc/caddy/Caddyfile", "unbound": "unbound-checkconf",
		"lighttpd": "lighttpd -tt -f /etc/lighttpd/lighttpd.conf", "exim": "exim -bV", "kea-dhcp4": "kea-dhcp4 -t /etc/kea/kea-dhcp4.conf"},
	dirs:  []string{"/data/media", "/srv/apps/portal", "/var/www/wiki", "/opt/minio/export", "/srv/git/repos", "/data/www/landing"},
	users: []string{"caddy", "unbound", "mosquitto", "memcached", "tomcat", "lighttpd"},
	domains: [][2]string{{"caddy", "httpd_t"}, {"lighttpd", "httpd_t"}, {"unbound", "named_t"}, {"memcached", "memcached_t"},
		{"tomcat", "tomcat_t"}, {"exim", "exim_t"}, {"mosquitto", "unconfined_service_t"}},
	commands: []string{"vim-enhanced", "podman-compose", "rust", "texlive-scheme-basic", "basalt-lab-broken", "cockpit", "jq"},
}

type genCtx struct {
	r *rand.Rand
	p pools
}

func (g genCtx) one(xs []string) string { return xs[g.r.Intn(len(xs))] }
func (g genCtx) n(lo, hi int) int       { return lo + g.r.Intn(hi-lo+1) }
func (g genCtx) ns(lo, hi int) string   { return strconv.Itoa(g.n(lo, hi)) }
func (g genCtx) yes(p float64) bool     { return g.r.Float64() < p }

func (g genCtx) date() string {
	return fmt.Sprintf("2026-%02d-%02d %02d:%02d:%02d", g.n(1, 12), g.n(1, 28), g.n(0, 23), g.n(0, 59), g.n(0, 59))
}

func (g genCtx) size() string {
	if g.yes(0.5) {
		return fmt.Sprintf("%d.%d GiB", g.n(1, 40), g.n(0, 9))
	}
	return fmt.Sprintf("%d.%d MiB", g.n(100, 990), g.n(0, 9))
}

func (g genCtx) unit() (stem, unit string) {
	stem = g.one(g.p.units)
	return stem, stem + ".service"
}

func act(kind string, kv ...string) action.Action {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return action.Action{Kind: kind, Params: m}
}

// sample makes one finding: its facts and planned changes.
func (g genCtx) sample() (*explain.Facts, []action.Action) {
	stem, u := g.unit()
	switch g.n(0, 9) {
	case 0, 1, 2, 3, 4: // units
		causes := []string{"config_error", "config_error", "selinux_denial", "port_conflict", "dependency_failed", "disk_full",
			"missing_file", "oom", "crashed", "permission", "unknown", "ok", "not_found"}
		cause := g.one(causes)
		f := explain.New("unit", cause, u)
		var acts []action.Action
		switch cause {
		case "ok":
			f.Cause, f.OK = "unknown", true
		case "config_error":
			file := g.p.files[stem]
			if file == "" {
				file = "/etc/" + stem + "/" + stem + ".conf"
			}
			f.Set("file", file)
			if g.yes(0.8) {
				f.Set("line", g.ns(1, 400))
			}
			if ck := g.p.checkers[stem]; ck != "" && g.yes(0.7) {
				msg := g.one([]string{"unknown directive \"%s\" in %s:%s", "invalid value \"%s\" in %s:%s", "unexpected \"%s\" in %s:%s"})
				f.Set("checker", ck).Set("checker_msg", fmt.Sprintf(msg, g.one([]string{"listn", "server_nme", "on", "}", "workers"}), file, orS(f.V("line"), "1")))
			} else {
				f.Set("journal", fmt.Sprintf("config error: invalid value for option %s in %s line %s", g.one([]string{"debug", "port", "workers", "timeout"}), file, orS(f.V("line"), "3")))
			}
			if g.yes(0.6) {
				snap := g.ns(2, 400)
				f.Set("snapshot", snap).Set("snapshot_date", g.date()).Set("restore_path", file)
				acts = []action.Action{act(action.FileRestore, "path", file, "snapshot", snap), act(action.UnitRestart, "unit", u)}
				f.Hint = g.yes(0.4)
			}
		case "selinux_denial":
			dom := "httpd_t"
			for _, d := range g.p.domains {
				if d[0] == stem {
					dom = d[1]
				}
			}
			dir := g.one(g.p.dirs)
			f.Set("domain", dom).Set("perm", g.one([]string{"read", "write", "open", "getattr", "search", "create"})).
				Set("object", dir+"/"+g.one([]string{"index.html", "app.log", "data.db", "config.json", "cache"})).
				Set("target_type", g.one([]string{"default_t", "var_t", "user_home_t", "admin_home_t", "tmp_t"}))
			if g.yes(0.5) {
				f.Set("count", g.ns(2, 60))
			}
			if g.yes(0.6) {
				acts = []action.Action{act(action.SELinuxFcontext, "path", dir, "type", g.one([]string{"httpd_sys_content_t", "httpd_sys_rw_content_t", "httpd_log_t"})),
					act(action.UnitRestart, "unit", u)}
			} else {
				f.Review = true
			}
		case "port_conflict":
			f.Set("port", strconv.Itoa(g.one2([]int{80, 443, 8080, 8443, 5432, 6379, 25, 53, 3000, g.n(1024, 65000)})))
			if g.yes(0.8) {
				ostem, _ := g.unit()
				f.Set("port_owner", fmt.Sprintf("%s (pid %d)", ostem, g.n(300, 99999)))
			}
		case "dependency_failed":
			_, d := g.unit()
			f.Set("dep", d).Set("dep_state", g.one([]string{"failed", "failed", "inactive"}))
		case "disk_full":
			f.Set("fs_path", g.one([]string{"/", "/var/log", "/srv", "/var/lib/pgsql", "/home"})).Set("fs_pct", g.ns(97, 100))
		case "missing_file":
			f.Set("journal", fmt.Sprintf("%s: %s: No such file or directory", stem, g.one(g.p.dirs)+"/"+g.one([]string{"app.conf", "key.pem", "settings.ini"})))
		case "oom":
			switch g.n(0, 2) {
			case 0:
				f.Set("memory_max", fmt.Sprintf("%d.0 MiB", 64*g.n(1, 16))).Set("oom_scope", "cgroup")
			case 1:
				f.Set("oom_scope", "system")
			}
		case "crashed":
			f.Set("result", g.one([]string{"core-dump", "signal", "exit-code", "watchdog"}))
			acts = []action.Action{act(action.UnitRestart, "unit", u)}
		case "permission":
			f.Set("dac_path", g.one(g.p.dirs)).Set("dac_mode", g.one([]string{"0700", "0750", "0600", "0640"})).Set("dac_owner", "root").
				Set("dac_user", fmt.Sprintf("%s (uid %d)", g.one(g.p.users), g.n(48, 999))).Set("dac_need", g.one([]string{"read", "search", "write"}))
		case "unknown":
			f.Set("journal", fmt.Sprintf("%s: giving up after %d attempts", stem, g.n(2, 9)))
		}
		if g.yes(0.12) && len(acts) > 0 && !f.Hint {
			f.Review = true
			f.Set("confidence", fmt.Sprintf("0.%02d", g.n(40, 74))).Set("threshold", "0.75")
		}
		if f.Hint {
			// The hint's actions are what the prose describes.
			return f, acts
		}
		return f, acts
	case 5, 6: // SELinux denial groups
		_, dom := g.domain()
		class := g.one([]string{"mislabeled", "missing_fcontext", "port", "boolean", "suspicious", "unknown"})
		f := explain.New("selinux", class, dom)
		f.Set("domain", dom).Set("perm", g.one([]string{"read", "write", "name_bind", "name_connect", "getattr", "open"}))
		if g.yes(0.4) {
			f.Set("count", g.ns(2, 90))
		}
		var acts []action.Action
		switch class {
		case "mislabeled":
			obj := g.one(g.p.dirs) + "/index.html"
			f.Set("object", obj).Set("path", obj).Set("target_type", g.one([]string{"admin_home_t", "user_home_t", "tmp_t"})).Set("default_type", "httpd_sys_content_t")
			acts = []action.Action{act(action.SELinuxRestorecon, "path", obj)}
		case "missing_fcontext":
			dir := g.one(g.p.dirs)
			want := g.one([]string{"httpd_sys_content_t", "httpd_log_t", "httpd_sys_rw_content_t"})
			f.Set("object", dir).Set("path", dir).Set("target_type", g.one([]string{"var_t", "default_t", "usr_t"})).Set("want_type", want)
			acts = []action.Action{act(action.SELinuxFcontext, "path", dir, "type", want)}
		case "port":
			port := g.ns(1025, 65000)
			f.Set("object", "tcp port "+port).Set("port", port).Set("proto", "tcp").Set("target_type", "unreserved_port_t").Set("port_type_now", "unreserved_port_t")
			acts = []action.Action{act(action.SELinuxPort, "port", port, "proto", "tcp", "type", g.one([]string{"http_port_t", "postgresql_port_t", "dns_port_t"}), "mode", "add")}
		case "boolean":
			b := g.one([]string{"httpd_can_network_connect", "httpd_can_network_relay", "httpd_can_network_connect_db", "httpd_enable_homedirs", "httpd_can_sendmail"})
			f.Set("object", "tcp port "+g.ns(1, 9999)).Set("target_type", "http_port_t").Set("boolean", b)
			acts = []action.Action{act(action.SELinuxBoolean, "name", b, "value", "on")}
		case "suspicious":
			f.Set("object", g.one([]string{"/etc/shadow", "/root/.ssh/id_ed25519", "/etc/sudoers", "/proc/kcore"})).Set("target_type", g.one([]string{"shadow_t", "ssh_home_t", "etc_t", "proc_kcore_t"}))
			f.Review = true
		case "unknown":
			f.Set("object", g.one([]string{"kcore", "random", "sock_file", "memfd"})).Set("target_type", g.one([]string{"proc_t", "device_t", "init_t"}))
			f.Review = true
		}
		return f, acts
	case 7, 8: // disk
		cause := g.one([]string{"snapshots", "journal", "package_cache", "other_data", "ok"})
		f := explain.New("disk", cause, "")
		var acts []action.Action
		if cause == "ok" {
			f.Cause, f.OK = "other_data", true
			f.Set("used_pct", g.ns(10, 84))
			return f, nil
		}
		pct := g.n(85, 100)
		f.Set("used_pct", strconv.Itoa(pct))
		if pct >= 95 {
			f.Set("level", "critical")
		}
		if g.yes(0.4) {
			f.Set("days_to_full", g.ns(1, 30))
		}
		switch cause {
		case "snapshots":
			n := g.ns(2, 400)
			f.Set("snapshot", n).Set("snapshot_date", g.date()).Set("snapshot_desc", g.one([]string{"before upgrade", "dnf install " + g.one(g.p.commands), "timeline", "basalt apply p-" + hex6(g)})).
				Set("snapshot_size", g.size())
			acts = []action.Action{act(action.SnapshotDelete, "snapshot", n)}
		case "journal":
			f.Set("journal_size", g.size())
			acts = []action.Action{act(action.JournalVacuum, "size", g.one([]string{"200M", "100M", "500M", "1G"}))}
		case "package_cache":
			f.Set("cache_size", g.size())
			acts = []action.Action{act(action.DnfClean)}
		case "other_data":
			if g.yes(0.3) {
				f.Set("confined", "yes")
			}
		}
		return f, acts
	default: // package transactions and rollbacks
		if g.yes(0.3) {
			f := explain.New("snapshot", "rollback", "")
			n := g.ns(2, 400)
			f.Set("snapshot", n).Set("snapshot_date", g.date()).Set("snapshot_desc", g.one([]string{"before upgrade", "dnf upgrade", "timeline"}))
			if g.yes(0.5) {
				id := "p-" + hex6(g)
				f.Set("before", id).Set("snapshot_desc", "basalt apply "+id)
			}
			f.Set("added", g.ns(0, 5)).Set("removed", g.ns(0, 5)).Set("changed", g.ns(0, 40)).Set("files", g.ns(0, 300))
			return f, []action.Action{act(action.SnapshotRollback, "snapshot", n)}
		}
		pkg := g.one(g.p.commands)
		cause := g.one([]string{"rollback", "investigate"})
		f := explain.New("dnf", cause, "")
		f.Set("command", "dnf install "+pkg).Set("failed", fmt.Sprintf("%s-%d.%d-1.fc44.x86_64", pkg, g.n(1, 9), g.n(0, 20)))
		snap := g.ns(2, 400)
		f.Set("snapshot", snap)
		if cause == "rollback" {
			f.Set("added", g.ns(1, 30)).Set("removed", g.ns(0, 3)).Set("changed", g.ns(0, 10))
			f.Hint = g.yes(0.4)
			if f.Hint {
				delete(f.Values, "added")
				delete(f.Values, "removed")
				delete(f.Values, "changed")
			}
			return f, []action.Action{act(action.SnapshotRollback, "snapshot", snap)}
		}
		return f, nil
	}
}

func (g genCtx) one2(xs []int) int { return xs[g.r.Intn(len(xs))] }

func (g genCtx) domain() (string, string) {
	d := g.p.domains[g.r.Intn(len(g.p.domains))]
	return d[0], d[1]
}

func hex6(g genCtx) string { return fmt.Sprintf("%06x", g.r.Intn(1<<24)) }

func orS(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// Pair is one training or evaluation item.
type Pair struct {
	ID         string         `json:"id"`
	Split      string         `json:"split"`
	Input      explain.Input  `json:"input"`
	User       string         `json:"user"`  // the user message the model gets (explain.UserMessage)
	Prose      string         `json:"prose"` // the target: template wording, random phrasing
	Template   string         `json:"template"`
	Provenance map[string]any `json:"provenance"`
}

func runRenderData(args []string) error {
	fs := flag.NewFlagSet("render-data", flag.ExitOnError)
	split := fs.String("split", "train", "train or heldout (disjoint pools)")
	n := fs.Int("n", 4000, "pairs")
	seed := fs.Int64("seed", 20261004, "random seed")
	out := fs.String("out", "", "output file (JSON lines; default stdout)")
	_ = fs.Parse(args)
	p := trainPools
	if *split == "heldout" {
		p = heldoutPools
	} else if *split != "train" {
		return fmt.Errorf("split %q", *split)
	}
	w := bufio.NewWriter(os.Stdout)
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = bufio.NewWriter(f)
	}
	defer w.Flush()
	g := genCtx{r: rand.New(rand.NewSource(*seed)), p: p}
	seen := map[string]bool{}
	for i := 0; len(seen) < *n && i < *n*20; i++ {
		f, acts := g.sample()
		in := explain.ModelInput(f, explain.Changes(acts))
		user := explain.UserMessage(in)
		if seen[user] {
			continue
		}
		seen[user] = true
		pr := Pair{ID: fmt.Sprintf("%s-%05d", *split, len(seen)), Split: *split, Input: in, User: user,
			Prose: explain.WriteVariant(f, acts, g.r).Prose(), Template: explain.Write(f, acts).Prose(),
			Provenance: map[string]any{"generator": RenderGenerator, "seed": *seed, "split": *split,
				"method":  "facts sampled from fixed pools; prose from the assistant's templates with random equivalent phrasings",
				"license": "Apache-2.0"}}
		b, _ := json.Marshal(pr)
		w.Write(append(b, '\n'))
	}
	return nil
}

// runHumanize asks a model for the prose of each pair through the same
// Humanizer the CLI uses (prompt, redaction for a remote endpoint,
// faithfulness check, fallback) and reports the acceptance rate and the
// latency; -out keeps every text for the quality rubric.
func runHumanize(args []string) error {
	fs := flag.NewFlagSet("humanize", flag.ExitOnError)
	ep := fs.String("endpoint", "http://127.0.0.1:8080/v1", "OpenAI-compatible endpoint")
	model := fs.String("model", "", "model name (the endpoint's first model when empty)")
	set := fs.String("set", "eval/render-heldout.jsonl", "pairs (render-data)")
	out := fs.String("out", "", "per-item results (JSON lines)")
	prompt := fs.String("prompt", "auto", "auto, full or compact")
	stream := fs.Bool("stream", true, "stream, and time the first accepted sentence")
	limit := fs.Int("n", 0, "at most this many pairs (0: all)")
	_ = fs.Parse(args)
	c := &llm.Client{Endpoint: *ep, Model: *model, Timeout: 180 * time.Second}
	h := &explain.Humanizer{C: c, Prompt: *prompt}
	if *model == "" {
		h.Model, _ = c.ServedModel(context.Background())
	}
	var w *bufio.Writer
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = bufio.NewWriter(f)
		defer w.Flush()
	}
	var lat, first []float64
	n, accepted, failed := 0, 0, 0
	reasons := map[string]int{}
	err := readJSONL(*set, func(b []byte) error {
		if *limit > 0 && n >= *limit {
			return nil
		}
		var p Pair
		if err := json.Unmarshal(b, &p); err != nil {
			return err
		}
		f := &explain.Facts{Kind: p.Input.Kind, Cause: p.Input.Cause, Subject: p.Input.Subject, Values: p.Input.Values}
		for _, fl := range p.Input.Flags {
			switch fl {
			case "hint_to_confirm":
				f.Hint = true
			case "needs_review":
				f.Review = true
			case "incomplete":
				f.Incomplete = true
			case "nothing_wrong":
				f.OK = true
			}
		}
		var acts []action.Action
		for _, ch := range p.Input.Changes {
			acts = append(acts, action.Action{Kind: ch.Kind, Params: ch.Params})
		}
		n++
		t0 := time.Now()
		var firstMS float64 = -1
		var cb func(string)
		if *stream {
			cb = func(string) {
				if firstMS < 0 {
					firstMS = float64(time.Since(t0).Milliseconds())
				}
			}
		}
		res := h.Write(context.Background(), f, acts, p.Template, cb)
		ms := float64(time.Since(t0).Milliseconds())
		switch {
		case res.Err != nil:
			failed++
			reasons["error"]++
		case res.Accepted:
			accepted++
			lat = append(lat, ms)
			if firstMS >= 0 {
				first = append(first, firstMS)
			}
		default:
			lat = append(lat, ms)
			for _, pr := range res.Problems {
				k, _, _ := strings.Cut(pr, " \"")
				reasons[k]++
			}
		}
		if w != nil {
			rec, _ := json.Marshal(map[string]any{"id": p.ID, "accepted": res.Accepted, "model_text": res.Model, "shown": res.Text,
				"template": p.Template, "problems": res.Problems, "ms": ms, "first_sentence_ms": firstMS, "error": errString(res.Err)})
			w.Write(append(rec, '\n'))
		}
		return nil
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"items": n, "accepted": accepted, "failed_calls": failed,
		"faithful_rate": round(float64(accepted)/float64(max(n, 1)), 3), "fallback_rate": round(float64(n-accepted)/float64(max(n, 1)), 3),
		"rejections": reasons, "latency_ms_p50": pct(lat, 50), "latency_ms_p95": pct(lat, 95),
		"first_sentence_ms_p50": pct(first, 50), "first_sentence_ms_p95": pct(first, 95), "model": h.Model, "endpoint": *ep})
}
