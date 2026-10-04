// Package ledger sends records to basalt-ledger, the system audit service
// (docs/ledger.md), over its Unix socket. Records use the shared schema
// version 1 (the one basalt-agent writes); the ledger fills in its own
// sequence, chain and the peer's identity.
//
// The client never blocks its caller: records go into a bounded queue
// that a background goroutine sends in order, reconnecting as needed.
// When the ledger is down, records wait in the queue; if the queue fills,
// the oldest are dropped and counted, and the count is sent as its own
// record once the ledger is reachable again.
package ledger

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// DefaultSocket is the ledger's socket.
const DefaultSocket = "/run/basalt-ledger/ledger.sock"

// Subject identifies the confined session or app an event belongs to.
type Subject struct {
	Profile string `json:"profile,omitempty"`
	Mode    string `json:"mode,omitempty"`
	Level   string `json:"level,omitempty"`
	Project string `json:"project,omitempty"`
	App     string `json:"app,omitempty"`
}

// Record is a schema version 1 record as a producer sends it.
type Record struct {
	V        int            `json:"v"`
	Time     string         `json:"time"`
	Producer string         `json:"producer"`
	UID      int            `json:"uid"`
	Session  string         `json:"session,omitempty"`
	Event    string         `json:"event"`
	Outcome  string         `json:"outcome"`
	Subject  Subject        `json:"subject"`
	Data     map[string]any `json:"data,omitempty"`
}

type request struct {
	Op     string `json:"op"`
	Record Record `json:"record"`
}

type reply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Seq   int64  `json:"seq,omitempty"`
}

// Client is an asynchronous ledger producer.
type Client struct {
	Socket   string
	Producer string

	mu      sync.Mutex
	queue   []Record
	dropped int
	wake    chan struct{}
	max     int
	conn    net.Conn
	rd      *bufio.Reader
	errLog  func(string)
	lastErr time.Time
}

// New starts a client sending to socket as producer. logf (may be nil)
// receives connection problems, at most once a minute.
func New(socket, producer string, logf func(string)) *Client {
	c := &Client{Socket: socket, Producer: producer, wake: make(chan struct{}, 1), max: 10000, errLog: logf}
	go c.loop()
	return c
}

// Send queues a record; the envelope (version, time, producer) is filled.
func (c *Client) Send(r Record) {
	r.V, r.Producer = 1, c.Producer
	if r.Time == "" {
		r.Time = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if r.Outcome == "" {
		r.Outcome = "ok"
	}
	c.mu.Lock()
	if len(c.queue) >= c.max {
		c.queue = c.queue[1:]
		c.dropped++
	}
	c.queue = append(c.queue, r)
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Pending returns the number of queued records.
func (c *Client) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.queue)
}

// Flush waits up to d for the queue to drain.
func (c *Client) Flush(d time.Duration) bool {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if c.Pending() == 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return c.Pending() == 0
}

func (c *Client) loop() {
	for {
		c.mu.Lock()
		if c.dropped > 0 {
			d := c.dropped
			c.dropped = 0
			c.queue = append([]Record{{V: 1, Producer: c.Producer, Time: time.Now().UTC().Format(time.RFC3339Nano),
				Event: "ledger.dropped", Outcome: "error", Data: map[string]any{"records": d,
					"reason": "the ledger was unreachable and the producer queue was full"}}}, c.queue...)
		}
		if len(c.queue) == 0 {
			c.mu.Unlock()
			select {
			case <-c.wake:
			case <-time.After(5 * time.Second):
			}
			continue
		}
		r := c.queue[0]
		c.mu.Unlock()
		if err := c.send(r); err != nil {
			c.closeConn()
			c.report(err)
			select {
			case <-c.wake:
			case <-time.After(2 * time.Second):
			}
			continue
		}
		c.mu.Lock()
		if len(c.queue) > 0 {
			c.queue = c.queue[1:]
		}
		c.mu.Unlock()
	}
}

func (c *Client) report(err error) {
	if c.errLog == nil || time.Since(c.lastErr) < time.Minute {
		return
	}
	c.lastErr = time.Now()
	c.errLog(fmt.Sprintf("ledger %s: %v (records are queued)", c.Socket, err))
}

func (c *Client) closeConn() {
	if c.conn != nil {
		c.conn.Close()
		c.conn, c.rd = nil, nil
	}
}

// send writes one record and reads the ledger's reply. A refusal is not
// retried (the record is dropped and the reason reported).
func (c *Client) send(r Record) error {
	if c.conn == nil {
		conn, err := net.DialTimeout("unix", c.Socket, 2*time.Second)
		if err != nil {
			return err
		}
		c.conn, c.rd = conn, bufio.NewReader(conn)
	}
	_ = c.conn.SetDeadline(time.Now().Add(5 * time.Second))
	b, err := json.Marshal(request{Op: "append", Record: r})
	if err != nil {
		return nil // unencodable: drop
	}
	if _, err := c.conn.Write(append(b, '\n')); err != nil {
		return err
	}
	line, err := c.rd.ReadBytes('\n')
	if err != nil {
		return err
	}
	var rep reply
	if json.Unmarshal(line, &rep) != nil {
		return errors.New("unreadable reply")
	}
	if !rep.OK && c.errLog != nil {
		c.errLog("ledger refused a record: " + rep.Error)
	}
	return nil
}
