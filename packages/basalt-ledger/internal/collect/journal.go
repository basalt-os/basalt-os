// Package collect turns events other programs already log into ledger
// records: SELinux denials, failed authentications and logins (audit and
// systemd-logind), polkit authentications, pkexec and sudo escalations,
// the system assistant's audit records and basalt-agent's grant helper,
// all read from the journal; and snapshots and rollbacks, read from
// snapper's snapshot directory.
//
// The mapping functions are pure (FromJournal, FromSnapshot) and tested
// with captured samples; the readers around them are thin.
package collect

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/record"
)

// JournalMatches are journalctl match groups ("+" separates
// alternatives): audit records, and messages of the programs mapped here.
var JournalMatches = []string{
	"_TRANSPORT=audit", "+",
	"SYSLOG_IDENTIFIER=basalt-assistant", "SYSLOG_IDENTIFIER=systemd-logind", "SYSLOG_IDENTIFIER=polkitd",
	"SYSLOG_IDENTIFIER=pkexec", "SYSLOG_IDENTIFIER=sudo", "SYSLOG_IDENTIFIER=basalt-agent-grant",
}

// Mapped is a record from a collector and the producer name it uses.
type Mapped struct {
	Record record.Record
	Key    string // throttle key; empty: never throttled
}

func str(e map[string]any, k string) string {
	if v, ok := e[k].(string); ok {
		return v
	}
	return ""
}

func journalTime(e map[string]any) string {
	us, err := strconv.ParseInt(str(e, "__REALTIME_TIMESTAMP"), 10, 64)
	if err != nil {
		return time.Now().UTC().Format(time.RFC3339Nano)
	}
	return time.UnixMicro(us).UTC().Format(time.RFC3339Nano)
}

var kvRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_-]*)=("[^"]*"|'[^']*'|\S+)`)

// auditFields parses key=value pairs of an audit message, including the
// nested msg='...' of user-space records, whose values win (they describe
// the subject; the outer ones describe the reporting process).
func auditFields(msg string) map[string]string {
	out := map[string]string{}
	for _, m := range kvRe.FindAllStringSubmatch(msg, -1) {
		v := m[2]
		if strings.HasPrefix(v, "'") {
			for k, iv := range auditFields(strings.Trim(v, "'")) {
				out[k] = iv
			}
			continue
		}
		if _, set := out[m[1]]; !set {
			out[m[1]] = strings.Trim(v, `"`)
		}
	}
	return out
}

var permsRe = regexp.MustCompile(`\{\s*([^}]*?)\s*\}`)

// Level returns the MCS/MLS level of an SELinux context ("" without one).
func Level(ctx string) string {
	f := strings.SplitN(ctx, ":", 4)
	if len(f) == 4 {
		return f[3]
	}
	return ""
}

func ctxType(ctx string) string {
	f := strings.Split(ctx, ":")
	if len(f) >= 3 {
		return f[2]
	}
	return ""
}

func data(m map[string]any) json.RawMessage {
	clean := map[string]any{}
	for k, v := range m {
		if s, ok := v.(string); ok && s == "" {
			continue
		}
		clean[k] = v
	}
	b, _ := json.Marshal(clean)
	return b
}

func lookupUID(name string) int {
	if name == "" {
		return -1
	}
	if u, err := user.Lookup(name); err == nil {
		if n, err := strconv.Atoi(u.Uid); err == nil {
			return n
		}
	}
	return -1
}

var (
	polkitOK   = regexp.MustCompile(`^Operator of (\S+) successfully authenticated as (\S+) to gain (\S+) authorization for action (\S+) for (.*?) \(owned by unix-user:(\S+)\)`)
	polkitFail = regexp.MustCompile(`^Operator of (\S+) FAILED to authenticate to gain authorization for action (\S+) for (.*?) \(owned by unix-user:(\S+)\)`)
	pkexecOK   = regexp.MustCompile(`^(\S+): Executing command \[USER=(\S+)\] \[TTY=(\S*)\] \[CWD=(\S*)\] \[COMMAND=(.*)\]$`)
	pkexecFail = regexp.MustCompile(`^(\S+): Error executing command as another user: (.*?) \[USER=(\S+)\].*\[COMMAND=(.*)\]$`)
	grantRe    = regexp.MustCompile(`^(granted|refused): (?:uid (\d+) session (\S+) )?(.*)$`)
)

