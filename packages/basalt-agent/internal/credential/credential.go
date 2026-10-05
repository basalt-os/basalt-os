// Package credential describes how the session proxy uses an API key on
// the agent's behalf, so the key itself never enters the agent session.
//
// A route ties one secret (a variable name such as ANTHROPIC_API_KEY) to
// the provider host it belongs to and the header that carries it. The
// agent gets a placeholder in that variable and a base URL variable
// pointing at plain http://HOST; its requests to HOST go to the session
// proxy as ordinary proxied HTTP (an absolute-URI request, or a CONNECT to
// HOST:80 that the proxy answers itself). The proxy removes every
// credential header the agent sent, adds the real one, and forwards the
// request over TLS to HOST on the route's port, verified against the
// system trust store. Requests to any other host never get a key: their
// credential headers are removed.
//
// Why not TLS interception with a per-session CA: the plain HTTP leg never
// leaves the session (a Unix socket in container mode, a password-protected
// loopback port in native mode), needs no certificate authority the agent
// must trust, and works the same for every client that honors a proxy and
// a base URL (Claude Code, Codex CLI, the Python and Node SDKs). With
// interception, a CA key would exist per session and every TLS stack in
// the image would need to trust it.
package credential

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Placeholder is what the agent sees instead of a key. It is not a secret.
const Placeholder = "basalt-agent-key-injected-by-session-proxy"

// Route says where one secret may be used.
type Route struct {
	Name     string `json:"name"`                // secret variable, e.g. ANTHROPIC_API_KEY
	Host     string `json:"host"`                // provider host the key belongs to
	Port     int    `json:"port"`                // upstream TLS port (443)
	Header   string `json:"header"`              // header that carries the key
	Scheme   string `json:"scheme,omitempty"`    // "Bearer": the header is "Bearer KEY"
	BaseEnv  string `json:"base_env,omitempty"`  // variable that points the agent at http://HOST[BasePath]
	BasePath string `json:"base_path,omitempty"` // path prefix of the base URL ("/v1")
	// Value is the key. Only the launcher and the proxy process hold it; it
	// travels between them on a private pipe and is never logged.
	Value string `json:"value,omitempty"`
}

// HeaderValue is the value of the injected header.
func (r Route) HeaderValue() string {
	if r.Scheme != "" {
		return r.Scheme + " " + r.Value
	}
	return r.Value
}

// BaseURL is the plain-HTTP URL the agent is pointed at. The proxy maps
// it to https://HOST:PORT.
func (r Route) BaseURL() string { return "http://" + r.Host + r.BasePath }

// Upstream is the host:port the proxy connects to.
func (r Route) Upstream() string { return net.JoinHostPort(r.Host, strconv.Itoa(r.Port)) }

// Describe is the route without its value, for audit records.
func (r Route) Describe() map[string]any {
	return map[string]any{"name": r.Name, "host": r.Host, "port": r.Port, "header": r.Header}
}

// Builtin routes for the providers the shipped profiles use. A profile can
// add its own or replace one with "route = ..." in [secrets].
var Builtin = map[string]Route{
	"ANTHROPIC_API_KEY":    {Host: "api.anthropic.com", Header: "X-Api-Key", BaseEnv: "ANTHROPIC_BASE_URL"},
	"ANTHROPIC_AUTH_TOKEN": {Host: "api.anthropic.com", Header: "Authorization", Scheme: "Bearer", BaseEnv: "ANTHROPIC_BASE_URL"},
	"OPENAI_API_KEY":       {Host: "api.openai.com", Header: "Authorization", Scheme: "Bearer", BaseEnv: "OPENAI_BASE_URL", BasePath: "/v1"},
	"GEMINI_API_KEY":       {Host: "generativelanguage.googleapis.com", Header: "X-Goog-Api-Key", BaseEnv: "GOOGLE_GEMINI_BASE_URL"},
	"GOOGLE_API_KEY":       {Host: "generativelanguage.googleapis.com", Header: "X-Goog-Api-Key", BaseEnv: "GOOGLE_GEMINI_BASE_URL"},
	"OPENROUTER_API_KEY":   {Host: "openrouter.ai", Header: "Authorization", Scheme: "Bearer", BaseEnv: "OPENROUTER_API_BASE", BasePath: "/api/v1"},
}

