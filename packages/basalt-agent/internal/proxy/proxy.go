// Package proxy is basalt-agent's per-session egress proxy: an HTTP proxy
// (CONNECT tunnels and plain absolute-URI requests) that lets a session
// reach only the names in its allowlist.
//
// It runs as its own process in the SELinux domain basalt_agent_proxy_t,
// the only part of a session that may resolve names and open connections
// to the network. The agent reaches it on a Unix socket (container mode,
// the container has no network at all) or on a loopback TCP port with a
// per-session password (native mode, where SELinux lets the agent connect
// to nothing but the proxy port type).
//
// Rules on top of the allowlist: IP literals are refused (entries are
// names), and a name that resolves only to loopback, private, link-local
// or other non-global addresses is refused, so an allowed name cannot be
// pointed at a local service (DNS rebinding), unless its entry is marked
// "private" (a local model server).
//
// Credentials: the proxy holds the session's API keys; the agent never
// does (internal/credential). A plain-HTTP request to a route's host
// (absolute URI, or inside a CONNECT to HOST:80, which the proxy answers
// itself) has every credential header removed, the route's header set to
// the real key, and is forwarded over verified TLS to HOST on the route's
// port. Plain-HTTP requests to any other host lose their credential
// headers, and are refused if they carry a session key anywhere in their
// headers or URL. TLS tunnels (CONNECT to port 443) are passed through
// untouched and never get a key.
package proxy

import (
	"bufio"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/allowlist"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/credential"
)

// Decision is reported for every request the proxy decides on. It never
// carries a credential value: Credential is the secret's name, Stripped
// the names of headers removed.
type Decision struct {
	Host       string   `json:"host"`
	Port       int      `json:"port"`
	Allowed    bool     `json:"allowed"`
	Reason     string   `json:"reason,omitempty"`
	Credential string   `json:"credential,omitempty"`
	Stripped   []string `json:"stripped,omitempty"`
	private    bool
}

// Server is the filtering proxy.
type Server struct {
	List         *allowlist.List
	Token        string // when set, Proxy-Authorization (Basic, any user, this password) is required
	OnDecision   func(Decision)
	AllowPrivate bool // let allowed names resolve to non-global addresses (tests only)
	DialTimeout  time.Duration
	Resolve      func(ctx context.Context, host string) ([]net.IP, error)
	// Routes are the session's credentials, at most one per host.
	Routes []credential.Route
	// UpstreamTLS overrides the TLS settings toward route hosts (tests: a
	// private root). Nil means the system trust store.
	UpstreamTLS *tls.Config

	once      sync.Once
	transport *http.Transport // plain http:// to allowed hosts
	routeTr   *http.Transport // https:// to route hosts
	routes    map[string]credential.Route
	strip     []string
	values    []string
}

func (s *Server) init() {
	s.once.Do(func() {
		if s.DialTimeout == 0 {
			s.DialTimeout = 15 * time.Second
		}
		if s.Resolve == nil {
			s.Resolve = func(ctx context.Context, host string) ([]net.IP, error) {
				return net.DefaultResolver.LookupIP(ctx, "ip", host)
			}
		}
		dial := func(ctx context.Context, _, addr string) (net.Conn, error) {
			h, p, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			port, _ := strconv.Atoi(p)
			d := s.Check(h, port)
			if !d.Allowed {
				return nil, errors.New(d.Reason)
			}
			return s.dial(ctx, d.Host, port, d.private)
		}
		s.transport = &http.Transport{
			Proxy:                 nil,
			DialContext:           dial,
			DisableCompression:    true,
			ResponseHeaderTimeout: 60 * time.Second,
			IdleConnTimeout:       30 * time.Second,
		}
		tc := &tls.Config{MinVersion: tls.VersionTLS12}
		if s.UpstreamTLS != nil {
			tc = s.UpstreamTLS.Clone()
		}
		// Model APIs can take minutes before the first byte of a
		// non-streaming answer.
		s.routeTr = &http.Transport{
			Proxy:                 nil,
			DialContext:           dial,
			TLSClientConfig:       tc,
			ForceAttemptHTTP2:     false,
			DisableCompression:    true,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 10 * time.Minute,
			IdleConnTimeout:       90 * time.Second,
		}
		s.routes = map[string]credential.Route{}
		for _, r := range s.Routes {
			h := allowlist.Normalize(r.Host)
			if _, dup := s.routes[h]; dup || r.Value == "" {
				continue
			}
			s.routes[h] = r
			s.values = append(s.values, r.Value)
		}
		s.strip = credential.Headers(s.Routes)
	})
}

// route returns the credential route for host, if any.
func (s *Server) route(host string) (credential.Route, bool) {
	r, ok := s.routes[allowlist.Normalize(host)]
	return r, ok
}

// Serve accepts proxy connections on l until it is closed.
func (s *Server) Serve(l net.Listener) error {
	s.init()
	srv := &http.Server{Handler: s, ReadHeaderTimeout: 30 * time.Second}
	return srv.Serve(l)
}

