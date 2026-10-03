// Package llm is a minimal client for an OpenAI-compatible chat completions
// endpoint (llama.cpp's llama-server, Ollama, vLLM). It is used by the
// optional language-model features: the natural-language translator and
// the model backend of the decision layer. The model only ever returns
// text constrained by a JSON schema; nothing in this package executes
// anything.
//
// Endpoints:
//
//	unix:/run/basalt-llm/llm.sock      a local socket (the basalt-llm service)
//	http://127.0.0.1:8080/v1           loopback HTTP
//	https://host/v1                    anything else: refused unless AllowRemote
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to one endpoint.
type Client struct {
	Endpoint    string
	Model       string
	AllowRemote bool // remote endpoints are an explicit opt-in (ADR: offline by default)
	Timeout     time.Duration
	APIKey      string // only sent to remote endpoints, never logged

	hc   *http.Client
	base string
}

// ErrRemote is returned for a non-local endpoint without AllowRemote.
var ErrRemote = errors.New("remote model endpoint refused (set allow_remote = yes to opt in)")

// Message is a chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Request is a chat completion request. Fields that a server does not know
// are ignored by llama.cpp, vLLM and Ollama.
type Request struct {
	Messages    []Message
	MaxTokens   int
	Schema      any // JSON schema the output must match (response_format json_schema)
	Grammar     string
	Logprobs    bool
	TopLogprobs int
	Seed        int
}

// TopLogprob is one candidate token at a position.
type TopLogprob struct {
	Token   string  `json:"token"`
	Logprob float64 `json:"logprob"`
}

// TokenLogprob is the sampled token and its alternatives.
type TokenLogprob struct {
	Token       string       `json:"token"`
	Logprob     float64      `json:"logprob"`
	TopLogprobs []TopLogprob `json:"top_logprobs"`
}

// Response is the part of a completion the callers use.
type Response struct {
	Text             string
	Logprobs         []TokenLogprob
	PromptTokens     int
	CompletionTokens int
	Elapsed          time.Duration
	PromptMS         float64 // server timings when reported (llama.cpp)
	PredictedMS      float64
}

func (c *Client) init() error {
	if c.hc != nil {
		return nil
	}
	if c.Timeout == 0 {
		c.Timeout = 60 * time.Second
	}
	ep := strings.TrimRight(c.Endpoint, "/")
	tr := &http.Transport{Proxy: nil, MaxIdleConns: 2, IdleConnTimeout: 30 * time.Second}
	switch {
	case strings.HasPrefix(ep, "unix:"):
		sock := strings.TrimPrefix(ep, "unix:")
		if !strings.HasPrefix(sock, "/") {
			return fmt.Errorf("unix endpoint needs an absolute path: %q", ep)
		}
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}
		c.base = "http://localhost/v1"
	case strings.HasPrefix(ep, "http://") || strings.HasPrefix(ep, "https://"):
		u, err := url.Parse(ep)
		if err != nil {
			return fmt.Errorf("endpoint %q: %v", ep, err)
		}
		if !isLoopback(u.Hostname()) && !c.AllowRemote {
			return ErrRemote
		}
		c.base = ep
	default:
		return fmt.Errorf("endpoint %q: use unix:/path or http(s)://host/v1", ep)
	}
	c.hc = &http.Client{Transport: tr, Timeout: c.Timeout}
	return nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Remote reports whether the endpoint leaves the machine.
func (c *Client) Remote() bool {
	ep := c.Endpoint
	if strings.HasPrefix(ep, "unix:") {
		return false
	}
	u, err := url.Parse(ep)
	return err != nil || !isLoopback(u.Hostname())
}

// Complete sends one chat completion request.
func (c *Client) Complete(ctx context.Context, r Request) (Response, error) {
	if err := c.init(); err != nil {
		return Response{}, err
	}
	body := map[string]any{
		"model":       c.Model,
		"messages":    r.Messages,
		"temperature": 0,
		"top_p":       1,
		"stream":      false,
		// Qwen 3 is a hybrid reasoning model: answer directly.
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
	}
	if c.Model == "" {
		body["model"] = "default"
	}
	if r.MaxTokens > 0 {
		body["max_tokens"] = r.MaxTokens
	}
	if r.Schema != nil {
		body["response_format"] = map[string]any{"type": "json_schema",
			"json_schema": map[string]any{"name": "answer", "strict": true, "schema": r.Schema}}
	}
	if r.Grammar != "" {
		body["grammar"] = r.Grammar
	}
	if r.Logprobs {
		body["logprobs"] = true
		body["top_logprobs"] = r.TopLogprobs
	}
	if r.Seed != 0 {
		body["seed"] = r.Seed
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return Response{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" && c.Remote() {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	t0 := time.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("model endpoint: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Response{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Response{}, fmt.Errorf("model endpoint: HTTP %d: %s", resp.StatusCode, firstLine(string(raw)))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Logprobs *struct {
				Content []TokenLogprob `json:"content"`
			} `json:"logprobs"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Timings struct {
			PromptMS    float64 `json:"prompt_ms"`
			PredictedMS float64 `json:"predicted_ms"`
		} `json:"timings"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Response{}, fmt.Errorf("model endpoint: bad response: %v", err)
	}
	if len(out.Choices) == 0 {
		return Response{}, errors.New("model endpoint: no choices")
	}
	res := Response{Text: out.Choices[0].Message.Content, Elapsed: time.Since(t0),
		PromptTokens: out.Usage.PromptTokens, CompletionTokens: out.Usage.CompletionTokens,
		PromptMS: out.Timings.PromptMS, PredictedMS: out.Timings.PredictedMS}
	if lp := out.Choices[0].Logprobs; lp != nil {
		res.Logprobs = lp.Content
	}
	return res, nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}
