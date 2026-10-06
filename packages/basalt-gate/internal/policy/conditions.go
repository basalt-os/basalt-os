package policy

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Env is what the conditions of rules are checked against at decision
// time. A value the gate cannot know yet is "unknown": a condition on it
// never lets an allow rule match, and always lets an ask or refuse rule
// match (fail safe).
type Env struct {
	Now      time.Time
	Power    string // ac, battery, unknown
	Network  string // a connection id, metered, unknown
	Presence string // present, away, unknown
}

// Unknown is the value of a condition the gate cannot read.
const Unknown = "unknown"

var dayNames = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

func dayIndex(s string) (int, error) {
	for i, d := range dayNames {
		if d == s {
			return i, nil
		}
	}
	return 0, fmt.Errorf("day %q (mon, tue, wed, thu, fri, sat, sun)", s)
}

// parseDays reads "mon-fri", "sat,sun", "mon-sun" into a weekday set.
func parseDays(s string) ([7]bool, error) {
	var set [7]bool
	if s == "" {
		for i := range set {
			set[i] = true
		}
		return set, nil
	}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		a, b, isRange := strings.Cut(part, "-")
		from, err := dayIndex(a)
		if err != nil {
			return set, err
		}
		to := from
		if isRange {
			if to, err = dayIndex(b); err != nil {
				return set, err
			}
		}
		// Ranges follow the week from Monday: mon-sun covers all seven.
		for i := from; ; i = (i + 1) % 7 {
			set[i] = true
			if i == to {
				break
			}
		}
	}
	return set, nil
}

// parseClock reads HH:MM (24:00 allowed) as minutes after midnight.
func parseClock(s string) (int, error) {
	h, m, ok := strings.Cut(s, ":")
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if !ok || err1 != nil || err2 != nil || len(m) != 2 || hh < 0 || hh > 24 || mm < 0 || mm > 59 || (hh == 24 && mm != 0) {
		return 0, fmt.Errorf("time %q (HH:MM)", s)
	}
	return hh*60 + mm, nil
}

func parseHours(s string) (int, int, error) {
	if s == "" {
		return 0, 24 * 60, nil
	}
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, fmt.Errorf("hours %q (HH:MM-HH:MM)", s)
	}
	from, err := parseClock(a)
	if err != nil {
		return 0, 0, err
	}
	to, err := parseClock(b)
	if err != nil {
		return 0, 0, err
	}
	return from, to, nil
}

func (w *When) check() error {
	if _, err := parseDays(w.Days); err != nil {
		return err
	}
	if _, _, err := parseHours(w.Hours); err != nil {
		return err
	}
	switch w.Power {
	case "", "any", "ac", "battery":
	default:
		return fmt.Errorf("power %q (any, ac, battery)", w.Power)
	}
	switch w.Presence {
	case "", "any", "present", "away":
	default:
		return fmt.Errorf("presence %q (any, present, away)", w.Presence)
	}
	if w.Network != "" && w.Network != "any" && w.Network != "metered" && !strings.HasPrefix(w.Network, "uuid:") {
		return fmt.Errorf("network %q (any, metered, uuid:CONNECTION)", w.Network)
	}
	return nil
}

// condition results
const (
	condNo = iota
	condYes
	condUnknown
)

// eval checks the conditions against env: yes, no, or unknown (a value
// the gate cannot read decides the condition).
func (w *When) eval(env Env) int {
	if w == nil {
		return condYes
	}
	days, err := parseDays(w.Days)
	if err != nil {
		return condNo
	}
	now := env.Now.Local()
	if !days[int(now.Weekday())] {
		return condNo
	}
	from, to, err := parseHours(w.Hours)
	if err != nil {
		return condNo
	}
	min := now.Hour()*60 + now.Minute()
	inHours := (from <= to && min >= from && min < to) || (from > to && (min >= from || min < to))
	if !inHours {
		return condNo
	}
	res := condYes
	check := func(want, have string) {
		if want == "" || want == "any" {
			return
		}
		if have == "" || have == Unknown {
			if res == condYes {
				res = condUnknown
			}
			return
		}
		if want != have {
			res = condNo
		}
	}
	check(w.Power, env.Power)
	check(w.Presence, env.Presence)
	if w.Network != "" && w.Network != "any" {
		check(w.Network, env.Network)
	}
	return res
}

// Schedule is a parsed trigger: days and a time of day.
type Schedule struct {
	Days   [7]bool
	Minute int
}

// ParseSchedule reads "fri 18:00", "mon-fri 09:30", "daily 03:00".
func ParseSchedule(s string) (Schedule, error) {
	var out Schedule
	d, t, ok := strings.Cut(strings.TrimSpace(s), " ")
	if !ok {
		return out, fmt.Errorf("schedule %q (e.g. \"fri 18:00\" or \"daily 03:00\")", s)
	}
	if d == "daily" {
		d = "mon-sun"
	}
	days, err := parseDays(d)
	if err != nil {
		return out, err
	}
	m, err := parseClock(strings.TrimSpace(t))
	if err != nil || m >= 24*60 {
		return out, fmt.Errorf("schedule %q: time", s)
	}
	out.Days, out.Minute = days, m
	return out, nil
}

// PowerFrom reads /sys/class/power_supply (root is the sysfs directory):
// "ac" when an external supply is online or there is no battery at all (a
// desktop or a server), "battery" when a battery discharges with no
// supply online, "unknown" when it cannot be read.
func PowerFrom(root string) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return Unknown
	}
	battery, online := false, false
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		typ, _ := os.ReadFile(filepath.Join(dir, "type"))
		switch strings.TrimSpace(string(typ)) {
		case "Battery":
			if scope, _ := os.ReadFile(filepath.Join(dir, "scope")); strings.TrimSpace(string(scope)) == "Device" {
				continue // a mouse or a headset
			}
			battery = true
		case "Mains", "USB", "USB_C", "USB_PD", "Wireless":
			if v, _ := os.ReadFile(filepath.Join(dir, "online")); strings.TrimSpace(string(v)) == "1" {
				online = true
			}
		}
	}
	if online || !battery {
		return "ac"
	}
	return "battery"
}