func (s *Server) decide(d Decision) {
	if s.OnDecision != nil {
		s.OnDecision(d)
	}
}

// Check applies the allowlist to host:port.
func (s *Server) Check(host string, port int) Decision {
	host = allowlist.Normalize(host)
	d := Decision{Host: host, Port: port}
	switch {
	case net.ParseIP(strings.Trim(host, "[]")) != nil:
		d.Reason = "IP address (only names can be allowed)"
	default:
		d.Allowed, d.private = s.List.Lookup(host, port)
		if !d.Allowed {
			d.Reason = "not in the session allowlist"
		}
	}
	return d
}

func (s *Server) authorized(r *http.Request) bool {
	if s.Token == "" {
		return true
	}
	h := r.Header.Get("Proxy-Authorization")
	enc, ok := strings.CutPrefix(h, "Basic ")
	if !ok {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(enc))
	if err != nil {
		return false
	}
	_, pass, _ := strings.Cut(string(raw), ":")
	return subtle.ConstantTimeCompare([]byte(pass), []byte(s.Token)) == 1
}

// ServeHTTP handles CONNECT and absolute-URI requests.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.init()
	if !s.authorized(r) {
		w.Header().Set("Proxy-Authenticate", `Basic realm="basalt-agent"`)
		http.Error(w, "basalt-agent: proxy authentication required", http.StatusProxyAuthRequired)
		s.decide(Decision{Host: hostOnly(r.Host), Reason: "proxy authentication failed"})
		return
	}
	if r.Method == http.MethodConnect {
		s.connect(w, r)
		return
	}
	if !r.URL.IsAbs() || r.URL.Scheme != "http" {
		http.Error(w, "basalt-agent: only CONNECT and http:// requests are proxied", http.StatusBadRequest)
		return
	}
	if rt, ok := s.route(r.URL.Hostname()); ok {
		s.forwardRoute(w, r, rt)
		return
	}
	port := 80
	if p := r.URL.Port(); p != "" {
		port, _ = strconv.Atoi(p)
	}
	d := s.Check(r.URL.Hostname(), port)
	if !d.Allowed {
		s.deny(w, d)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	dropHopByHop(out.Header)
	// No credential goes to a host it does not belong to: credential
	// headers are removed, and a request carrying a session key anywhere
	// (another header, the URL) is refused.
	d.Stripped = s.stripCredentials(out.Header)
	if s.carriesKey(out) {
		d.Allowed, d.Reason = false, "the request carries a session credential"
		s.deny(w, d)
		return
	}
	resp, err := s.transport.RoundTrip(out)
	if err != nil {
		d.Allowed, d.Reason = false, "upstream: "+err.Error()
		s.deny(w, d)
		return
	}
	defer resp.Body.Close()
	s.decide(d)
	copyResponse(w, resp)
}

var hopByHop = []string{"Proxy-Authorization", "Proxy-Connection", "Connection", "Keep-Alive", "Te", "Trailer", "Upgrade", "Transfer-Encoding"}

func dropHopByHop(h http.Header) {
	// Headers named in Connection are hop-by-hop too.
	for _, v := range h.Values("Connection") {
		for _, n := range strings.Split(v, ",") {
			if n = strings.TrimSpace(n); n != "" {
				h.Del(n)
			}
		}
	}
	for _, n := range hopByHop {
		h.Del(n)
	}
}

// stripCredentials removes every credential header and returns the names
// it removed (sorted).
func (s *Server) stripCredentials(h http.Header) []string {
	var out []string
	for _, n := range s.strip {
		if _, ok := h[n]; ok {
			out = append(out, n)
			h.Del(n)
		}
	}
	sort.Strings(out)
	return out
}

// carriesKey reports whether a session key appears verbatim in the
// request's URL or headers. The agent never has a key, so this is a second
// line: it catches a key that reached the session some other way.
func (s *Server) carriesKey(r *http.Request) bool {
	if len(s.values) == 0 {
		return false
	}
	hay := []string{r.URL.String(), r.Host}
	if u, err := url.QueryUnescape(r.URL.RawQuery); err == nil {
		hay = append(hay, u)
	}
	for k, vs := range r.Header {
		hay = append(hay, k)
		hay = append(hay, vs...)
	}
	for _, v := range s.values {
		for _, h := range hay {
			if strings.Contains(h, v) {
				return true
			}
		}
	}
	return false
}

