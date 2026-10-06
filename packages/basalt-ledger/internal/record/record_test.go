package record

import (
	"encoding/json"
	"strings"
	"testing"
)

func mk(event, outcome string, data map[string]any) Record {
	d, _ := json.Marshal(data)
	return Record{Producer: "basalt-agent", UID: 1000, Session: "s-0123456789ab", Event: event, Outcome: outcome,
		Severity: Severity(event, outcome), Subject: Subject{Profile: "claude", Mode: "native", Project: "/home/dev/src/app"}, Data: d}
}

func TestSeverity(t *testing.T) {
	for _, c := range []struct{ ev, out, want string }{
		{"session.start", "ok", "info"}, {"egress.deny", "denied", "warning"}, {"selinux.avc", "denied", "warning"},
		{"snapshot.rollback", "ok", "notice"}, {"polkit.auth", "ok", "notice"}, {"ledger.chain_error", "error", "critical"},
		{"dns.rebinding", "denied", "warning"}, {"egress.grant", "ok", "notice"},
	} {
		if got := Severity(c.ev, c.out); got != c.want {
			t.Errorf("%s/%s: %s, want %s", c.ev, c.out, got, c.want)
		}
	}
}

func TestDescribe(t *testing.T) {
	cases := map[string]Record{
		"Agent claude (s-0123456789ab) was stopped from reaching example.com:443 (not in the session allowlist)": mk("egress.deny", "denied",
			map[string]any{"host": "example.com", "port": 443, "reason": "not in the session allowlist"}),
		"Agent claude (s-0123456789ab) started in native mode on ~/src/app": mk("session.start", "ok", nil),
		"Agent claude (s-0123456789ab) used the key ANTHROPIC_API_KEY on api.anthropic.com (the key stays in the session proxy; the agent never sees it)": mk("credential.use", "ok",
			map[string]any{"credential": "ANTHROPIC_API_KEY", "host": "api.anthropic.com", "port": 443}),
		"Agent claude (s-0123456789ab) sent credential headers (Authorization, X-Api-Key) to example.com:443; they were removed, keys go only to their own provider": mk("credential.strip", "denied",
			map[string]any{"headers": []any{"Authorization", "X-Api-Key"}, "host": "example.com", "port": 443}),
		"Agent claude (s-0123456789ab) tried to connect straight to 1.1.1.1:443 without an allowed name; dropped": mk("egress.drop", "denied", map[string]any{"dst": "1.1.1.1", "port": 443}),
	}
	for want, r := range cases {
		if got := Describe(r); got != want {
			t.Errorf("got  %q\nwant %q", got, want)
		}
	}
	end := mk("session.end", "ok", map[string]any{"duration_s": 125, "exit_code": 0, "egress_allowed": 4, "egress_denied": 1})
	if got := Describe(end); !strings.Contains(got, "after 2m5s") || !strings.Contains(got, "4 connections allowed, 1 refused") {
		t.Errorf("%q", got)
	}
	unknown := mk("custom.thing", "ok", nil)
	if got := Describe(unknown); !strings.Contains(got, "custom.thing") {
		t.Errorf("%q", got)
	}
	avc := Record{Producer: "selinux", Event: "selinux.avc", Outcome: "denied", Data: json.RawMessage(
		`{"comm":"cat","perms":"read","tclass":"file","name":"id_ed25519","scontext":"u:r:basalt_agent_t:s0:c1,c2"}`)}
	if got := Describe(avc); got != "SELinux stopped cat (basalt_agent_t) from read on file id_ed25519" {
		t.Errorf("%q", got)
	}
}

func TestSummarize(t *testing.T) {
	recs := []Record{
		mk("session.start", "ok", nil),
		mk("egress.allow", "allowed", map[string]any{"host": "api.anthropic.com", "port": 443}),
		mk("dns.deny", "denied", map[string]any{"name": "example.org"}),
		mk("egress.drop", "denied", map[string]any{"dst": "1.1.1.1", "port": 443}),
		mk("session.end", "ok", map[string]any{"exit_code": 0}),
		{Producer: "login", Event: "login.session", Severity: "info"},
	}
	s := Summarize(recs)
	if len(s.Sessions) != 1 || s.Sessions[0].Refusals != 2 || len(s.Sessions[0].Reached) != 1 {
		t.Fatalf("%+v", s)
	}
	joined := strings.Join(s.Lines, "\n")
	for _, want := range []string{"Agent claude worked on ~/src/app (native mode", "reached api.anthropic.com", "2 attempts refused (example.org, 1.1.1.1)", "1 logins."} {
		if !strings.Contains(joined, want) {
			t.Errorf("summary lacks %q:\n%s", want, joined)
		}
	}
}

