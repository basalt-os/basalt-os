package decide

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/llm"
)

// fakeServer answers like llama-server: the sampled token "2" with the
// alternatives given, and records the request.
func fakeServer(t *testing.T, top map[string]float64, got *map[string]any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, got)
		var alts []map[string]any
		for k, v := range top {
			alts = append(alts, map[string]any{"token": k, "logprob": v})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message":  map[string]any{"content": "2"},
				"logprobs": map[string]any{"content": []any{map[string]any{"token": "2", "logprob": top["2"], "top_logprobs": alts}}},
			}},
		})
	}))
}

func TestModelBackendReadsOptionProbabilities(t *testing.T) {
	var req map[string]any
	srv := fakeServer(t, map[string]float64{"2": math.Log(0.6), "1": math.Log(0.2), "\n": math.Log(0.1), "7": math.Log(0.05)}, &req)
	defer srv.Close()
	m := &ModelBackend{C: &llm.Client{Endpoint: srv.URL + "/v1", Model: "test"}}
	a, err := m.Answer(context.Background(), DnfNext("k", map[string]bool{"post_scriptlet_failed": true}))
	if err != nil {
		t.Fatal(err)
	}
	// Options: 1 rollback, 2 investigate. "\n" and "7" are not options.
	if a.Top != "investigate" || math.Abs(a.Probabilities["investigate"]-0.75) > 0.01 || math.Abs(a.Probabilities["rollback"]-0.25) > 0.01 {
		t.Fatalf("answer %+v", a)
	}
	if a.Backend != "model:test" {
		t.Errorf("backend %q", a.Backend)
	}
	if r, _ := m.Answer(context.Background(), RouteBigger("k", nil)); r.Backend != "rules/v1" {
		t.Errorf("routing must stay with the rules: %q", r.Backend)
	}
	// The output was constrained to the option numbers.
	rf, _ := json.Marshal(req["response_format"])
	if !strings.Contains(string(rf), `"enum":[1,2]`) || req["max_tokens"].(float64) != 1 || req["logprobs"] != true {
		t.Errorf("request %v", req)
	}
	msgs, _ := json.Marshal(req["messages"])
	if !strings.Contains(string(msgs), "post_scriptlet_failed") {
		t.Errorf("features not in the prompt: %s", msgs)
	}
}

func TestModelBackendCalibrationAndFloor(t *testing.T) {
	var req map[string]any
	srv := fakeServer(t, map[string]float64{"2": math.Log(0.9), "1": math.Log(0.1)}, &req)
	defer srv.Close()
	m := &ModelBackend{C: &llm.Client{Endpoint: srv.URL + "/v1"}, Calibration: map[string]float64{"disk.cause": 2}}
	a, err := m.Answer(context.Background(), DiskCause("/", nil))
	if err != nil {
		t.Fatal(err)
	}
	// Temperature 2 flattens 0.9/0.1 to 0.75/0.25 between the two seen
	// options; the two unseen ones get a small floor.
	if a.Top != "journal" || a.Probabilities["journal"] > 0.75 || a.Probabilities["other_data"] <= 0 {
		t.Fatalf("answer %+v", a)
	}
}

func TestRemoteEndpointRefused(t *testing.T) {
	l := FromConfigFull(Config{Backend: "openai-compatible", Endpoint: "https://models.example.com/v1"}, nil, nil, 0.75)
	d := l.Ask(context.Background(), DnfNext("k", nil))
	if d.Answer.Backend != "rules/v1 (fallback)" {
		t.Fatalf("a remote endpoint without opt-in must not be used: %+v", d.Answer)
	}
}
