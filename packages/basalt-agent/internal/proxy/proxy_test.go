package proxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/allowlist"
)

// echo starts a TCP server on loopback that answers each line with "echo: line".
func echo(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					fmt.Fprintf(c, "echo: %s\n", sc.Text())
				}
			}()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

type recorder struct {
	mu sync.Mutex
	ds []Decision
}

func (r *recorder) add(d Decision) { r.mu.Lock(); r.ds = append(r.ds, d); r.mu.Unlock() }

func start(t *testing.T, s *Server) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() { _ = s.Serve(l) }()
	return l.Addr().String()
}

func connect(t *testing.T, proxyAddr, target, token string) (string, net.Conn) {
	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if token != "" {
		req += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("basalt:"+token)) + "\r\n"
	}
	fmt.Fprint(c, req+"\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		c.Close()
		return fmt.Sprintf("%d %s", resp.StatusCode, strings.TrimSpace(string(b))), nil
	}
	return "200", &bufConn{Conn: c, r: br}
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func TestConnect(t *testing.T) {
	port := echo(t)
	e, _ := allowlist.ParseEntry("allowed.example.com:" + strconv.Itoa(port))
	rec := &recorder{}
	s := &Server{List: allowlist.New([]allowlist.Entry{e}), Token: "s3cret", OnDecision: rec.add, AllowPrivate: true,
		Resolve: func(_ context.Context, h string) ([]net.IP, error) { return []net.IP{net.ParseIP("127.0.0.1")}, nil }}
	addr := start(t, s)

	// Allowed name: the tunnel works end to end.
	st, c := connect(t, addr, fmt.Sprintf("allowed.example.com:%d", port), "s3cret")
	if st != "200" {
		t.Fatalf("allowed: %s", st)
	}
	fmt.Fprint(c, "hello\n")
	line, _ := bufio.NewReader(c).ReadString('\n')
	c.Close()
	if line != "echo: hello\n" {
		t.Fatalf("tunnel: %q", line)
	}

	for _, tc := range []struct{ target, token, want string }{
		{fmt.Sprintf("other.example.com:%d", port), "s3cret", "403"},
		{"allowed.example.com:22", "s3cret", "403"},
		{fmt.Sprintf("127.0.0.1:%d", port), "s3cret", "403"},
		{fmt.Sprintf("allowed.example.com:%d", port), "wrong", "407"},
		{fmt.Sprintf("allowed.example.com:%d", port), "", "407"},
	} {
		st, _ := connect(t, addr, tc.target, tc.token)
		if !strings.HasPrefix(st, tc.want) {
			t.Errorf("%s (token %q): %s, want %s", tc.target, tc.token, st, tc.want)
		}
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.ds) != 6 || !rec.ds[0].Allowed || rec.ds[1].Allowed {
		t.Errorf("decisions: %+v", rec.ds)
	}
}

func TestPrivateAddressesRefused(t *testing.T) {
	port := echo(t)
	e, _ := allowlist.ParseEntry("rebind.example.com:" + strconv.Itoa(port))
	s := &Server{List: allowlist.New([]allowlist.Entry{e}),
		Resolve: func(_ context.Context, h string) ([]net.IP, error) { return []net.IP{net.ParseIP("127.0.0.1")}, nil }}
	addr := start(t, s)
	st, _ := connect(t, addr, fmt.Sprintf("rebind.example.com:%d", port), "")
	if !strings.HasPrefix(st, "403") || !strings.Contains(st, "local or private") {
		t.Errorf("rebinding to loopback: %s", st)
	}
	for ip, want := range map[string]bool{"8.8.8.8": true, "10.1.2.3": false, "192.168.0.1": false, "169.254.1.1": false,
		"100.64.0.1": false, "::1": false, "fd00::1": false, "2606:4700::1": true, "0.0.0.0": false} {
		if Global(net.ParseIP(ip)) != want {
			t.Errorf("Global(%s) != %v", ip, want)
		}
	}
}

func TestPlainHTTP(t *testing.T) {
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	go func() {
		_ = http.Serve(up, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Proxy-Authorization") != "" {
				w.WriteHeader(500)
				return
			}
			fmt.Fprint(w, "upstream ok")
		}))
	}()
	port := up.Addr().(*net.TCPAddr).Port
	e, _ := allowlist.ParseEntry("plain.example.com:" + strconv.Itoa(port))
	s := &Server{List: allowlist.New([]allowlist.Entry{e}), Token: "t", AllowPrivate: true,
		Resolve: func(_ context.Context, h string) ([]net.IP, error) { return []net.IP{net.ParseIP("127.0.0.1")}, nil }}
	addr := start(t, s)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "GET http://plain.example.com:%d/x HTTP/1.1\r\nHost: plain.example.com\r\nProxy-Authorization: Basic %s\r\nConnection: close\r\n\r\n",
		port, base64.StdEncoding.EncodeToString([]byte("u:t")))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != "upstream ok" {
		t.Errorf("plain http: %d %q", resp.StatusCode, b)
	}
}

func TestPrivateEntry(t *testing.T) {
	port := echo(t)
	e, _ := allowlist.ParseEntry("model.lab.example:" + strconv.Itoa(port) + " private")
	s := &Server{List: allowlist.New([]allowlist.Entry{e}),
		Resolve: func(_ context.Context, h string) ([]net.IP, error) { return []net.IP{net.ParseIP("127.0.0.1")}, nil }}
	addr := start(t, s)
	if st, c := connect(t, addr, fmt.Sprintf("model.lab.example:%d", port), ""); st != "200" {
		t.Errorf("private entry: %s", st)
	} else {
		c.Close()
	}
}
