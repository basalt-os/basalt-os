// Package simulate replays recorded requests (the gate.request records
// the gate writes to the ledger, with their metadata but never their
// content) against a draft rule set: "last week this rule would have
// allowed 3 requests", listed. Nothing is decided or run.
package simulate

import (
	"sort"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/policy"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/proposal"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/pkg/gate"
)

// Record is one ledger record as the simulator reads it.
type Record struct {
	Time    string         `json:"time"`
	UID     int            `json:"uid"`
	Event   string         `json:"event"`
	Outcome string         `json:"outcome"`
	Data    map[string]any `json:"data"`
}

// Request is a recorded request, rebuilt.
type Request struct {
	ID    string
	Time  time.Time
	Who   string
	Input policy.Input
	Was   string
}

// Requests rebuilds the requests from gate.request records, with the
// final outcome from the last gate.decision of each.
func Requests(recs []Record) []Request {
	was := map[string]string{}
	for _, r := range recs {
		if r.Event == "gate.decision" {
			if id, _ := r.Data["id"].(string); id != "" {
				if o, _ := r.Data["outcome"].(string); o != "" {
					was[id] = o
				}
			}
		}
	}
	var out []Request
	for _, r := range recs {
		if r.Event != "gate.request" || r.Outcome == "denied" {
			continue
		}
		d := r.Data
		id, _ := d["id"].(string)
		t, err := time.Parse(time.RFC3339Nano, r.Time)
		if err != nil {
			continue
		}
		req := proposal.Requester{UID: r.UID, Taint: str(d["taint"]), Origin: str(d["origin"])}
		if m, ok := d["requester"].(map[string]any); ok {
			req.Kind, req.Name, req.Session = str(m["kind"]), str(m["name"]), str(m["session"])
		}
		if l, ok := d["via"].([]any); ok {
			for _, e := range l {
				if m, ok := e.(map[string]any); ok {
					req.Via = append(req.Via, proposal.Party{Kind: str(m["kind"]), Name: str(m["name"]), Session: str(m["session"])})
				}
			}
		}
		p := &proposal.Proposal{ID: id, Requester: req, Class: str(d["class"])}
		var calls []policy.CallFacts
		if l, ok := d["calls"].([]any); ok {
			for _, e := range l {
				m, _ := e.(map[string]any)
				cf := policy.CallFacts{Call: proposal.Call{Action: str(m["action"]), Args: map[string]any{}},
					Class: str(m["class"])}
				cf.System, _ = m["system"].(bool)
				if rl, ok := m["resources"].([]any); ok {
					for _, x := range rl {
						k, v, ok := strings.Cut(str(x), ":")
						if ok {
							cf.Resources = append(cf.Resources, proposal.Resource{Kind: k, Value: v})
						}
					}
				}
				p.Calls = append(p.Calls, cf.Call)
				calls = append(calls, cf)
			}
		}
		env := policy.Env{Now: t, Power: policy.Unknown, Presence: policy.Unknown, Network: policy.Unknown}
		if m, ok := d["env"].(map[string]any); ok {
			if s := str(m["power"]); s != "" {
				env.Power = s
			}
			if s := str(m["presence"]); s != "" {
				env.Presence = s
			}
			if s := str(m["network"]); s != "" {
				env.Network = s
			}
		}
		out = append(out, Request{ID: id, Time: t, Who: str(d["who"]), Was: was[id],
			Input: policy.Input{P: p, Calls: calls, Env: env}})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// replayCounter counts what the draft rules allowed during the replay,
// so limits apply as they would have.
type use struct {
	rule, chain string
	t           time.Time
}

type replayCounter struct{ uses []use }

func (c *replayCounter) Count(rule, chain string, since time.Time) int {
	n := 0
	for _, u := range c.uses {
		if u.rule == rule && (chain == "" || u.chain == chain) && !u.t.Before(since) {
			n++
		}
	}
	return n
}

func (c *replayCounter) Paused(string) bool { return false }

// Run replays reqs against rules with the engine's registry and hard
// limits. A request whose actions are no longer registered is replayed
// with the classes it was recorded with.
func Run(eng *policy.Engine, rules []policy.Rule, reqs []Request, since time.Time) gate.Sim {
	sim := gate.Sim{Since: since.UTC().Format(time.RFC3339), Items: []gate.SimItem{}}
	byRef := map[string]policy.Rule{}
	for _, r := range rules {
		byRef[r.Ref()] = r
	}
	rc := &replayCounter{}
	for _, rq := range reqs {
		if rq.Time.Before(since) {
			continue
		}
		d := eng.DecideWith(rq.Input, rc, rules)
		if d.Outcome == policy.Allowed {
			for _, ref := range d.Rules {
				if r, ok := byRef[ref]; ok {
					rc.uses = append(rc.uses, use{rule: r.Key(), chain: policy.ChainKey(rq.Input.P.Requester), t: rq.Time})
				}
			}
		}
		sim.Total++
		switch d.Outcome {
		case policy.Allowed:
			sim.Allowed++
		case policy.Asked:
			sim.Asked++
		default:
			sim.Refused++
		}
		was := rq.Was
		if was != "" && !sameOutcome(was, d.Outcome) {
			sim.Changed++
		}
		sim.Items = append(sim.Items, gate.SimItem{Time: rq.Time.UTC().Format(time.RFC3339), ID: rq.ID,
			Actions: rq.Input.P.Actions(), Who: rq.Who, Would: d.Outcome, By: d.By, Was: was})
	}
	if sim.Total == 0 {
		sim.Note = "no recorded requests in this period"
	}
	return sim
}

// sameOutcome compares a recorded final outcome with a policy outcome:
// a request a person approved or declined was "asked" by the policy.
func sameOutcome(was, would string) bool {
	switch was {
	case "approved", "declined", "expired", "cancelled", "asked":
		return would == policy.Asked
	}
	return was == would
}
