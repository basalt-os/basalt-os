package collect

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func j(kv ...string) map[string]any {
	m := map[string]any{"__REALTIME_TIMESTAMP": "1791100000000000"}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func TestJournalMapping(t *testing.T) {
	cases := []struct {
		name    string
		entry   map[string]any
		event   string
		outcome string
		check   func(t *testing.T, m Mapped)
	}{
		{"avc", j("_TRANSPORT", "audit", "_AUDIT_TYPE", "1400", "MESSAGE",
			`avc:  denied  { read } for  pid=812 comm="cat" name="id_ed25519" dev="vda3" ino=1234 scontext=unconfined_u:unconfined_r:basalt_agent_t:s0:c228,c272 tcontext=unconfined_u:object_r:ssh_home_t:s0 tclass=file permissive=0`),
			"selinux.avc", "denied", func(t *testing.T, m Mapped) {
				d := m.Record.DataMap()
				if m.Record.Subject.Level != "s0:c228,c272" || d["perms"] != "read" || d["comm"] != "cat" || d["tclass"] != "file" || m.Key == "" {
					t.Fatalf("%+v %v", m.Record.Subject, d)
				}
			}},
		{"user avc", j("_TRANSPORT", "audit", "_AUDIT_TYPE", "1107", "MESSAGE",
			`USER_AVC pid=1 uid=0 auid=4294967295 ses=4294967295 subj=system_u:system_r:init_t:s0 msg='avc:  denied  { start } for auid=1000 uid=0 gid=0 path="/x.service" cmdline="" scontext=unconfined_u:unconfined_r:basalt_agent_t:s0:c1,c2 tcontext=system_u:object_r:systemd_unit_file_t:s0 tclass=service permissive=0 exe="/usr/lib/systemd/systemd"'`),
			"selinux.avc", "denied", func(t *testing.T, m Mapped) {
				if m.Record.UID != 1000 || m.Record.DataMap()["perms"] != "start" {
					t.Fatalf("%+v", m.Record)
				}
			}},
		{"avc granted is skipped", j("_TRANSPORT", "audit", "_AUDIT_TYPE", "1400", "MESSAGE", `avc:  granted  { setenforce }`), "", "", nil},
		{"auth failure", j("_TRANSPORT", "audit", "_AUDIT_TYPE", "1100", "MESSAGE",
			`USER_AUTH pid=900 uid=0 auid=4294967295 ses=4294967295 subj=system_u:system_r:sshd_t:s0-s0:c0.c1023 msg='op=PAM:authentication grantors=? acct="dev" exe="/usr/sbin/sshd" hostname=10.86.0.1 addr=10.86.0.1 terminal=ssh res=failed'`),
			"auth.failure", "denied", func(t *testing.T, m Mapped) {
				if d := m.Record.DataMap(); d["acct"] != "dev" || d["addr"] != "10.86.0.1" || m.Record.Subject.App != "sshd" {
					t.Fatalf("%v", d)
				}
			}},
		{"auth success skipped", j("_TRANSPORT", "audit", "_AUDIT_TYPE", "1100", "MESSAGE", `USER_AUTH msg='op=PAM:authentication acct="dev" res=success'`), "", "", nil},
		{"login", j("SYSLOG_IDENTIFIER", "systemd-logind", "MESSAGE_ID", "8d45620c1a4348dbb17410da57c60c66", "USER_ID", "root",
			"SESSION_ID", "12", "MESSAGE", "New session 12 of user root."), "login.session", "ok", func(t *testing.T, m Mapped) {
			if m.Record.UID != 0 || m.Record.DataMap()["login_session"] != "12" {
				t.Fatalf("%+v", m.Record)
			}
		}},
		{"polkit ok", j("SYSLOG_IDENTIFIER", "polkitd", "MESSAGE",
			"Operator of unix-process:2045:39331 successfully authenticated as unix-user:root to gain TEMPORARY authorization for action org.basalt-os.agent.grant for unix-process:2045:39331 [pkexec /usr/libexec/basalt-agent/basalt-agent-grant] (owned by unix-user:root)"),
			"polkit.auth", "ok", func(t *testing.T, m Mapped) {
				if m.Record.DataMap()["action"] != "org.basalt-os.agent.grant" {
					t.Fatalf("%v", m.Record.DataMap())
				}
			}},
		{"polkit failed", j("SYSLOG_IDENTIFIER", "polkitd", "MESSAGE",
			"Operator of unix-session:3 FAILED to authenticate to gain authorization for action org.basalt-os.ledger.read-all for unix-process:1:2 [pkexec] (owned by unix-user:root)"),
			"polkit.auth", "denied", nil},
		{"pkexec", j("SYSLOG_IDENTIFIER", "pkexec", "MESSAGE",
			"root: Executing command [USER=root] [TTY=/dev/pts/0] [CWD=/root] [COMMAND=/usr/bin/basalt-ledger show]"), "escalation.pkexec", "ok",
			func(t *testing.T, m Mapped) {
				if m.Record.DataMap()["command"] != "/usr/bin/basalt-ledger show" {
					t.Fatalf("%v", m.Record.DataMap())
				}
			}},
		{"sudo ok", j("SYSLOG_IDENTIFIER", "sudo", "MESSAGE", "    root : TTY=pts/0 ; PWD=/root ; USER=root ; COMMAND=/usr/bin/id"), "escalation.sudo", "ok", nil},
		{"sudo without a terminal", j("SYSLOG_IDENTIFIER", "sudo", "MESSAGE", "    root : PWD=/root ; USER=root ; COMMAND=/usr/bin/true"), "escalation.sudo", "ok", nil},
		{"sudo wrong password", j("SYSLOG_IDENTIFIER", "sudo", "MESSAGE", "   other : 1 incorrect password attempt ; PWD=/home/other ; USER=root ; COMMAND=/usr/bin/id"), "escalation.sudo", "denied", nil},
		{"sudo denied", j("SYSLOG_IDENTIFIER", "sudo", "MESSAGE", "     root : user NOT in sudoers ; TTY=pts/0 ; PWD=/root ; USER=root ; COMMAND=/usr/bin/id"),
			"escalation.sudo", "denied", nil},
		{"sudo session line skipped", j("SYSLOG_IDENTIFIER", "sudo", "MESSAGE", "pam_unix(sudo:session): session opened for user root(uid=0) by dev(uid=1000)"), "", "", nil},
		{"assistant", j("SYSLOG_IDENTIFIER", "basalt-assistant", "BASALT_AUDIT_TYPE", "apply", "BASALT_AUDIT_SEQ", "41",
			"BASALT_AUDIT_HASH", "ab", "BASALT_AUDIT_PREV", "cd", "BASALT_AUDIT_DATA", `{"proposal":"p-1"}`, "_UID", "0",
			"MESSAGE", "basalt-assistant audit #41 apply: applied p-1: restart nginx"), "assistant.apply", "ok", func(t *testing.T, m Mapped) {
			if m.Record.Src == nil || m.Record.Src.Seq != 41 || m.Record.DataMap()["text"] != "applied p-1: restart nginx" {
				t.Fatalf("%+v", m.Record)
			}
		}},
		{"assistant without audit fields skipped", j("SYSLOG_IDENTIFIER", "basalt-assistant", "MESSAGE", "starting"), "", "", nil},
		{"grant helper", j("SYSLOG_IDENTIFIER", "basalt-agent-grant", "MESSAGE", `granted: uid 1000 session s-0123456789ab host "a.example.com"`),
			"agent.grant.helper", "ok", func(t *testing.T, m Mapped) {
				if m.Record.UID != 1000 || m.Record.Session != "s-0123456789ab" {
					t.Fatalf("%+v", m.Record)
				}
			}},
		{"other program skipped", j("SYSLOG_IDENTIFIER", "sshd", "MESSAGE", "Accepted publickey"), "", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, ok := FromJournal(c.entry)
			if c.event == "" {
				if ok {
					t.Fatalf("mapped: %+v", m.Record)
				}
				return
			}
			if !ok || m.Record.Event != c.event || m.Record.Outcome != c.outcome {
				t.Fatalf("got %v %+v", ok, m.Record)
			}
			if m.Record.Time != "2026-10-04T07:46:40Z" {
				t.Fatalf("time %s", m.Record.Time)
			}
			if c.check != nil {
				c.check(t, m)
			}
		})
	}
}

