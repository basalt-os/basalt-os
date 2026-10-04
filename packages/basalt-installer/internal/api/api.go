// Package api serves the installer session over a local Unix socket:
// newline-delimited JSON requests {"id","op","args"}, responses {"id","ok",
// "result"|"error"} and, after "subscribe", events {"event": {...}}. The GUI
// (Quickshell) uses it; the socket accepts only root and the uids listed in
// the options, checked with SO_PEERCRED on every connection.
package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"syscall"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/plan"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/session"
)

// Request is one call.
type Request struct {
	ID   int             `json:"id"`
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args,omitempty"`
}

// Response answers one Request.
type Response struct {
	ID     int    `json:"id"`
	OK     bool   `json:"ok"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Server is the socket server.
type Server struct {
	Session *session.Session
	// AllowUIDs may connect besides root.
	AllowUIDs []int
	// Logf receives connection errors (default: log.Printf).
	Logf func(format string, a ...any)
}

// Listen creates the socket (mode 0660, group gid when >= 0) and serves
// until ctx ends.
func (s *Server) Listen(ctx context.Context, path string, gid int) error {
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer l.Close()
	if err := os.Chmod(path, 0o660); err != nil {
		return err
	}
	if gid >= 0 {
		if err := os.Chown(path, 0, gid); err != nil {
			return err
		}
	}
	go func() { <-ctx.Done(); l.Close() }()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.serve(ctx, c.(*net.UnixConn))
	}
}

func (s *Server) logf(format string, a ...any) {
	if s.Logf != nil {
		s.Logf(format, a...)
		return
	}
	log.Printf(format, a...)
}

// PeerUID returns the uid of the process on the other end of the socket.
func PeerUID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return -1, err
	}
	var cred *syscall.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return -1, err
	}
	if cerr != nil {
		return -1, cerr
	}
	return int(cred.Uid), nil
}

func (s *Server) allowed(uid int) bool {
	if uid == 0 {
		return true
	}
	for _, u := range s.AllowUIDs {
		if u == uid {
			return true
		}
	}
	return false
}

func (s *Server) serve(ctx context.Context, c *net.UnixConn) {
	defer c.Close()
	uid, err := PeerUID(c)
	if err != nil || !s.allowed(uid) {
		s.logf("api: refused connection from uid %d (%v)", uid, err)
		return
	}
	var wmu sync.Mutex
	enc := json.NewEncoder(c)
	send := func(v any) error {
		wmu.Lock()
		defer wmu.Unlock()
		return enc.Encode(v)
	}
	var stop func()
	defer func() {
		if stop != nil {
			stop()
		}
	}()
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		var req Request
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			_ = send(Response{OK: false, Error: "bad request: " + err.Error()})
			continue
		}
		if req.Op == "subscribe" {
			if stop == nil {
				var ch <-chan any
				ch, stop = s.subscribe()
				go func() {
					for e := range ch {
						if send(map[string]any{"event": e}) != nil {
							return
						}
					}
				}()
			}
			_ = send(Response{ID: req.ID, OK: true})
			continue
		}
		result, err := s.handle(ctx, req)
		resp := Response{ID: req.ID, OK: err == nil, Result: result}
		if err != nil {
			resp.Error = err.Error()
		}
		if send(resp) != nil {
			return
		}
	}
}

func (s *Server) subscribe() (<-chan any, func()) {
	ch, stop := s.Session.Subscribe()
	out := make(chan any, 64)
	go func() {
		defer close(out)
		for e := range ch {
			out <- e
		}
	}()
	return out, stop
}

func decode[T any](raw json.RawMessage) (T, error) {
	var v T
	if len(raw) == 0 {
		return v, nil
	}
	err := json.Unmarshal(raw, &v)
	return v, err
}

func (s *Server) handle(ctx context.Context, req Request) (any, error) {
	ss := s.Session
	switch req.Op {
	case "facts":
		return ss.Facts(ctx, true)
	case "suggest":
		p, f, err := ss.Suggest(ctx)
		return map[string]any{"plan": p, "facts": f, "subvolumes": subvolumes()}, err
	case "validate":
		a, err := decode[struct{ Plan plan.Plan }](req.Args)
		if err != nil {
			return nil, err
		}
		is, err := ss.Validate(ctx, a.Plan)
		return map[string]any{"issues": is, "ok": is.Err() == nil}, err
	case "preview":
		a, err := decode[struct{ Plan plan.Plan }](req.Args)
		if err != nil {
			return nil, err
		}
		pv, err := ss.MakePreview(ctx, a.Plan)
		if err != nil {
			return map[string]any{"issues": pv.Issues}, err
		}
		pub := pv.Public()
		return map[string]any{"token": pub.Token, "text": pub.Text, "issues": pub.Issues, "steps": len(pub.Steps),
			"confirm_word": pub.ConfirmWord(), "resolved": pub.Resolved}, nil
	case "install":
		a, err := decode[struct {
			Token   string `json:"token"`
			Confirm string `json:"confirm"`
		}](req.Args)
		if err != nil {
			return nil, err
		}
		return nil, ss.Install(ctx, a.Token, a.Confirm)
	case "cancel":
		return nil, ss.Cancel()
	case "status":
		return ss.Status(), nil
	case "recovery_key":
		k := ss.RecoveryKey()
		if k == "" {
			return nil, errors.New("no recovery key to show")
		}
		return map[string]string{"key": k}, nil
	case "ack_recovery_key":
		a, err := decode[struct {
			Proof string `json:"proof"`
		}](req.Args)
		if err != nil {
			return nil, err
		}
		return nil, ss.AckRecoveryKey(a.Proof)
	case "key_media":
		return ss.KeyMedia(ctx)
	case "save_recovery_key":
		a, err := decode[struct {
			Device string `json:"device"`
		}](req.Args)
		if err != nil {
			return nil, err
		}
		where, err := ss.SaveRecoveryKey(ctx, a.Device)
		if err != nil {
			return nil, err
		}
		return map[string]string{"saved": where}, nil
	case "finish":
		a, err := decode[struct {
			Action string `json:"action"`
		}](req.Args)
		if err != nil {
			return nil, err
		}
		return nil, ss.Finish(ctx, a.Action)
	}
	return nil, fmt.Errorf("unknown op %q", req.Op)
}

func subvolumes() []map[string]any {
	var out []map[string]any
	for _, s := range plan.Subvolumes() {
		out = append(out, map[string]any{"name": s.Name, "mountpoint": s.Mountpoint, "required": s.Required, "purpose": s.Purpose})
	}
	return out
}
