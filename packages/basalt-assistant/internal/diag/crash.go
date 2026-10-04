package diag

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// CrashInfo tells a crash that is likely to pass with a restart (a first
// crash after the service ran for a while) from one that is not: a unit
// that crashes again and again, or right as it starts, would crash the same
// way after a restart, and the restart would then fail its verification.
type CrashInfo struct {
	// Code and Status of the main process: killed, dumped or exited, and
	// the signal or exit status ("11/SEGV").
	Code   string `json:"code,omitempty"`
	Status string `json:"status,omitempty"`
	// Count is how many times the unit ended this same way in the lookback
	// window (this crash included).
	Count int `json:"count"`
	// Window is the lookback the count covers.
	Window string `json:"window"`
	// Restarts is how often systemd restarted the unit (NRestarts, or the
	// restart counter in the journal).
	Restarts int `json:"restarts,omitempty"`
	// StartLimit: systemd gave up restarting it (start-limit-hit).
	StartLimit bool `json:"start_limit,omitempty"`
	// RanFor is how long the main process ran before it ended ("" when
	// unknown); RanSeconds the same in seconds (-1 when unknown).
	RanFor     string `json:"ran_for,omitempty"`
	RanSeconds int64  `json:"ran_seconds"`
	// Repeating: the crash is not plausibly transient; no restart.
	Repeating bool `json:"repeating"`
	// Reasons say why it is (or is not) repeating, in plain words.
	Reasons []string `json:"reasons,omitempty"`
}

// CrashLookback is how far back the journal is read for earlier crashes.
const CrashLookback = 24 * time.Hour

// QuickCrash: a main process that ends this soon after it started crashes
// on its way up, which a restart repeats.
const QuickCrash = 30 * time.Second

var (
	reMainExited    = regexp.MustCompile(`Main process exited, code=(\w+), status=(\d+)(?:/(\w+))?`)
	reRestartCount  = regexp.MustCompile(`restart counter is at (\d+)`)
	reStartLimitHit = regexp.MustCompile(`(?i)start request repeated too quickly|result 'start-limit-hit'`)
)

// exitCodes maps systemd's ExecMainCode (the siginfo si_code) to the word
// its journal messages use.
var exitCodes = map[string]string{"1": "exited", "2": "killed", "3": "dumped"}

// unitCrash reads the unit's recent history (journal, restart counters,
// how long the process ran) and decides whether the crash repeats.
func (e *Env) unitCrash(ctx context.Context, rep *UnitReport) {
	st := rep.State
	c := &CrashInfo{Code: exitCodes[strings.TrimSpace(st["ExecMainCode"])], Status: strings.TrimSpace(st["ExecMainStatus"]),
		Window: "24 hours", RanSeconds: -1}
	since := e.now().Add(-CrashLookback)
	res := e.R.Read(ctx, "journalctl", "--no-pager", "-o", "cat", "-u", rep.Unit,
		"--since", "@"+strconv.FormatInt(since.Unix(), 10), "-n", "2000")
	name := ""
	for _, l := range strings.Split(res.Out, "\n") {
		if m := reMainExited.FindStringSubmatch(l); m != nil {
			if c.Code == "" {
				c.Code = m[1]
			}
			if c.Status == "" || c.Status == "0" {
				c.Status = m[2]
			}
			if m[1] == c.Code && m[2] == c.Status {
				c.Count++
				if m[3] != "" {
					name = m[3]
				}
			}
		}
		if m := reRestartCount.FindStringSubmatch(l); m != nil {
			if n, _ := strconv.Atoi(m[1]); n > c.Restarts {
				c.Restarts = n
			}
		}
		if reStartLimitHit.MatchString(l) {
			c.StartLimit = true
		}
	}
	if c.Count == 0 {
		c.Count = 1 // this crash, when the journal does not show it
	}
	if name != "" {
		c.Status += "/" + name
	}
	if n, err := strconv.Atoi(strings.TrimSpace(st["NRestarts"])); err == nil && n > c.Restarts {
		c.Restarts = n
	}
	if st["Result"] == "start-limit-hit" {
		c.StartLimit = true
	}
	start, end := unixTS(st["ExecMainStartTimestamp"]), unixTS(st["ExecMainExitTimestamp"])
	if !start.IsZero() && !end.IsZero() && !end.Before(start) {
		d := end.Sub(start)
		c.RanSeconds = int64(d / time.Second)
		c.RanFor = humanDuration(d)
	}

	how := strings.TrimSpace(c.Code + " " + c.Status)
	if c.StartLimit {
		c.Reasons = append(c.Reasons, "systemd stopped restarting it because it failed too often in a short time (start limit)")
	}
	if c.Restarts > 0 {
		c.Reasons = append(c.Reasons, fmt.Sprintf("systemd already restarted it %d times and it crashed again", c.Restarts))
	}
	if c.Count >= 2 {
		c.Reasons = append(c.Reasons, fmt.Sprintf("it ended the same way (%s) %d times in the last %s", how, c.Count, c.Window))
	}
	if c.RanSeconds >= 0 && time.Duration(c.RanSeconds)*time.Second < QuickCrash {
		c.Reasons = append(c.Reasons, fmt.Sprintf("it crashed %s after it started, on its way up", c.RanFor))
	}
	c.Repeating = len(c.Reasons) > 0
	rep.Crash = c
	ev := fmt.Sprintf("crash: main process %s; %d time(s) in the last %s", orDash(how), c.Count, c.Window)
	if c.RanFor != "" {
		ev += ", after running " + c.RanFor
	}
	if c.Restarts > 0 {
		ev += fmt.Sprintf(", %d automatic restarts", c.Restarts)
	}
	if c.StartLimit {
		ev += ", start limit hit"
	}
	rep.Evidence = append(rep.Evidence, ev)
	if !c.Repeating {
		c.Reasons = append(c.Reasons, "first crash after it ran for a while: plausibly transient")
	}
}

// crashNext is how to investigate a crash that repeats.
func crashNext(u string, c *CrashInfo) string {
	s := "read its log (`journalctl -u " + u + " -b`)"
	if c.Code == "dumped" {
		s += " and the core dump (`coredumpctl info COREDUMP_UNIT=" + u + "`)"
	}
	s += ", fix the program, its configuration or its input, then start it again"
	if c.StartLimit {
		s += " (`systemctl reset-failed " + u + "` clears the start limit)"
	}
	return s
}

// humanDuration renders a run time the way people say it.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d/time.Hour))
	}
	return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
}