// FromJournal maps one journal entry (journalctl -o json) to a record.
func FromJournal(e map[string]any) (Mapped, bool) {
	msg := str(e, "MESSAGE")
	r := record.Record{Time: journalTime(e), UID: -1}
	if str(e, "_TRANSPORT") == "audit" {
		return fromAudit(e, msg, r)
	}
	switch str(e, "SYSLOG_IDENTIFIER") {
	case "basalt-assistant":
		typ := str(e, "BASALT_AUDIT_TYPE")
		if typ == "" {
			return Mapped{}, false
		}
		r.Producer, r.Event, r.Outcome = "basalt-assistant", "assistant."+typ, "ok"
		if typ == "refuse" {
			r.Outcome = "denied"
		}
		r.UID, _ = strconv.Atoi(str(e, "_UID"))
		r.Subject.App = "basalt-assistant"
		text := msg
		if i := strings.Index(msg, ": "); i >= 0 && strings.HasPrefix(msg, "basalt-assistant audit #") {
			text = msg[i+2:]
		}
		d := map[string]any{"text": text}
		if raw := str(e, "BASALT_AUDIT_DATA"); raw != "" {
			var v any
			if json.Unmarshal([]byte(raw), &v) == nil {
				d["data"] = v
			}
		}
		r.Data = data(d)
		seq, _ := strconv.ParseInt(str(e, "BASALT_AUDIT_SEQ"), 10, 64)
		r.Src = &record.Src{Seq: seq, Hash: str(e, "BASALT_AUDIT_HASH"), Prev: str(e, "BASALT_AUDIT_PREV")}
		return Mapped{Record: r}, true
	case "systemd-logind":
		if str(e, "MESSAGE_ID") != "8d45620c1a4348dbb17410da57c60c66" { // new session
			return Mapped{}, false
		}
		u := str(e, "USER_ID")
		r.Producer, r.Event, r.Outcome, r.UID = "login", "login.session", "ok", lookupUID(u)
		r.Subject.App = "systemd-logind"
		r.Data = data(map[string]any{"user": u, "login_session": str(e, "SESSION_ID"), "leader": str(e, "LEADER")})
		return Mapped{Record: r}, true
	case "polkitd":
		r.Producer, r.Event, r.Subject.App = "polkit", "polkit.auth", "polkit"
		if m := polkitOK.FindStringSubmatch(msg); m != nil {
			r.Outcome, r.UID = "ok", lookupUID(m[6])
			r.Data = data(map[string]any{"operator": m[1], "as": m[2], "scope": m[3], "action": m[4], "for": m[5], "user": m[6]})
			return Mapped{Record: r}, true
		}
		if m := polkitFail.FindStringSubmatch(msg); m != nil {
			r.Outcome, r.UID = "denied", lookupUID(m[4])
			r.Data = data(map[string]any{"operator": m[1], "action": m[2], "for": m[3], "user": m[4]})
			return Mapped{Record: r}, true
		}
		return Mapped{}, false
	case "pkexec":
		r.Producer, r.Event, r.Subject.App = "pkexec", "escalation.pkexec", "pkexec"
		if m := pkexecOK.FindStringSubmatch(msg); m != nil {
			r.Outcome, r.UID = "ok", lookupUID(m[1])
			r.Data = data(map[string]any{"user": m[1], "as": m[2], "tty": m[3], "cwd": m[4], "command": m[5]})
			return Mapped{Record: r}, true
		}
		if m := pkexecFail.FindStringSubmatch(msg); m != nil {
			r.Outcome, r.UID = "denied", lookupUID(m[1])
			r.Data = data(map[string]any{"user": m[1], "reason": m[2], "as": m[3], "command": m[4]})
			return Mapped{Record: r}, true
		}
		return Mapped{}, false
	case "sudo":
		d, ok := parseSudo(msg)
		if !ok {
			return Mapped{}, false
		}
		r.Producer, r.Event, r.Subject.App, r.UID = "sudo", "escalation.sudo", "sudo", lookupUID(d["user"].(string))
		r.Outcome = "ok"
		if _, failed := d["reason"]; failed {
			r.Outcome = "denied"
		}
		r.Data = data(d)
		return Mapped{Record: r}, true
	case "basalt-agent-grant":
		m := grantRe.FindStringSubmatch(msg)
		if m == nil {
			return Mapped{}, false
		}
		r.Producer, r.Event, r.Subject.App = "basalt-agent-grant", "agent.grant.helper", "basalt-agent"
		r.Outcome = "ok"
		if m[1] == "refused" {
			r.Outcome = "denied"
		}
		if m[2] != "" {
			r.UID, _ = strconv.Atoi(m[2])
			r.Session = m[3]
		}
		r.Data = data(map[string]any{"text": m[4]})
		return Mapped{Record: r}, true
	}
	return Mapped{}, false
}

