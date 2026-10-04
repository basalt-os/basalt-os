// Package client talks to a running `basalt-installer serve` over its Unix
// socket. The text installer uses it to follow an installation the engine
// service runs (an unattended installation from the boot menu): progress,
// the recovery key and its acknowledgement.
package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/engine"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/probe"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/session"
)

// DefaultSocket is where `basalt-installer serve` listens on the live image.
const DefaultSocket = "/run/basalt-installer-api/api.sock"

// Client sends one request per connection; Subscribe keeps its own.
type Client struct {
	Socket string
}

type response struct {
	ID     int             `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
}

// Call sends op with args and decodes the result into out (when not nil).
func (c *Client) Call(ctx context.Context, op string, args, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	req := map[string]any{"id": 1, "op": op}
	if args != nil {
		req["args"] = args
	}
	b, _ := json.Marshal(req)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return err
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	for sc.Scan() {
		var r response
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.ID != 1 {
			continue
		}
		if !r.OK {
			return errors.New(r.Error)
		}
		if out != nil && len(r.Result) > 0 {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return errors.New("the installer engine closed the connection")
}

// Wait waits until the socket accepts connections.
func (c *Client) Wait(ctx context.Context) error {
	for {
		var st session.Status
		if err := c.Call(ctx, "status", nil, &st); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Status returns the engine's session state.
func (c *Client) Status(ctx context.Context) (session.Status, error) {
	var st session.Status
	err := c.Call(ctx, "status", nil, &st)
	return st, err
}

// Subscribe streams the session's events (the history first). stop closes
// the connection; the channel is closed when it ends.
func (c *Client) Subscribe(ctx context.Context) (<-chan engine.Event, func(), error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return nil, nil, err
	}
	if _, err := conn.Write([]byte(`{"id":1,"op":"subscribe"}` + "\n")); err != nil {
		conn.Close()
		return nil, nil, err
	}
	ch := make(chan engine.Event, 4096)
	var once sync.Once
	stop := func() { once.Do(func() { conn.Close() }) }
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(conn)
		sc.Buffer(make([]byte, 64*1024), 16<<20)
		for sc.Scan() {
			var msg struct {
				Event *engine.Event `json:"event"`
			}
			if json.Unmarshal(sc.Bytes(), &msg) != nil || msg.Event == nil {
				continue
			}
			ch <- *msg.Event
		}
	}()
	return ch, stop, nil
}

// RecoveryKey returns the recovery key while it is not acknowledged.
func (c *Client) RecoveryKey(ctx context.Context) (string, error) {
	var r struct {
		Key string `json:"key"`
	}
	err := c.Call(ctx, "recovery_key", nil, &r)
	return r.Key, err
}

// AckRecoveryKey confirms the person stored the key (its first group).
func (c *Client) AckRecoveryKey(proof string) error {
	return c.Call(context.Background(), "ack_recovery_key", map[string]string{"proof": proof}, nil)
}

// Cancel stops the running installation.
func (c *Client) Cancel() error { return c.Call(context.Background(), "cancel", nil, nil) }

// Finish runs the end action (after the acknowledgement).
func (c *Client) Finish(ctx context.Context, action string) error {
	return c.Call(ctx, "finish", map[string]string{"action": action}, nil)
}

// KeyMedia lists the removable file systems for a copy of the key.
func (c *Client) KeyMedia(ctx context.Context) ([]probe.KeyMedium, error) {
	var m []probe.KeyMedium
	err := c.Call(ctx, "key_media", nil, &m)
	return m, err
}

// SaveRecoveryKey writes the key to one of them.
func (c *Client) SaveRecoveryKey(ctx context.Context, device string) (string, error) {
	var r struct {
		Saved string `json:"saved"`
	}
	err := c.Call(ctx, "save_recovery_key", map[string]string{"device": device}, &r)
	return r.Saved, err
}
