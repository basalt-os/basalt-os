// Package config reads /etc/basalt-gate/gate.conf: "key = value" lines,
// comments with #. Unknown keys are errors, so a typo never silently
// leaves a default in place.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/peer"
)

// DefaultFile is the configuration file.
const DefaultFile = "/etc/basalt-gate/gate.conf"

// Config of the gate daemon.
type Config struct {
	Socket       string
	StateDir     string
	Registry     string
	Presets      string
	HardLimits   string
	LedgerSocket string
	PowerSupply  string
	// DefaultPreset is installed when there is no rule set yet.
	DefaultPreset string
	// AllowCodeConfirm keeps root's `basalt apply ID --yes --confirm CODE`
	// (recorded as a person's decision).
	AllowCodeConfirm  bool
	InteractiveExpiry time.Duration
	DeferrableExpiry  time.Duration
	ClaimWindow       time.Duration
	// AgentTaintFloor: requests from agent domains carry at least this
	// taint (agents read untrusted content by nature).
	AgentTaintFloor string
	// RequestsPerMinute per requester (bursts of half as many again).
	RequestsPerMinute int
	Peers             peer.Config
}

// Default is the packaged configuration.
func Default() Config {
	return Config{
		Socket:            "/run/basalt-gate/gate.sock",
		StateDir:          "/var/lib/basalt-gate",
		Registry:          "/usr/share/basalt/gate/actions.d",
		Presets:           "/usr/share/basalt/gate/presets",
		HardLimits:        "/usr/share/basalt/gate/hardlimits.json",
		LedgerSocket:      "/run/basalt-ledger/ledger.sock",
		PowerSupply:       "/sys/class/power_supply",
		DefaultPreset:     "careful",
		AllowCodeConfirm:  true,
		InteractiveExpiry: 5 * time.Minute,
		DeferrableExpiry:  24 * time.Hour,
		ClaimWindow:       10 * time.Minute,
		AgentTaintFloor:   "web",
		RequestsPerMinute: 30,
		Peers:             peer.DefaultConfig(),
	}
}

// Load reads file over the defaults; a missing file gives the defaults.
func Load(file string) (Config, error) {
	c := Default()
	f, err := os.Open(file)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return c, fmt.Errorf("%s:%d: expected key = value", file, n)
		}
		if err := c.set(strings.TrimSpace(k), strings.TrimSpace(v)); err != nil {
			return c, fmt.Errorf("%s:%d: %w", file, n, err)
		}
	}
	return c, sc.Err()
}

func (c *Config) set(k, v string) error {
	dur := func(dst *time.Duration) error {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return fmt.Errorf("%s: duration %q", k, v)
		}
		*dst = d
		return nil
	}
	switch k {
	case "socket":
		c.Socket = v
	case "state":
		c.StateDir = v
	case "registry":
		c.Registry = v
	case "presets":
		c.Presets = v
	case "hardlimits":
		c.HardLimits = v
	case "ledger":
		c.LedgerSocket = v
	case "power_supply":
		c.PowerSupply = v
	case "default_preset":
		c.DefaultPreset = v
	case "allow_code_confirm":
		switch v {
		case "yes":
			c.AllowCodeConfirm = true
		case "no":
			c.AllowCodeConfirm = false
		default:
			return fmt.Errorf("allow_code_confirm: yes or no")
		}
	case "interactive_expiry":
		return dur(&c.InteractiveExpiry)
	case "deferrable_expiry":
		return dur(&c.DeferrableExpiry)
	case "claim_window":
		return dur(&c.ClaimWindow)
	case "agent_taint_floor":
		switch v {
		case "none", "system", "personal", "web":
			c.AgentTaintFloor = v
		default:
			return fmt.Errorf("agent_taint_floor: none, system, personal or web")
		}
	case "requests_per_minute":
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 10000 {
			return fmt.Errorf("requests_per_minute: 1 to 10000")
		}
		c.RequestsPerMinute = n
	case "deciders":
		c.Peers.Deciders = strings.Fields(v)
	case "tty_deciders":
		c.Peers.TTYDeciders = strings.Fields(v)
	case "relays":
		// TYPE:kind,kind TYPE:kind
		m := map[string][]string{}
		for _, f := range strings.Fields(v) {
			t, kinds, ok := strings.Cut(f, ":")
			if !ok || t == "" || kinds == "" {
				return fmt.Errorf("relays: TYPE:kind,kind")
			}
			m[t] = strings.Split(kinds, ",")
		}
		c.Peers.Relays = m
	case "agent_types":
		c.Peers.AgentPrefixes = strings.Fields(v)
	default:
		return fmt.Errorf("unknown key %q", k)
	}
	return nil
}
