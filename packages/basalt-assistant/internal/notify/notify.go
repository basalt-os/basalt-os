// Package notify delivers the assistant's findings beyond the journal: a
// desktop notification (freedesktop org.freedesktop.Notifications) to each
// local graphical session, and an optional webhook signed with HMAC-SHA256.
//
// It reads findings from the journal records the confined daemon writes
// (basalt-assistantd keeps no network access and cannot reach user
// sessions), matched on journald's trusted _SYSTEMD_UNIT field, so a local
// user cannot forge one with logger(1).
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Event is one finding of the daemon.
type Event struct {
	Seq         int64     `json:"seq"`
	Time        time.Time `json:"time"`
	Host        string    `json:"host"`
	Proposal    string    `json:"proposal"`
	Title       string    `json:"title"`
	Kind        string    `json:"kind"`
	Subject     string    `json:"subject"`
	Severity    float64   `json:"severity"`
	NeedsReview bool      `json:"needs_review"`
	Notify      bool      `json:"notify"` // the decision layer chose to notify
	Cursor      string    `json:"-"`
}

// findingData is the part of the audit record's data that matters here.
type findingData struct {
	Proposal    string  `json:"proposal"`
	Title       string  `json:"title"`
	Kind        string  `json:"kind"`
	Subject     string  `json:"subject"`
	Severity    float64 `json:"severity"`
	NeedsReview bool    `json:"needs_review"`
	Notify      bool    `json:"notify"`
}

// ParseEntry reads one line of `journalctl -o json`. ok is false for an
// entry that is not a finding of the daemon.
func ParseEntry(line []byte) (ev Event, ok bool, err error) {
	var e map[string]any
	if err := json.Unmarshal(line, &e); err != nil {
		return ev, false, err
	}
	str := func(k string) string {
		v, _ := e[k].(string)
		return v
	}
	ev.Cursor = str("__CURSOR")
	if str("_SYSTEMD_UNIT") != DaemonUnit || str("BASALT_AUDIT_TYPE") != "finding" {
		return ev, false, nil
	}
	var d findingData
	if err := json.Unmarshal([]byte(str("BASALT_AUDIT_DATA")), &d); err != nil || d.Proposal == "" {
		return ev, false, fmt.Errorf("finding without readable data (cursor %s)", ev.Cursor)
	}
	ev.Seq, _ = strconv.ParseInt(str("BASALT_AUDIT_SEQ"), 10, 64)
	if us, err := strconv.ParseInt(str("__REALTIME_TIMESTAMP"), 10, 64); err == nil {
		ev.Time = time.UnixMicro(us).UTC()
	}
	ev.Host = str("_HOSTNAME")
	ev.Proposal, ev.Title, ev.Kind, ev.Subject = d.Proposal, d.Title, d.Kind, d.Subject
	ev.Severity, ev.NeedsReview, ev.Notify = math.Round(d.Severity*100)/100, d.NeedsReview, d.Notify
	if ev.Title == "" {
		// Older daemons: the text is "<id>: <title>".
		msg := str("MESSAGE")
		if _, t, ok := strings.Cut(msg, ev.Proposal+": "); ok {
			ev.Title = t
		}
	}
	return ev, true, nil
}

// DaemonUnit is the only unit whose findings are delivered.
const DaemonUnit = "basalt-assistantd.service"

// --- webhook ---------------------------------------------------------------------

// Webhook posts events as signed JSON.
type Webhook struct {
	URL     string
	Secret  []byte
	Client  *http.Client
	Retries int           // attempts after the first (default 2)
	Backoff time.Duration // before the first retry, doubled each time (default 2 s)
	Version string        // in the User-Agent
	now     func() time.Time
}

// CheckURL accepts https URLs, and http only to the loopback interface.
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Host == "" {
		return fmt.Errorf("webhook_url %q has no host", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		h := u.Hostname()
		if h == "localhost" {
			return nil
		}
		if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("webhook_url %q: plain http is only allowed to localhost; use https", raw)
	}
	return fmt.Errorf("webhook_url %q: scheme must be https", raw)
}