func TestHashStableAcrossRoundTrip(t *testing.T) {
	r := mk("egress.deny", "denied", map[string]any{"n": 123456789, "f": 0.25, "s": "<html>&"})
	r.Hash = HashOf(r)
	b, _ := json.Marshal(r)
	var back Record
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if HashOf(back) != r.Hash {
		t.Fatal("hash changes after a JSON round trip")
	}
}

// Records of basalt-nvidia (the NVIDIA driver of basalt-nonfree).
func TestDriverRecords(t *testing.T) {
	for _, c := range []struct{ ev, out, sev, want string }{
		{"driver.install", "ok", "notice", "The NVIDIA driver was installed; the next start is a trial (snapshot 42 holds the system from before)"},
		{"driver.check", "ok", "notice", "The NVIDIA driver passed its check after the start (GPU 0: NVIDIA GeForce RTX 4060 Laptop GPU)"},
		{"driver.check", "error", "warning", "The NVIDIA driver failed its check after the start: nvidia-smi does not answer"},
		{"driver.fallback", "error", "warning", "The NVIDIA driver was switched off and nouveau is used from the next start: nvidia-smi does not answer"},
		{"driver.kernel_hold", "ok", "warning", "Kernel 7.2.9-200.fc44.x86_64 has no NVIDIA module yet; the computer keeps starting kernel 7.2.8-200.fc44.x86_64"},
		{"driver.kernel_release", "ok", "notice", "Kernel 7.2.9-200.fc44.x86_64 now has its NVIDIA module and is the default again"},
	} {
		r := Record{Producer: "basalt-nvidia", Event: c.ev, Outcome: c.out, Severity: Severity(c.ev, c.out)}
		r.Data, _ = json.Marshal(map[string]any{"snapshot": "42", "gpu": "GPU 0: NVIDIA GeForce RTX 4060 Laptop GPU",
			"reason": "nvidia-smi does not answer", "kernel": "7.2.9-200.fc44.x86_64",
			"default": "7.2.8-200.fc44.x86_64"})
		if r.Severity != c.sev {
			t.Errorf("%s/%s: severity %s, want %s", c.ev, c.out, r.Severity, c.sev)
		}
		if got := Describe(r); got != c.want {
			t.Errorf("got  %q\nwant %q", got, c.want)
		}
	}
}

// The desktop's model downloads (basalt-models): who agreed, what, how
// much, and that the checksum was checked.
func TestDescribeModels(t *testing.T) {
	rec := func(ev, out, data string) Record {
		return Record{Producer: "basalt-models", UID: 1000, Event: ev, Outcome: out, Data: json.RawMessage(data)}
	}
	cases := map[string]Record{
		"basalt agreed to download english (ggml-base.en,ggml-silero-v5.1.2, 148849309 bytes) from the desktop": rec("model.download.request", "allowed",
			`{"kind":"voice","what":"english","models":"ggml-base.en,ggml-silero-v5.1.2","size":"148849309","user":"basalt"}`),
		"english was downloaded and verified against its pinned checksum (ggml-base.en,ggml-silero-v5.1.2, 148849309 bytes), as basalt agreed": rec("model.download", "ok",
			`{"kind":"voice","what":"english","models":"ggml-base.en,ggml-silero-v5.1.2","size":"148849309","user":"basalt"}`),
		"The download of recommended that basalt agreed to failed (network)": rec("model.download", "error",
			`{"kind":"llm","what":"recommended","reason":"network","user":"basalt"}`),
		"uid 1000 asked to download english from the desktop and was refused: model download is turned off (downloads = nobody)": rec("model.download.request", "denied",
			`{"what":"english","reason":"model download is turned off (downloads = nobody)"}`),
	}
	for want, r := range cases {
		if got := Describe(r); got != want {
			t.Errorf("got  %q\nwant %q", got, want)
		}
	}
	if s := Severity("model.download", "ok"); s != "notice" {
		t.Errorf("model.download severity %s", s)
	}
	if s := Severity("model.download", "error"); s != "warning" {
		t.Errorf("failed model.download severity %s", s)
	}
}