var sudoKey = regexp.MustCompile(`^([A-Z]+)=(.*)$`)

// parseSudo reads sudo's log line: "USER : [REASON ; ]TTY=.. ; PWD=.. ;
// USER=.. ; COMMAND=..". Lines without COMMAND (PAM session notes) are
// not escalations.
func parseSudo(msg string) (map[string]any, bool) {
	user, rest, ok := strings.Cut(strings.TrimSpace(msg), " : ")
	if !ok || strings.ContainsAny(user, " ") {
		return nil, false
	}
	d := map[string]any{"user": user}
	parts := strings.Split(rest, " ; ")
	for i, p := range parts {
		m := sudoKey.FindStringSubmatch(p)
		if m == nil {
			d["reason"] = p
			continue
		}
		switch m[1] {
		case "COMMAND":
			d["command"] = strings.Join(append([]string{m[2]}, parts[i+1:]...), " ; ")
			return d, true
		case "TTY":
			d["tty"] = m[2]
		case "PWD":
			d["cwd"] = m[2]
		case "USER":
			d["as"] = m[2]
		}
	}
	return nil, false
}

func fromAudit(e map[string]any, msg string, r record.Record) (Mapped, bool) {
	r.Producer = "selinux"
	f := auditFields(msg)
	switch str(e, "_AUDIT_TYPE") {
	case "1400", "1107": // AVC, USER_AVC
		if !strings.Contains(msg, "avc:  denied") {
			return Mapped{}, false
		}
		perms := ""
		if m := permsRe.FindStringSubmatch(msg); m != nil {
			perms = m[1]
		}
		r.Event, r.Outcome = "selinux.avc", "denied"
		scon := f["scontext"]
		r.Subject = record.Subject{Level: Level(scon), App: ctxType(scon)}
		d := map[string]any{"perms": perms, "comm": f["comm"], "name": f["name"], "path": f["path"], "scontext": scon,
			"tcontext": f["tcontext"], "tclass": f["tclass"], "permissive": f["permissive"], "pid": f["pid"],
			"exe": f["exe"], "service": f["unit"]}
		if f["auid"] != "" && f["auid"] != "4294967295" {
			if n, err := strconv.Atoi(f["auid"]); err == nil {
				r.UID = n
			}
		}
		r.Data = data(d)
		key := fmt.Sprintf("avc %s %s %s %s %s", ctxType(scon), ctxType(f["tcontext"]), f["tclass"], perms, f["comm"])
		return Mapped{Record: r, Key: key}, true
	case "1401": // SELINUX_ERR
		r.Event, r.Outcome = "selinux.error", "error"
		r.Data = data(map[string]any{"message": msg})
		return Mapped{Record: r, Key: "selinux-err " + msg}, true
	case "1100", "1112": // USER_AUTH, USER_LOGIN
		if f["res"] != "failed" {
			return Mapped{}, false
		}
		r.Producer, r.Event, r.Outcome = "login", "auth.failure", "denied"
		r.UID = lookupUID(f["acct"])
		r.Subject.App = filepath.Base(f["exe"])
		r.Data = data(map[string]any{"acct": f["acct"], "exe": f["exe"], "addr": f["addr"], "terminal": f["terminal"],
			"op": f["op"], "hostname": f["hostname"]})
		return Mapped{Record: r, Key: "auth " + f["acct"] + " " + f["exe"] + " " + f["addr"]}, true
	}
	return Mapped{}, false
}