func init() {
	for k, r := range Builtin {
		r.Name, r.Port = k, 443
		Builtin[k] = r
	}
}

// Headers that carry credentials. The proxy removes them from every
// request it can see (plain HTTP), and on a route sets only the route's
// own header.
var commonHeaders = []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Api-Key", "Proxy-Authorization"}

// Headers returns the credential header names to remove: the common ones
// plus those of routes, canonicalized, sorted.
func Headers(routes []Route) []string {
	seen := map[string]bool{}
	for _, h := range commonHeaders {
		seen[canonical(h)] = true
	}
	for _, r := range routes {
		seen[canonical(r.Header)] = true
	}
	out := make([]string, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

func canonical(h string) string {
	// net/textproto canonical form without importing net/http here.
	b := []byte(strings.ToLower(h))
	up := true
	for i, c := range b {
		if up && c >= 'a' && c <= 'z' {
			b[i] = c - 'a' + 'A'
		}
		up = c == '-'
	}
	return string(b)
}

var (
	envRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	tokenRe  = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	schemeRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]{0,31}$`)
	pathRe   = regexp.MustCompile(`^(/[A-Za-z0-9._~-]+)+$`)
	hostRe   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?)+$`)
)

// Parse reads a profile route line:
//
//	NAME HOST[:PORT] header=HEADER [scheme=Bearer] [base_env=VAR] [base_path=/v1]
func Parse(spec string) (Route, error) {
	f := strings.Fields(spec)
	if len(f) < 3 {
		return Route{}, fmt.Errorf("route %q: expected NAME HOST[:PORT] header=HEADER [scheme=S] [base_env=VAR] [base_path=/P]", spec)
	}
	r := Route{Name: f[0], Port: 443}
	if !envRe.MatchString(r.Name) {
		return Route{}, fmt.Errorf("route %q: bad secret name %q", spec, r.Name)
	}
	host, port, hasPort := strings.Cut(f[1], ":")
	r.Host = strings.TrimSuffix(strings.ToLower(host), ".")
	if net.ParseIP(r.Host) != nil || !hostRe.MatchString(r.Host) {
		return Route{}, fmt.Errorf("route %q: host must be a name (no addresses, no wildcards)", spec)
	}
	if hasPort {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return Route{}, fmt.Errorf("route %q: bad port %q", spec, port)
		}
		r.Port = n
	}
	for _, kv := range f[2:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return Route{}, fmt.Errorf("route %q: expected key=value, got %q", spec, kv)
		}
		switch k {
		case "header":
			if !tokenRe.MatchString(v) || strings.EqualFold(v, "Proxy-Authorization") || strings.EqualFold(v, "Host") {
				return Route{}, fmt.Errorf("route %q: bad header %q", spec, v)
			}
			r.Header = canonical(v)
		case "scheme":
			if !schemeRe.MatchString(v) {
				return Route{}, fmt.Errorf("route %q: bad scheme %q", spec, v)
			}
			r.Scheme = v
		case "base_env":
			if !envRe.MatchString(v) {
				return Route{}, fmt.Errorf("route %q: bad variable %q", spec, v)
			}
			r.BaseEnv = v
		case "base_path":
			if !pathRe.MatchString(v) {
				return Route{}, fmt.Errorf("route %q: bad path %q", spec, v)
			}
			r.BasePath = v
		default:
			return Route{}, fmt.Errorf("route %q: unknown key %q", spec, k)
		}
	}
	if r.Header == "" {
		return Route{}, fmt.Errorf("route %q: header= is required", spec)
	}
	return r, nil
}

// String renders a route in the profile format.
func (r Route) String() string {
	s := r.Name + " " + r.Host
	if r.Port != 443 {
		s += ":" + strconv.Itoa(r.Port)
	}
	s += " header=" + r.Header
	if r.Scheme != "" {
		s += " scheme=" + r.Scheme
	}
	if r.BaseEnv != "" {
		s += " base_env=" + r.BaseEnv
	}
	if r.BasePath != "" {
		s += " base_path=" + r.BasePath
	}
	return s
}
