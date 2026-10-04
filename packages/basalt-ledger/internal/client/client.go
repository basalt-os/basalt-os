// Package client talks to the basalt-ledger daemon over its socket (the
// JSON API in docs/ledger.md): one JSON request per line, one reply per
// line, any number of requests per connection.
package client

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/server"
)

// DefaultSocket is the ledger daemon's socket.
const DefaultSocket = "/run/basalt-ledger/ledger.sock"

// Client is one connection.
type Client struct {
	c  net.Conn
	br *bufio.Reader
}

// Dial connects to socket.
func Dial(socket string) (*Client, error) {
	c, err := net.DialTimeout("unix", socket, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("the ledger is not reachable at %s: %w", socket, err)
	}
	return &Client{c: c, br: bufio.NewReaderSize(c, 1<<20)}, nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.c.Close() }

// Do sends one request and reads its reply.
func (c *Client) Do(req server.Request) (server.Reply, error) {
	var rep server.Reply
	b, err := json.Marshal(req)
	if err != nil {
		return rep, err
	}
	_ = c.c.SetDeadline(time.Now().Add(2 * time.Minute))
	if _, err := c.c.Write(append(b, '\n')); err != nil {
		return rep, err
	}
	line, err := c.br.ReadBytes('\n')
	if err != nil {
		return rep, err
	}
	return rep, json.Unmarshal(line, &rep)
}

// Raw sends a raw request line and returns the raw reply line.
func (c *Client) Raw(line []byte) ([]byte, error) {
	_ = c.c.SetDeadline(time.Now().Add(2 * time.Minute))
	if _, err := c.c.Write(append(line, '\n')); err != nil {
		return nil, err
	}
	return c.br.ReadBytes('\n')
}
