package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/audit"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/decide"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/diag"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/runner"
)

func TestProtocolAndProposeOnly(t *testing.T) {
	dir := t.TempDir()
	fake := &runner.Fake{Default: &runner.Result{}}
	al := audit.New(dir+"/audit.jsonl", "basalt-mcp")
	al.Journal = false
	env := &diag.Env{R: fake, Confined: true, Now: time.Now, Decide: decide.NewRules(al, nil), SnapshotDir: dir,
		Glob: func(string) []string { return nil }}
	s := &Server{Env: env, Store: proposal.Store{Dir: dir + "/proposals"}, Audit: al, Decide: env.Decide, Version: "test"}

	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"basalt_propose_action","arguments":{"kind":"selinux.boolean","params":{"name":"httpd_can_network_connect","value":"on"},"reason":"test"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"basalt_propose_action","arguments":{"kind":"selinux.boolean","params":{"name":"x; reboot","value":"on"},"reason":"bad"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"nope"}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"basalt_propose_action","arguments":{"kind":"file.restore","params":{"path":"/etc/nginx/nginx.conf","snapshot":"27"},"reason":"a client"}}}`,
	}, "\n")
	var out bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	var resps []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatal(err)
		}
		resps = append(resps, m)
	}
	if len(resps) != 6 {
		t.Fatalf("%d responses (notifications get none): %s", len(resps), out.String())
	}
	init := resps[0]["result"].(map[string]any)
	if init["protocolVersion"] != ProtocolVersion {
		t.Errorf("initialize: %v", init)
	}
	tools := resps[1]["result"].(map[string]any)["tools"].([]any)
	reads := 0
	for _, x := range tools {
		tl := x.(map[string]any)
		ann := tl["annotations"].(map[string]any)
		if ann["readOnlyHint"] == true {
			reads++
		} else if !strings.HasPrefix(tl["name"].(string), "basalt_propose_") {
			t.Errorf("write tool %s is not a propose tool", tl["name"])
		}
	}
	if reads < 8 {
		t.Errorf("read tools: %d", reads)
	}
	ok := resps[2]["result"].(map[string]any)
	sc := ok["structuredContent"].(map[string]any)
	if sc["stored"] != true || ok["isError"] == true {
		t.Fatalf("propose: %v", ok)
	}
	text := ok["content"].([]any)[0].(map[string]any)["text"].(string)
	if strings.Contains(text, "--confirm") || !strings.Contains(text, "sudo basalt apply") {
		t.Errorf("the confirmation code must stay with the person at the CLI:\n%s", text)
	}
	p, err := s.Store.Load(sc["id"].(string))
	if err != nil || p.Status != proposal.Pending || !p.NeedsReview {
		t.Fatalf("stored %+v %v", p, err)
	}
	for _, q := range fake.Asked {
		if strings.HasPrefix(q, "setsebool") {
			t.Fatal("an MCP tool executed a change")
		}
	}
	if bad := resps[3]["result"].(map[string]any); bad["isError"] != true {
		t.Errorf("invalid action accepted: %v", bad)
	}
	if resps[4]["error"] == nil {
		t.Error("unknown method")
	}
	// A file restore through MCP (the confined view) is stored as a hint.
	hint := resps[5]["result"].(map[string]any)
	hs := hint["structuredContent"].(map[string]any)
	if hint["isError"] == true || hs["stored"] != true || hs["hint"] != true {
		t.Fatalf("restore through MCP: %v", hint)
	}
	hp, err := s.Store.Load(hs["id"].(string))
	if err != nil || len(hp.Actions) != 0 || len(hp.Hints) != 1 || hp.Hints[0].Actions[0].Kind != "file.restore" {
		t.Fatalf("stored %+v %v", hp, err)
	}
	if text := hint["content"].([]any)[0].(map[string]any)["text"].(string); !strings.Contains(text, "basalt confirm "+hp.ID) {
		t.Errorf("hint text:\n%s", text)
	}
	if n, err := audit.Verify(dir + "/audit.jsonl"); err != nil || n == 0 {
		t.Errorf("audit: %d %v", n, err)
	}
}