// Throttle lets one record per key through per window and counts the
// rest; the count is attached to the next record let through.
type Throttle struct {
	Window time.Duration
	mu     sync.Mutex
	last   map[string]time.Time
	skip   map[string]int
}

// Pass reports whether a record with key may be written; repeats is the
// number of records suppressed for the key since the last one passed.
func (t *Throttle) Pass(key string, now time.Time) (ok bool, repeats int) {
	if key == "" {
		return true, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last == nil {
		t.last, t.skip = map[string]time.Time{}, map[string]int{}
	}
	if l, seen := t.last[key]; seen && now.Sub(l) < t.Window {
		t.skip[key]++
		return false, 0
	}
	if len(t.last) > 8192 {
		t.last, t.skip = map[string]time.Time{}, map[string]int{}
	}
	t.last[key] = now
	n := t.skip[key]
	delete(t.skip, key)
	return true, n
}

// Sink receives collected records.
type Sink func(collector string, r record.Record) error

// Journal follows the journal with journalctl and maps entries. The
// cursor is saved in cursorFile so a restart continues where it stopped;
// the first start begins at the end of the journal.
func Journal(cursorFile string, sink Sink, logf func(string, ...any), stop <-chan struct{}) {
	th := &Throttle{Window: time.Minute}
	for {
		args := []string{"--follow", "--output=json", "--no-pager", "--all"}
		if b, err := os.ReadFile(cursorFile); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			args = append(args, "--after-cursor="+strings.TrimSpace(string(b)))
		} else {
			args = append(args, "--lines=0")
		}
		args = append(args, JournalMatches...)
		cmd := exec.Command("journalctl", args...)
		out, err := cmd.StdoutPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err != nil {
			logf("journal collector: %v", err)
		} else {
			done := make(chan struct{})
			go func() {
				select {
				case <-stop:
					_ = cmd.Process.Kill()
				case <-done:
				}
			}()
			readJournal(out, cursorFile, th, sink, logf)
			_ = cmd.Wait()
			close(done)
		}
		select {
		case <-stop:
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func readJournal(r interface{ Read([]byte) (int, error) }, cursorFile string, th *Throttle, sink Sink, logf func(string, ...any)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 256<<10), 8<<20)
	lastSave := time.Time{}
	cursor := ""
	save := func() {
		if cursor == "" {
			return
		}
		tmp := cursorFile + ".tmp"
		if os.WriteFile(tmp, []byte(cursor+"\n"), 0o600) == nil {
			_ = os.Rename(tmp, cursorFile)
		}
	}
	defer save()
	for sc.Scan() {
		var e map[string]any
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		cursor = str(e, "__CURSOR")
		m, ok := FromJournal(e)
		if ok {
			pass, repeats := th.Pass(m.Key, time.Now())
			if pass {
				if repeats > 0 {
					d := m.Record.DataMap()
					d["repeats_before"] = repeats
					m.Record.Data = data(d)
				}
				if err := sink("journal", m.Record); err != nil {
					logf("journal collector: %v", err)
				}
			}
		}
		if time.Since(lastSave) > time.Second {
			save()
			lastSave = time.Now()
		}
	}
}
