package vsm

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The journal reader of the VSM domain: the diagnosers' expressions
// (internal/diag lineFeatures), the two codes the domain added (start
// timeout, OOM kill) and the extractors knowledge cases contribute (a new
// case can teach the reader a new message without retraining). It returns
// codes plus the bindings a message names (config file and line, port,
// path).
var (
	reConfigAt   = regexp.MustCompile(`(?:in |at |file )?(/etc/[A-Za-z0-9._/@+-]+):(\d+)`)
	reConfigErr  = regexp.MustCompile(`(?i)syntax error|bad configuration|unknown directive|invalid (?:option|directive|value|parameter|number)|parse error|unexpected|AH00526|directive is not allowed|is duplicate|not terminated| in /etc/[^ :]+:\d+`)
	reAddrInUse  = regexp.MustCompile(`(?i)address already in use|EADDRINUSE|\(98:`)
	reBindPort   = regexp.MustCompile(`(?:bind\(\) to |listen )\S*?:(\d{1,5}) failed`)
	rePortInMsg  = regexp.MustCompile(`(?:bind\(\) to |listen(?:ing)? on |port[ =]|:)\[?[0-9a-fA-F.:]*\]?:?(\d{1,5})\b`)
	rePermDenied = regexp.MustCompile(`(?i)permission denied|\(13:|EACCES`)
	reNoSuchFile = regexp.MustCompile(`(?i)no such file or directory|\(2:`)
	reNoSpace    = regexp.MustCompile(`(?i)no space left on device|ENOSPC`)
	reQuotedPath = regexp.MustCompile(`"(/[^"]+)"|'(/[^']+)'|\s(/[A-Za-z0-9._/@+-]+)`)
	reTimeout    = regexp.MustCompile(`(?i)start operation timed out|Failed with result 'timeout'`)
	reOOM        = regexp.MustCompile(`(?i)killed by the OOM killer|result 'oom-kill'`)
)

// JournalFeatures classifies journal messages. unit, when set, limits
// "Dependency failed for X" to this unit (a failed unit's journal also
// shows the jobs it broke). Bindings are first-wins.
func JournalFeatures(lines []string, extractors map[string]*regexp.Regexp, unit string) (map[string]bool, map[string]string) {
	codes, b := map[string]bool{}, map[string]string{}
	setdef := func(k, v string) {
		if _, ok := b[k]; !ok {
			b[k] = v
		}
	}
	excodes := make([]string, 0, len(extractors))
	for c := range extractors {
		excodes = append(excodes, c)
	}
	sort.Strings(excodes)
	for _, m := range lines {
		cfg := reConfigErr.MatchString(m)
		if cfg {
			codes["ce"] = true
		}
		if reAddrInUse.MatchString(m) {
			codes["au"] = true
		}
		perm := rePermDenied.MatchString(m)
		if perm {
			codes["pd"] = true
		}
		nsf := reNoSuchFile.MatchString(m)
		if nsf && !cfg {
			codes["nf"] = true
		}
		if reNoSpace.MatchString(m) {
			codes["ns"] = true
		}
		if strings.Contains(m, "Dependency failed") && (unit == "" || strings.Contains(m, unit)) {
			codes["dp"] = true
		}
		if reTimeout.MatchString(m) {
			codes["to"] = true
		}
		if reOOM.MatchString(m) {
			codes["om"] = true
		}
		if mm := reConfigAt.FindStringSubmatch(m); mm != nil && cfg {
			codes["cl"] = true
			setdef("config_file", mm[1])
			setdef("config_line", mm[2])
		}
		pm := reBindPort.FindStringSubmatch(m)
		if pm == nil && (strings.Contains(m, "bind") || strings.Contains(strings.ToLower(m), "listen")) {
			pm = rePortInMsg.FindStringSubmatch(m)
		}
		if pm != nil {
			if n, err := strconv.Atoi(pm[1]); err == nil && n > 0 && n < 65536 {
				setdef("port", pm[1])
			}
		}
		if perm || nsf {
			if q := reQuotedPath.FindStringSubmatch(m); q != nil {
				for _, g := range q[1:] {
					if g != "" {
						setdef("path", g)
						break
					}
				}
			}
		}
		for _, c := range excodes {
			if extractors[c].MatchString(m) {
				codes[c] = true
			}
		}
	}
	return codes, b
}

// UnitStateFeatures turns systemctl show properties into codes.
func UnitStateFeatures(state map[string]string) map[string]bool {
	codes := map[string]bool{}
	if state["ActiveState"] == "failed" {
		codes["uf"] = true
	}
	switch state["Result"] {
	case "signal", "core-dump", "watchdog":
		codes["sg"] = true
	}
	return codes
}
