// Package config reads /etc/basalt/assistant.conf: an INI-style file with
// [section] headers and key = value lines.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/decide"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/diag"
)

// DefaultPath of the configuration file.
const DefaultPath = "/etc/basalt/assistant.conf"

// Config is the assistant's configuration.
type Config struct {
	StateDir  string
	AuditPath string

	// Decision layer.
	Backend       string // rules (default) or openai-compatible (a local model; falls back to rules)
	ModelEndpoint string
	Model         string
	AllowRemote   bool               // a non-local model endpoint is refused unless set
	Calibration   map[string]float64 // per question: temperature applied to the model's log-probabilities

	// Natural-language translator (`basalt ask`), off by default.
	Translator         bool
	TranslatorEndpoint string
	TranslatorModel    string
	TranslatorCompact  bool // the model is fine-tuned for the translator: short prompt
	DefaultThreshold   float64
	Thresholds         map[string]float64

	Disk diag.DiskThresholds

	// Event engine.
	DedupWindow  time.Duration // the same problem is reported once per window
	MaxPerHour   int           // new proposals per hour (rate limit)
	DiskInterval time.Duration
	DnfMinAge    time.Duration // a pre snapshot without post older than this is an unfinished transaction
	DnfSettle    time.Duration // wait after an rpm failure record before diagnosing
	AVCSettle    time.Duration // wait for a burst of denials before analyzing
	UnitSettle   time.Duration
}

// Defaults returns the built-in configuration.
func Defaults() Config {
	return Config{
		StateDir: "/var/lib/basalt-assistant", AuditPath: "/var/log/basalt-assistant/audit.jsonl",
		Backend: "rules", DefaultThreshold: 0.75, Thresholds: map[string]float64{}, Calibration: map[string]float64{},
		TranslatorEndpoint: "unix:/run/basalt-llm/llm.sock",
		Disk:               diag.DefaultDiskThresholds,
		DedupWindow:        time.Hour, MaxPerHour: 20, DiskInterval: 5 * time.Minute,
		DnfMinAge: 2 * time.Minute, DnfSettle: 15 * time.Second, AVCSettle: 5 * time.Second, UnitSettle: 3 * time.Second,
	}
}

// Load reads path over the defaults; a missing file is not an error.
func Load(path string) (Config, error) {
	c := Defaults()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	defer f.Close()
	sec := ""
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			sec = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return c, fmt.Errorf("%s:%d: expected key = value", path, n)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if err := c.set(sec, k, v); err != nil {
			return c, fmt.Errorf("%s:%d: %v", path, n, err)
		}
	}
	return c, sc.Err()
}

func (c *Config) set(sec, k, v string) error {
	fl := func() (float64, error) { return strconv.ParseFloat(v, 64) }
	du := func() (time.Duration, error) { return time.ParseDuration(v) }
	sz := func() (int64, error) { return parseSize(v) }
	var err error
	switch sec + "." + k {
	case "paths.state_dir":
		c.StateDir = v
	case "paths.audit_log":
		c.AuditPath = v
	case "decision.backend":
		if v != "rules" && v != "openai-compatible" {
			return fmt.Errorf("backend %q (rules or openai-compatible)", v)
		}
		c.Backend = v
	case "decision.endpoint":
		c.ModelEndpoint = v
	case "decision.model":
		c.Model = v
	case "decision.allow_remote", "translator.allow_remote":
		c.AllowRemote, err = yesNo(v)
	case "translator.enabled":
		c.Translator, err = yesNo(v)
	case "translator.endpoint":
		c.TranslatorEndpoint = v
	case "translator.model":
		c.TranslatorModel = v
	case "translator.prompt":
		switch v {
		case "examples":
			c.TranslatorCompact = false
		case "compact":
			c.TranslatorCompact = true
		default:
			err = fmt.Errorf("prompt %q (examples or compact)", v)
		}
	case "decision.default_threshold":
		c.DefaultThreshold, err = fl()
	case "disk.warn_pct":
		c.Disk.WarnPct, err = fl()
	case "disk.crit_pct":
		c.Disk.CritPct, err = fl()
	case "disk.snapshots_large":
		c.Disk.SnapshotsLargeBytes, err = sz()
	case "disk.journal_large":
		c.Disk.JournalLargeBytes, err = sz()
	case "disk.cache_large":
		c.Disk.CacheLargeBytes, err = sz()
	case "disk.journal_vacuum_to":
		c.Disk.JournalVacuumTo = v
	case "events.dedup_window":
		c.DedupWindow, err = du()
	case "events.max_proposals_per_hour":
		c.MaxPerHour, err = strconv.Atoi(v)
	case "events.disk_interval":
		c.DiskInterval, err = du()
	case "events.dnf_min_age":
		c.DnfMinAge, err = du()
	case "events.dnf_settle":
		c.DnfSettle, err = du()
	case "events.avc_settle":
		c.AVCSettle, err = du()
	case "events.unit_settle":
		c.UnitSettle, err = du()
	default:
		if sec == "calibration" {
			var t float64
			t, err = fl()
			if err == nil && (t < 0.05 || t > 20) {
				err = fmt.Errorf("temperature %v outside [0.05, 20]", t)
			}
			c.Calibration[k] = t
			return err
		}
		if sec == "thresholds" {
			var t float64
			t, err = fl()
			if err == nil && (t <= 0 || t > 1) {
				err = fmt.Errorf("threshold %v outside (0, 1]", t)
			}
			c.Thresholds[k] = t
			return err
		}
		return fmt.Errorf("unknown setting %s.%s", sec, k)
	}
	return err
}

// DecideConfig is the decision layer's backend selection.
func (c Config) DecideConfig() decide.Config {
	return decide.Config{Backend: c.Backend, Endpoint: c.ModelEndpoint, Model: c.Model,
		AllowRemote: c.AllowRemote, Calibration: c.Calibration}
}

func yesNo(v string) (bool, error) {
	switch strings.ToLower(v) {
	case "yes", "true", "on", "1":
		return true, nil
	case "no", "false", "off", "0":
		return false, nil
	}
	return false, fmt.Errorf("expected yes or no, got %q", v)
}

func parseSize(v string) (int64, error) {
	mult := int64(1)
	switch {
	case strings.HasSuffix(v, "G"):
		mult, v = 1<<30, strings.TrimSuffix(v, "G")
	case strings.HasSuffix(v, "M"):
		mult, v = 1<<20, strings.TrimSuffix(v, "M")
	case strings.HasSuffix(v, "K"):
		mult, v = 1<<10, strings.TrimSuffix(v, "K")
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n * mult, err
}