// forwardRoute sends a plain-HTTP request for a route's host to the
// provider over TLS with the real credential. The target is the route's
// host and port, whatever the request's Host header or port said.
func (s *Server) forwardRoute(w http.ResponseWriter, r *http.Request, rt credential.Route) {
	d := s.Check(rt.Host, rt.Port)
	d.Credential = rt.Name
	if !d.Allowed {
		s.deny(w, d)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL = &url.URL{Scheme: "https", Host: rt.Host, Path: r.URL.Path, RawPath: r.URL.RawPath, RawQuery: r.URL.RawQuery}
	if rt.Port != 443 {
		out.URL.Host = rt.Upstream()
	}
	out.Host = ""
	dropHopByHop(out.Header)
	s.stripCredentials(out.Header)
	out.Header.Set(rt.Header, rt.HeaderValue())
	resp, err := s.routeTr.RoundTrip(out)
	if err != nil {
		d.Allowed, d.Reason = false, "upstream: "+err.Error()
		s.deny(w, d)
		return
	}
	defer resp.Body.Close()
	s.decide(d)
	copyResponse(w, resp)
}

// copyResponse writes resp to w, flushing as data arrives (streamed model
// answers, server-sent events).
func copyResponse(w http.ResponseWriter, resp *http.Response) {
	dropHopByHop(resp.Header)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			_ = rc.Flush()
		}
		if err != nil {
			return
		}
	}
}

func (s *Server) deny(w http.ResponseWriter, d Decision) {
	s.decide(d)
	http.Error(w, fmt.Sprintf("basalt-agent: %s:%d refused: %s", d.Host, d.Port, d.Reason), http.StatusForbidden)
}

func hostOnly(hp string) string {
	if h, _, err := net.SplitHostPort(hp); err == nil {
		return h
	}
	return hp
}

func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	h, p, err := net.SplitHostPort(r.Host)
	port, perr := strconv.Atoi(p)
	if err != nil || perr != nil {
		http.Error(w, "basalt-agent: CONNECT needs host:port", http.StatusBadRequest)
		return
	}
	// A client that tunnels plain HTTP (CONNECT HOST:80, as Node's fetch
	// does for http:// URLs) to a route's host: the proxy is the other end
	// of the tunnel and serves the requests itself, with the credential.
	if rt, ok := s.route(h); ok && port == 80 {
		s.serveRouteTunnel(w, rt)
		return
	}
	d := s.Check(h, port)
	if !d.Allowed {
		s.deny(w, d)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.DialTimeout)
	up, err := s.dial(ctx, d.Host, port, d.private)
	cancel()
	if err != nil {
		d.Allowed, d.Reason = false, err.Error()
		s.deny(w, d)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	s.decide(d)
	_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	go pipe(up, client, buf.Reader)
}

// serveRouteTunnel answers a CONNECT to a route's host on port 80 and
// serves the plain HTTP requests sent through it as route requests.
func (s *Server) serveRouteTunnel(w http.ResponseWriter, rt credential.Route) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		client.Close()
		return
	}
	c := &bufferedConn{Conn: client, r: buf.Reader}
	l := newOneConnListener(c)
	srv := &http.Server{
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       90 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodConnect {
				http.Error(w, "basalt-agent: CONNECT inside a tunnel", http.StatusBadRequest)
				return
			}
			s.forwardRoute(w, r, rt)
		}),
	}
	go func() { _ = srv.Serve(l) }()
}

// bufferedConn reads first what the CONNECT request's reader had buffered.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// oneConnListener hands out one connection, then ends when it is closed.
type oneConnListener struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
	addr net.Addr
}

type closeNotifyConn struct {
	net.Conn
	l    *oneConnListener
	once sync.Once
}

func (c *closeNotifyConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.l.Close() })
	return err
}

func newOneConnListener(c net.Conn) *oneConnListener {
	l := &oneConnListener{ch: make(chan net.Conn, 1), done: make(chan struct{}), addr: c.LocalAddr()}
	l.ch <- &closeNotifyConn{Conn: c, l: l}
	return l
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *oneConnListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return l.addr }

func pipe(up, client net.Conn, br *bufio.Reader) {
	done := make(chan struct{}, 2)
	go func() {
		// Bytes the client sent after the CONNECT line may be buffered.
		if n := br.Buffered(); n > 0 {
			b, _ := br.Peek(n)
			_, _ = up.Write(b)
		}
		_, _ = io.Copy(up, client)
		closeWrite(up)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, up)
		closeWrite(client)
		done <- struct{}{}
	}()
	<-done
	<-done
	up.Close()
	client.Close()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// Global reports whether ip is a public unicast address.
func Global(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() ||
		ip.IsMulticast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	// Shared address space (RFC 6598) and the IPv4-mapped/compatible forms.
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 100 && v4[1]&0xc0 == 64 || v4[0] == 0 || v4[0] >= 240 {
			return false
		}
	}
	return true
}

// dial connects to host (an allowed name) on port, refusing non-global addresses.
func (s *Server) dial(ctx context.Context, host string, port int, private bool) (net.Conn, error) {
	ips, err := s.Resolve(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve: %w", err)
	}
	var errs []error
	tried := 0
	for _, ip := range ips {
		if !s.AllowPrivate && !private && !Global(ip) {
			continue
		}
		tried++
		dl := net.Dialer{Timeout: s.DialTimeout}
		c, err := dl.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), strconv.Itoa(port)))
		if err == nil {
			return c, nil
		}
		errs = append(errs, err)
	}
	if tried == 0 {
		return nil, errors.New("name resolves only to local or private addresses")
	}
	return nil, errors.Join(errs...)
}
