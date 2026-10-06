package record

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Plain-English sentences for records, from templates (no model needed).
// A template names data fields as {field}; {agent}, {project}, {who},
// {session} come from the envelope. Unknown events fall back to a generic
// sentence, so every record reads as something.
var templates = map[string]string{
	"session.start":                 "{agent} started in {mode} mode on {project}",
	"session.end":                   "{agent} finished after {duration} (exit code {exit_code}): {egress_allowed} connections allowed, {egress_denied} refused",
	"egress.allow":                  "{agent} reached {host}:{port}",
	"egress.deny":                   "{agent} was stopped from reaching {host}:{port} ({reason})",
	"grant.request":                 "Someone asked to give {agent} more access: {kind} {value}",
	"grant.apply":                   "{agent} was given more access ({kind} {value}) after administrator approval",
	"relabel":                       "{agent} was handed {path} ({count} files labeled for its session)",
	"install":                       "{agent} tools were installed ({method} {package})",
	"credential.use":                "{agent} used the key {credential} on {host} (the key stays in the session proxy; the agent never sees it)",
	"credential.strip":              "{agent} sent credential headers ({headers}) to {host}:{port}; they were removed, keys go only to their own provider",
	"egress.session.start":          "Network for {agent} session is closed by default; {n_allow} names allowed",
	"egress.session.end":            "Network rules for {agent} session were removed ({reason})",
	"egress.session.refused":        "A request to filter cgroup {cgroup} was refused: {reason}",
	"egress.grant":                  "{agent} may now also reach {entry}",
	"dns.allow":                     "{agent} looked up {name} (allowed, {addresses})",
	"dns.deny":                      "{agent} asked for {name}, which is not on its list; the lookup was refused",
	"dns.rebinding":                 "{name} pointed at a local address ({addresses}); refused, to protect local services",
	"dns.direct":                    "{agent} tried to use another DNS server ({dst}); blocked",
	"egress.drop":                   "{agent} tried to connect straight to {dst}:{port} without an allowed name; dropped",
	"selinux.avc":                   "SELinux stopped {comm} ({source}) from {perms} on {tclass} {name}",
	"selinux.error":                 "SELinux reported a policy error: {message}",
	"login.session":                 "{user} logged in (session {login_session})",
	"auth.failure":                  "A failed login or authentication for {acct} ({exe})",
	"polkit.auth":                   "{user} authenticated as an administrator for {action}",
	"escalation.pkexec":             "{user} ran {command} as {as} through pkexec",
	"escalation.pkexec:denied":      "{user} tried to run {command} as {as} through pkexec and was refused ({reason})",
	"escalation.sudo":               "{user} ran {command} as {as} with sudo",
	"escalation.sudo:denied":        "{user} tried to run {command} as {as} with sudo and was refused ({reason})",
	"polkit.auth:denied":            "{user} failed to authenticate as an administrator for {action}",
	"agent.grant.helper":            "The grant helper {verdict}: {text}",
	"snapshot.create":               "Snapshot {number} was taken ({description})",
	"snapshot.rollback":             "The system was rolled back: snapshot {new_root} becomes the root at the next boot",
	"driver.install":                "The NVIDIA driver was installed; the next start is a trial (snapshot {snapshot} holds the system from before)",
	"driver.check":                  "The NVIDIA driver passed its check after the start ({gpu})",
	"driver.check:error":            "The NVIDIA driver failed its check after the start: {reason}",
	"driver.fallback":               "The NVIDIA driver was switched off and nouveau is used from the next start: {reason}",
	"driver.retry":                  "The NVIDIA driver will be tried again at the next start",
	"driver.boot":                   "This start used nouveau instead of the NVIDIA driver: {reason}",
	"driver.boot:error":             "This start used nouveau instead of the NVIDIA driver: {reason}",
	"driver.kernel_hold":            "Kernel {kernel} has no NVIDIA module yet; the computer keeps starting kernel {default}",
	"driver.kernel_release":         "Kernel {kernel} now has its NVIDIA module and is the default again",
	"model.download.consent":        "{user} chose Download for {what} ({bytes} bytes) on the desktop ({purpose})",
	"model.remove.consent":          "{user} chose Remove for {what} on the desktop",
	"model.download.request":        "{user} agreed to download {what} ({models}, {size} bytes) from the desktop",
	"model.download.request:denied": "{user} asked to download {what} from the desktop and was refused: {reason}",
	"model.remove.request:denied":   "{user} asked to remove {what} from the desktop and was refused: {reason}",
	"model.download":                "{what} was downloaded and verified against its pinned checksum ({models}, {size} bytes), as {user} agreed",
	"model.download:error":          "The download of {what} that {user} agreed to failed ({reason})",
	"model.enable":                  "The assistant's local model ({what}) was turned on after its download",
	"model.remove":                  "{user} removed the model {what}",
	"ledger.start":                  "The audit ledger started (chain at record {last_seq}, exports signed with the {key_kind} key {key_id})",
	"ledger.retention":              "{file} (records {first_seq} to {last_seq}) was removed by the retention policy ({retention}), sealed {sealed}",
	"ledger.seal":                   "The ledger file was sealed and continues in a new file ({file})",
	"ledger.continue":               "The ledger continues from {from}",
	"ledger.refused":                "A request to {op} the ledger was refused ({reason})",
	"ledger.ratelimited":            "A producer sent too many records; {dropped} were refused",
	"ledger.dropped":                "{records} records were lost before they reached the ledger ({reason})",
}

