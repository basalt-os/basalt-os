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
package proxy

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/allowlist"
)

// Decision is reported for every request the proxy decides on.
type Decision struct {
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
	private bool
}

// Server is the filtering proxy.
type Server struct {
	List         *allowlist.List
	Token        string // when set, Proxy-Authorization (Basic, any user, this password) is required
	OnDecision   func(Decision)
	AllowPrivate bool // let allowed names resolve to non-global addresses (tests only)
	DialTimeout  time.Duration
	Resolve      func(ctx context.Context, host string) ([]net.IP, error)

	once      sync.Once
	transport *http.Transport
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
		s.transport = &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
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
			},
			ResponseHeaderTimeout: 60 * time.Second,
			IdleConnTimeout:       30 * time.Second,
		}
	})
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
	for _, h := range []string{"Proxy-Authorization", "Proxy-Connection", "Connection", "Keep-Alive", "Te", "Trailer", "Upgrade"} {
		out.Header.Del(h)
	}
	resp, err := s.transport.RoundTrip(out)
	if err != nil {
		d.Allowed, d.Reason = false, "upstream: "+err.Error()
		s.deny(w, d)
		return
	}
	defer resp.Body.Close()
	s.decide(d)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
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
