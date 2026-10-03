package translate

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestParseValidates(t *testing.T) {
	ok := map[string]string{
		`{"intent":"why","unit":"nginx"}`:                       "why nginx",
		`{"intent":"fix_selinux","since":"3d"}`:                 "fix selinux --since 72h",
		`{"intent":"snapshots","mode":"diff","a":4,"b":0}`:      "snapshots diff 4 0",
		`{"intent":"snapshots","mode":"rollback","a":63,"b":0}`: "snapshots rollback 63",
		`{"intent":"apply","id":"p-97dfa5"}`:                    "apply p-97dfa5",
		`{"intent":"pending","all":true}`:                       "pending --all",
		`{"intent":"none"}`:                                     "",
	}
	for in, want := range ok {
		got, err := Parse(in)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if s := strings.Join(got.Args(), " "); s != want {
			t.Errorf("%s: args %q, want %q", in, s, want)
		}
	}
	bad := []string{
		`{"intent":"why","unit":"nginx; rm -rf /"}`,
		`{"intent":"apply","id":"p-97dfa5 --yes"}`,
		`{"intent":"exec","cmd":"id"}`,
		`{"intent":"snapshots","mode":"rollback","a":0,"b":0}`,
		`{"intent":"fix_selinux","since":"1 hour"}`,
		`not json`,
	}
	for _, in := range bad {
		if _, err := Parse(in); err == nil {
			t.Errorf("accepted %s", in)
		}
	}
}

func TestChangesNeverRunFromTheTranslator(t *testing.T) {
	for _, s := range []string{`{"intent":"apply","id":"p-97dfa5"}`, `{"intent":"snapshots","mode":"rollback","a":5,"b":0}`} {
		in, _ := Parse(s)
		if !in.Changes() {
			t.Errorf("%s must be marked as a change", s)
		}
	}
}

func TestSchemaIsJSON(t *testing.T) {
	b, err := json.Marshal(Schema())
	if err != nil || !strings.Contains(string(b), `"const":"apply"`) {
		t.Fatalf("schema: %v %s", err, b)
	}
}

func TestGround(t *testing.T) {
	cases := []struct {
		text string
		in   Intent
		want string
	}{
		{"aplica a correção", Intent{Intent: Apply, ID: "p-5e5e5e"}, Clarify},
		{"apply p-97dfa5", Intent{Intent: Apply, ID: "p-97dfa5"}, Apply},
		{"ngnix wont start", Intent{Intent: Why, Unit: "nginx"}, Why},
		{"why did the service fail", Intent{Intent: Why, Unit: "service"}, Clarify},
		{"por que falhou?", Intent{Intent: Why, Unit: "firewalld"}, Clarify},
		{"rollback pro 12", Intent{Intent: Snapshots, Mode: "rollback", A: 12}, Snapshots},
		{"rollback to the snapshot", Intent{Intent: Snapshots, Mode: "rollback", A: 1}, Clarify},
		{"why did backup.timer fail", Intent{Intent: Why, Unit: "backup.timer"}, Why},
		{"what did selinux deny today?", Intent{Intent: FixSELinux, Since: "202m"}, FixSELinux},
	}
	for _, c := range cases {
		got, _ := Ground(c.text, c.in)
		if c.in.Intent == FixSELinux && got.Since != "" {
			t.Errorf("%q: invented window %q kept", c.text, got.Since)
		}
		if got.Intent != c.want {
			t.Errorf("%q %+v: got %s, want %s", c.text, c.in, got.Intent, c.want)
		}
	}
}

func TestIntentComesFirst(t *testing.T) {
	var s struct {
		AnyOf []json.RawMessage `json:"anyOf"`
	}
	if err := json.Unmarshal(Schema(), &s); err != nil || len(s.AnyOf) != 10 {
		t.Fatalf("schema: %v", err)
	}
	for _, a := range s.AnyOf {
		if !strings.Contains(string(a), `"properties":{"intent":`) {
			t.Errorf("intent is not the first property: %s", a)
		}
	}
}

// The fine-tuning script must train with exactly the prompt used here.
func TestCompactPromptMatchesTraining(t *testing.T) {
	b, err := os.ReadFile("../../../../eval/tools/train-translator-lora.py")
	if err != nil {
		t.Skip("training script not in this tree:", err)
	}
	s := strings.ReplaceAll(string(b), "\"\n                  \"", "")
	if !strings.Contains(s, CompactPrompt) {
		t.Fatal("CompactPrompt differs from COMPACT_PROMPT in eval/tools/train-translator-lora.py")
	}
}