var fieldRe = regexp.MustCompile(`\{([a-z_]+)\}`)

// Describe returns a plain-English sentence for r.
func Describe(r Record) string {
	d := r.DataMap()
	vals := map[string]string{}
	for k, v := range d {
		vals[k] = format(v)
	}
	vals["agent"] = agentName(r)
	vals["project"] = shortPath(r.Subject.Project)
	vals["mode"] = r.Subject.Mode
	vals["session"] = r.Session
	if _, ok := vals["user"]; !ok {
		vals["user"] = fmt.Sprintf("uid %d", r.UID)
	}
	if v, ok := d["duration_s"]; ok {
		vals["duration"] = (time.Duration(toInt(v)) * time.Second).String()
	}
	if a, ok := d["allow"].([]any); ok {
		vals["n_allow"] = fmt.Sprint(len(a))
	}
	if r.Event == "agent.grant.helper" {
		vals["verdict"] = "granted a request"
		if r.Outcome != "ok" {
			vals["verdict"] = "refused a request"
		}
	}
	if r.Event == EventStart && vals["key_kind"] == "" {
		vals["key_kind"] = "development" // started by 0.1.0
	}
	if r.Event == "selinux.avc" {
		vals["source"] = contextType(fmt.Sprint(d["scontext"]))
	}
	if r.Event == "assistant.apply" || strings.HasPrefix(r.Event, "assistant.") {
		if t, ok := d["text"].(string); ok && t != "" {
			return "Assistant: " + t
		}
	}
	tpl, ok := templates[r.Event+":"+r.Outcome]
	if !ok {
		tpl, ok = templates[r.Event]
	}
	if !ok {
		s := fmt.Sprintf("%s: %s (%s)", r.Producer, r.Event, r.Outcome)
		if r.Session != "" {
			s += " in session " + r.Session
		}
		return s
	}
	out := fieldRe.ReplaceAllStringFunc(tpl, func(m string) string {
		v := vals[m[1:len(m)-1]]
		if v == "" || v == "<nil>" {
			return "?"
		}
		return v
	})
	return out
}

func agentName(r Record) string {
	switch {
	case r.Subject.Profile != "" && r.Session != "":
		return fmt.Sprintf("Agent %s (%s)", r.Subject.Profile, r.Session)
	case r.Subject.Profile != "":
		return "Agent " + r.Subject.Profile
	case r.Subject.App != "":
		return r.Subject.App
	case r.Session != "":
		return "Session " + r.Session
	}
	return "A program"
}

// shortPath shows a project as ~/rel when under /home/USER.
func shortPath(p string) string {
	if p == "" {
		return "?"
	}
	parts := strings.Split(filepath.Clean(p), "/")
	if len(parts) > 3 && parts[1] == "home" {
		return "~/" + strings.Join(parts[3:], "/")
	}
	return p
}

func contextType(ctx string) string {
	f := strings.Split(ctx, ":")
	if len(f) >= 3 {
		return f[2]
	}
	return ctx
}

func format(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprint(int64(x))
		}
		return fmt.Sprint(x)
	case []any:
		s := make([]string, 0, len(x))
		for _, e := range x {
			s = append(s, format(e))
		}
		return strings.Join(s, ", ")
	case bool:
		if x {
			return "yes"
		}
		return "no"
	}
	return fmt.Sprint(v)
}

func toInt(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int:
		return int64(x)
	case int64:
		return x
	}
	return 0
}

// Summary is a short plain-English digest of a set of records.
type Summary struct {
	Lines    []string         `json:"lines"`
	Counts   map[string]int   `json:"counts"` // by severity
	Sessions []SessionSummary `json:"sessions"`
}