// ReadSecret reads the HMAC key from a file that only root may read.
func ReadSecret(path string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by others (mode %v); chmod 0600 it", path, st.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	b = bytes.TrimSpace(b)
	if len(b) < 16 {
		return nil, fmt.Errorf("%s: the key is shorter than 16 bytes", path)
	}
	return b, nil
}

// Sign returns the signature header value for a body sent at timestamp ts
// (Unix seconds): "sha256=" + hex(HMAC-SHA256(secret, ts + "." + body)).
func Sign(secret []byte, ts string, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(ts))
	m.Write([]byte{'.'})
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// Payload is the JSON body of a webhook call.
type Payload struct {
	Event   string `json:"event"` // "finding"
	Finding Event  `json:"finding"`
	Hint    string `json:"hint"` // the command that shows the proposal
}

// Send posts one event, retrying on network errors and 5xx answers.
func (w Webhook) Send(ctx context.Context, ev Event) error {
	body, err := json.Marshal(Payload{Event: "finding", Finding: ev, Hint: "basalt show " + ev.Proposal})
	if err != nil {
		return err
	}
	client := w.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	retries, backoff := w.Retries, w.Backoff
	if retries == 0 {
		retries = 2
	}
	if backoff == 0 {
		backoff = 2 * time.Second
	}
	now := w.now
	if now == nil {
		now = time.Now
	}
	var last error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}
		ts := strconv.FormatInt(now().Unix(), 10)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "basalt-notify/"+w.Version)
		req.Header.Set("X-Basalt-Event", "finding")
		req.Header.Set("X-Basalt-Timestamp", ts)
		req.Header.Set("X-Basalt-Signature", Sign(w.Secret, ts, body))
		resp, err := client.Do(req)
		if err != nil {
			last = err
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return nil
		case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
			last = fmt.Errorf("webhook answered %s", resp.Status)
		default:
			return fmt.Errorf("webhook answered %s", resp.Status)
		}
	}
	return last
}

// --- desktop -------------------------------------------------------------------

// Session is a local graphical login session.
type Session struct {
	ID   string
	UID  int
	User string
	Type string // wayland, x11
}

// GraphicalSessions keeps the sessions that can show a notification:
// local (not remote), user class, wayland or x11, active or online. props
// holds `loginctl show-session` properties per session id.
func GraphicalSessions(props map[string]map[string]string) []Session {
	var out []Session
	seen := map[int]bool{}
	for id, p := range props {
		if p["Remote"] == "yes" || !strings.HasPrefix(p["Class"], "user") {
			continue
		}
		if p["Type"] != "wayland" && p["Type"] != "x11" {
			continue
		}
		if p["State"] != "active" && p["State"] != "online" {
			continue
		}
		uid, err := strconv.Atoi(p["User"])
		if err != nil || uid == 0 || seen[uid] {
			continue
		}
		seen[uid] = true
		out = append(out, Session{ID: id, UID: uid, User: p["Name"], Type: p["Type"]})
	}
	return out
}

// ParseProperties reads `loginctl show-session` output (Key=Value lines).
func ParseProperties(out string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			m[k] = v
		}
	}
	return m
}

// Summary and Body are the notification's text.
func Summary(ev Event) string { return "Basalt OS: " + ev.Title }

// Body of the notification.
func Body(ev Event) string {
	s := "Proposal " + ev.Proposal
	if ev.Subject != "" {
		s = ev.Subject + ". " + s
	}
	if ev.NeedsReview {
		s += " needs review"
	}
	return s + ". Details: basalt show " + ev.Proposal
}

// BusctlArgs are the arguments of `busctl --user call` for the freedesktop
// Notify method; severity 4 or more is critical urgency.
func BusctlArgs(ev Event) []string {
	urgency := "1"
	if ev.Severity >= 4 {
		urgency = "2"
	}
	return []string{"--user", "call", "org.freedesktop.Notifications", "/org/freedesktop/Notifications",
		"org.freedesktop.Notifications", "Notify", "susssasa{sv}i",
		"Basalt OS", "0", "dialog-warning", Summary(ev), Body(ev), "0", "1", "urgency", "y", urgency, "-1"}
}

// ErrNothingToDo means no delivery is configured on this machine.
var ErrNothingToDo = errors.New("no desktop session target and no webhook configured: nothing to deliver")
