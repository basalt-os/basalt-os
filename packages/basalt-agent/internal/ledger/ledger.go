// Package ledger forwards basalt-agent's audit records to basalt-ledger,
// the system audit service (docs/ledger.md). The session audit log in the
// user's home stays the producer's own chain; the ledger keeps a copy in
// the system chain, with the record's seq and hash as its source position,
// so the two can be compared and the user's copy cannot be quietly
// rewritten.
//
// Sending never blocks the session: records are queued and sent by one
// goroutine; Flush waits a little at the end of a session. When the
// ledger is not installed or not running, records stay in the user's log
// only.
package ledger

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"sync"
	"time"
)

// Socket is the ledger daemon's socket.
var Socket = "/run/basalt-ledger/ledger.sock"

// Client is an asynchronous producer.
type Client struct {
	mu      sync.Mutex
	queue   []any
	wake    chan struct{}
	conn    net.Conn
	rd      *bufio.Reader
	Errors  int
	Refused []string
	busy    bool
}

// New returns a client, or nil when no ledger is installed.
func New() *Client {
	if st, err := os.Stat(Socket); err != nil || st.Mode()&os.ModeSocket == 0 {
		return nil
	}
	c := &Client{wake: make(chan struct{}, 1)}
	go c.loop()
	return c
}

// Send queues a record (any JSON object of schema version 1).
func (c *Client) Send(rec any) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if len(c.queue) < 5000 {
		c.queue = append(c.queue, rec)
	} else {
		c.Errors++
	}
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Flush waits up to d for the queue to drain and closes the connection.
func (c *Client) Flush(d time.Duration) {
	if c == nil {
		return
	}
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		c.mu.Lock()
		n, busy := len(c.queue), c.busy
		c.mu.Unlock()
		if n == 0 && !busy {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.mu.Lock()
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	c.mu.Unlock()
}

func (c *Client) loop() {
	for {
		c.mu.Lock()
		if len(c.queue) == 0 {
			c.mu.Unlock()
			<-c.wake
			continue
		}
		rec := c.queue[0]
		c.queue = c.queue[1:]
		c.busy = true
		c.mu.Unlock()
		err := c.send(rec)
		c.mu.Lock()
		if err != nil {
			c.Errors++
			if c.conn != nil {
				c.conn.Close()
				c.conn = nil
			}
		}
		c.busy = false
		c.mu.Unlock()
	}
}

func (c *Client) send(rec any) error {
	if c.conn == nil {
		conn, err := net.DialTimeout("unix", Socket, 2*time.Second)
		if err != nil {
			return err
		}
		c.mu.Lock()
		c.conn, c.rd = conn, bufio.NewReader(conn)
		c.mu.Unlock()
	}
	_ = c.conn.SetDeadline(time.Now().Add(5 * time.Second))
	b, err := json.Marshal(map[string]any{"op": "append", "record": rec})
	if err != nil {
		return nil
	}
	if _, err := c.conn.Write(append(b, '\n')); err != nil {
		return err
	}
	line, err := c.rd.ReadBytes('\n')
	if err != nil {
		return err
	}
	var rep struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if json.Unmarshal(line, &rep) == nil && !rep.OK {
		c.mu.Lock()
		if len(c.Refused) < 10 {
			c.Refused = append(c.Refused, rep.Error)
		}
		c.mu.Unlock()
	}
	return nil
}