// SessionSummary is what one agent session did.
type SessionSummary struct {
	Session   string   `json:"session"`
	Agent     string   `json:"agent"`
	Project   string   `json:"project"`
	Mode      string   `json:"mode"`
	Start     string   `json:"start,omitempty"`
	End       string   `json:"end,omitempty"`
	Reached   []string `json:"reached"`
	Refused   []string `json:"refused"`
	Denials   int      `json:"selinux_denials"`
	Grants    int      `json:"grants"`
	ExitCode  *int     `json:"exit_code,omitempty"`
	Refusals  int      `json:"refusals"`
	Rebinding int      `json:"rebinding"`
}

// Summarize groups records by agent session and counts the rest.
func Summarize(recs []Record) Summary {
	s := Summary{Counts: map[string]int{}}
	by := map[string]*SessionSummary{}
	var order []string
	other := map[string]int{}
	for _, r := range recs {
		s.Counts[r.Severity]++
		if r.Session == "" {
			if r.Producer != Producer {
				other[r.Event]++
			}
			continue
		}
		ss := by[r.Session]
		if ss == nil {
			ss = &SessionSummary{Session: r.Session}
			by[r.Session] = ss
			order = append(order, r.Session)
		}
		if r.Subject.Profile != "" {
			ss.Agent, ss.Mode = r.Subject.Profile, r.Subject.Mode
		}
		if r.Subject.Project != "" {
			ss.Project = r.Subject.Project
		}
		d := r.DataMap()
		switch r.Event {
		case "session.start":
			ss.Start = r.Time
		case "session.end":
			ss.End = r.Time
			c := int(toInt(d["exit_code"]))
			ss.ExitCode = &c
		case "egress.allow", "dns.allow":
			ss.Reached = addUniq(ss.Reached, firstOf(d, "host", "name"))
		case "egress.deny", "dns.deny", "egress.drop", "dns.direct":
			ss.Refusals++
			ss.Refused = addUniq(ss.Refused, firstOf(d, "host", "name", "dst"))
		case "dns.rebinding":
			ss.Rebinding++
			ss.Refused = addUniq(ss.Refused, firstOf(d, "name"))
		case "selinux.avc":
			ss.Denials++
		case "grant.apply", "egress.grant":
			if r.Outcome == "ok" {
				ss.Grants++
			}
		}
	}
	for _, id := range order {
		ss := by[id]
		s.Sessions = append(s.Sessions, *ss)
		who := "An agent"
		if ss.Agent != "" {
			who = "Agent " + ss.Agent
		}
		mode := ss.Mode
		if mode == "" {
			mode = "?"
		}
		line := fmt.Sprintf("%s worked on %s (%s mode, session %s)", who, shortPath(ss.Project), mode, id)
		if ss.ExitCode != nil {
			line += fmt.Sprintf(", exit code %d", *ss.ExitCode)
		} else {
			line += ", still running or ended without a record"
		}
		if len(ss.Reached) > 0 {
			line += "; reached " + strings.Join(ss.Reached, ", ")
		}
		if ss.Refusals+ss.Rebinding > 0 {
			line += fmt.Sprintf("; %d attempts refused (%s)", ss.Refusals+ss.Rebinding, strings.Join(ss.Refused, ", "))
		}
		if ss.Denials > 0 {
			line += fmt.Sprintf("; %d SELinux denials", ss.Denials)
		}
		if ss.Grants > 0 {
			line += fmt.Sprintf("; %d grants approved by an administrator", ss.Grants)
		}
		s.Lines = append(s.Lines, line+".")
	}
	keys := make([]string, 0, len(other))
	for k := range other {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	nice := map[string]string{"selinux.avc": "SELinux denials outside agent sessions", "login.session": "logins",
		"auth.failure": "failed authentications", "polkit.auth": "administrator authentications",
		"escalation.sudo": "sudo commands", "escalation.pkexec": "pkexec commands", "snapshot.rollback": "rollbacks",
		"snapshot.create": "snapshots taken"}
	for _, k := range keys {
		label := nice[k]
		if label == "" {
			label = k + " events"
		}
		s.Lines = append(s.Lines, fmt.Sprintf("%d %s.", other[k], label))
	}
	if len(s.Lines) == 0 {
		s.Lines = []string{"Nothing recorded in this period."}
	}
	return s
}

func firstOf(d map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := d[k]; ok && format(v) != "" {
			return format(v)
		}
	}
	return "?"
}

func addUniq(l []string, v string) []string {
	for _, x := range l {
		if x == v {
			return l
		}
	}
	if len(l) >= 20 {
		return l
	}
	return append(l, v)
}