func TestThrottle(t *testing.T) {
	th := &Throttle{Window: time.Minute}
	now := time.Now()
	if ok, _ := th.Pass("k", now); !ok {
		t.Fatal("first")
	}
	for i := 0; i < 3; i++ {
		if ok, _ := th.Pass("k", now.Add(time.Second)); ok {
			t.Fatal("repeat passed")
		}
	}
	ok, n := th.Pass("k", now.Add(2*time.Minute))
	if !ok || n != 3 {
		t.Fatalf("%v %d", ok, n)
	}
}

func TestSnapshots(t *testing.T) {
	dir := t.TempDir()
	write := func(si SnapshotInfo) {
		d := filepath.Join(dir, itoa(si.Num))
		_ = os.MkdirAll(d, 0o755)
		b, _ := xml.Marshal(struct {
			XMLName xml.Name `xml:"snapshot"`
			SnapshotInfo
		}{SnapshotInfo: si})
		if err := os.WriteFile(filepath.Join(d, "info.xml"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(SnapshotInfo{Type: "pre", Num: 3, Date: "2026-10-04 10:00:00", Description: "dnf install nginx"})
	write(SnapshotInfo{Type: "single", Num: 5, Date: "2026-10-04 11:00:00", Description: "writable copy of #3"})
	snaps := ReadSnapshots(dir)
	if len(snaps) != 2 {
		t.Fatalf("%+v", snaps)
	}
	r := FromSnapshot(snaps[1])
	if r.Event != "snapshot.rollback" || r.DataMap()["target"] != float64(3) || r.Time != "2026-10-04T11:00:00Z" {
		t.Fatalf("%+v %v", r, r.DataMap())
	}
	if FromSnapshot(snaps[0]).Event != "snapshot.create" {
		t.Fatal("pre snapshot")
	}
	// basalt-rollback: both snapshots carry its description; the writable
	// copy (no cleanup algorithm) is the new root.
	if r := FromSnapshot(SnapshotInfo{Num: 21, Type: "single", Description: "basalt-rollback"}); r.Event != "snapshot.rollback" || r.DataMap()["new_root"] != float64(21) {
		t.Fatalf("%+v", r)
	}
	if r := FromSnapshot(SnapshotInfo{Num: 20, Type: "single", Description: "basalt-rollback", Cleanup: "number"}); r.Event != "snapshot.create" {
		t.Fatalf("%+v", r)
	}
}

func itoa(n int) string { return string(rune('0' + n)) }
