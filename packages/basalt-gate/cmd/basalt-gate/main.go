// basalt-gate is the command line of the approval gate (docs/gate.md):
// ask for an action, look at the queue, approve or decline with polkit,
// stop and resume all automation, list rules, add or remove them (as gate
// requests) and replay recorded requests against draft rules. It is also
// reached as `basalt gate` through the basalt command's fallback.
//
// Exit codes of `request --wait` (and of `request` for immediate
// answers): 0 allowed, 2 still waiting, 3 declined, 4 refused,
// 5 expired or cancelled, 1 any other error.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/conformance"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/pkg/gate"
)

var version = "dev"

// TTYHelper is the terminal decider program.
var TTYHelper = "/usr/libexec/basalt-gate/basalt-gate-tty"

const usage = `basalt-gate: the approval gate

  basalt-gate status                       automation state, rules, waiting requests
  basalt-gate request --action ID [--arg K=V | --arg K:=JSON]... [--wait] [--timeout S]
                      [--deferrable] [--taint T] [--class-hint Cn] [--group G] [--json]
  basalt-gate check   --action ID [--arg ...]  would it be allowed now? (nothing is queued)
  basalt-gate queue                        requests waiting for you (as a terminal decider)
  basalt-gate show ID                      one request: who asks, what changes, the code
  basalt-gate approve ID... [--remember]   approve (polkit asks for your password)
  basalt-gate decline ID...                decline
  basalt-gate history [N]                  decided requests
  basalt-gate stop [REASON]                stop all automation now (no password)
  basalt-gate resume                       resume automation (polkit)
  basalt-gate rules list                   rules as sentences
  basalt-gate rules add FILE [--scope user|system]    add rules (as requests; always ask and
                                                       never allow take effect at once)
  basalt-gate rules remove ID [--scope user|system]
  basalt-gate rules draft FILE [--scope S] [--since 7d]  a rule's sentence and dry run
  basalt-gate rules unpause KEY            let a rule allow again after its limit
  basalt-gate simulate --rules FILE [--since 7d] [--scope S] [--records FILE]
                                           replay recorded requests against a rule file
  basalt-gate fake-server SOCKET           a fake gate for testing a client of the third-party
                                           subset (PROTOCOL.md); prints each request and
                                           every protocol violation
  basalt-gate version
`

func main() {
	code, err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "basalt-gate:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func run(args []string) (int, error) {
	if len(args) == 0 {
		fmt.Print(usage)
		return 0, nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "help", "-h", "--help":
		fmt.Print(usage)
		return 0, nil
	case "version", "--version":
		fmt.Println("basalt-gate", version)
		return 0, nil
	case "status":
		return cmdStatus()
	case "request":
		return cmdRequest(rest, false)
	case "check":
		return cmdRequest(rest, true)
	case "queue", "list":
		return cmdQueue("pending", 0)
	case "history":
		n := 50
		if len(rest) > 0 {
			n, _ = strconv.Atoi(rest[0])
		}
		return cmdQueue("history", n)
	case "show":
		if len(rest) != 1 {
			return 1, errors.New("usage: basalt-gate show ID")
		}
		return cmdShow(rest[0])
	case "approve", "decline":
		return cmdDecide(cmd == "approve", rest)
	case "stop":
		return cmdStop(strings.Join(rest, " "))
	case "resume":
		rep, err := viaTTY(gate.Request{Op: "resume"}, true)
		return report(rep, err, "Automation resumed.")
	case "rules":
		return cmdRules(rest)
	case "simulate":
		return cmdSimulate(rest)
	case "fake-server":
		return cmdFakeServer(rest)
	}
	return 1, fmt.Errorf("unknown command %q (basalt-gate help)", cmd)
}

func dial(role string) (*gate.Client, error) {
	c, err := gate.Detect("", "basalt-gate/"+version, role)
	if err != nil {
		return nil, fmt.Errorf("the approval gate is not running (%v)", err)
	}
	return c, nil
}

func report(rep gate.Reply, err error, okText string) (int, error) {
	if err != nil {
		return 1, err
	}
	if !rep.OK {
		return 1, errors.New(rep.Error)
	}
	if okText != "" {
		fmt.Println(okText)
	}
	return 0, nil
}

func cmdStatus() (int, error) {
	c, err := dial("requester")
	if err != nil {
		return 1, err
	}
	defer c.Close()
	rep, err := c.GateStatus()
	if err != nil {
		return 1, err
	}
	st := rep.Status
	state := "on"
	if st.Stopped {
		state = fmt.Sprintf("STOPPED by %s at %s (every request asks a person)", st.StoppedBy, st.StoppedAt)
	}
	seal := "sealed"
	if !st.SealOK {
		seal = "NOT TRUSTED (the rule file failed its seal; only the careful rules are in force)"
	}
	fmt.Printf("Approval gate %s (%s)\n", st.Version, st.Protocol)
	fmt.Printf("  automation: %s\n  preset:     %s\n  rules:      %d (%s)\n  waiting:    %d\n  actions:    %d registered\n",
		state, st.Preset, st.Rules, seal, st.Pending, st.Actions)
	fmt.Printf("  you are:    %s (%s)\n", c.Hello.Kind, strings.Join(c.Hello.Roles, ", "))
	return 0, nil
}

// argList collects repeated --arg flags.
type argList []string

func (a *argList) String() string     { return strings.Join(*a, ",") }
func (a *argList) Set(v string) error { *a = append(*a, v); return nil }

func parseArgs(l argList) (map[string]any, error) {
	out := map[string]any{}
	for _, a := range l {
		if k, v, ok := strings.Cut(a, ":="); ok && !strings.Contains(k, "=") {
			var x any
			dec := json.NewDecoder(strings.NewReader(v))
			dec.UseNumber()
			if err := dec.Decode(&x); err != nil {
				return nil, fmt.Errorf("--arg %s: %v", k, err)
			}
			out[k] = x
			continue
		}
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			return nil, fmt.Errorf("--arg %q (K=V or K:=JSON)", a)
		}
		out[k] = v
	}
	return out, nil
}

func exitFor(decision string) int {
	switch decision {
	case gate.Allowed:
		return 0
	case gate.Asked:
		return 2
	case gate.Declined:
		return 3
	case gate.Refused:
		return 4
	case gate.Expired, gate.Cancelled:
		return 5
	}
	return 1
}

func cmdRequest(args []string, check bool) (int, error) {
	fs := flag.NewFlagSet("request", flag.ContinueOnError)
	action := fs.String("action", "", "action id")
	var al argList
	fs.Var(&al, "arg", "argument K=V (string) or K:=JSON")
	argsJSON := fs.String("args-json", "", "all arguments as one JSON object")
	wait := fs.Bool("wait", false, "wait for the decision")
	timeout := fs.Int("timeout", 180, "seconds to wait")
	deferrable := fs.Bool("deferrable", false, "may wait in the queue up to a day")
	taint := fs.String("taint", "", "none, system, personal or web")
	hint := fs.String("class-hint", "", "raise the class (C0 to C5)")
	group := fs.String("group", "", "approve together with other requests of this group")
	asJSON := fs.Bool("json", false, "print the reply as JSON")
	if err := fs.Parse(args); err != nil {
		return 1, err
	}
	if *action == "" {
		return 1, errors.New("--action is required")
	}
	a, err := parseArgs(al)
	if err != nil {
		return 1, err
	}
	if *argsJSON != "" {
		dec := json.NewDecoder(strings.NewReader(*argsJSON))
		dec.UseNumber()
		if err := dec.Decode(&a); err != nil {
			return 1, fmt.Errorf("--args-json: %v", err)
		}
	}
	c, err := dial("requester")
	if err != nil {
		return 1, err
	}
	defer c.Close()
	p := gate.Proposal{Calls: []gate.Call{{Action: *action, Args: a}}, Taint: *taint, Deferrable: *deferrable,
		ClassHint: *hint, Group: *group}
	var rep gate.Reply
	if check {
		rep, err = c.Check(p)
	} else {
		rep, err = c.Propose(p)
	}
	if err != nil {
		return 1, err
	}
	if !check && *wait && rep.Decision == gate.Asked {
		if !*asJSON {
			fmt.Fprintf(os.Stderr, "Waiting for approval of %s (%s)...\n", rep.ID, rep.Reason)
		}
		rep, err = c.Wait(rep.ID, *timeout)
		if err != nil {
			return 1, err
		}
		if rep.TimedOut {
			rep.Decision = gate.Asked
		}
	}
	if *asJSON {
		b, _ := json.Marshal(rep)
		fmt.Println(string(b))
	} else {
		line := rep.Decision
		if rep.ID != "" {
			line = rep.ID + ": " + line
		}
		if rep.Class != "" {
			line += " (" + rep.Class + ")"
		}
		if rep.Reason != "" {
			line += ": " + rep.Reason
		}
		fmt.Println(line)
	}
	return exitFor(rep.Decision), nil
}

// viaTTY runs one decider request through the terminal decider helper.
// With auth, a textual polkit agent (pkttyagent) is registered for the
// helper's process first, so polkit can ask for the password here.
func viaTTY(req gate.Request, auth bool) (gate.Reply, error) {
	var rep gate.Reply
	b, _ := json.Marshal(req)
	cmd := exec.Command(TTYHelper, string(b))
	cmd.Stdin, cmd.Stderr = os.Stdin, os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return rep, err
	}
	var syncW *os.File
	if auth {
		r, w, err := os.Pipe()
		if err != nil {
			return rep, err
		}
		cmd.ExtraFiles = []*os.File{r}
		cmd.Env = append(os.Environ(), "BASALT_GATE_TTY_SYNC=3")
		syncW = w
		defer r.Close()
	}
	if err := cmd.Start(); err != nil {
		return rep, fmt.Errorf("the terminal decider (%s): %w", TTYHelper, err)
	}
	var agent *exec.Cmd
	if auth {
		agent, err = startAgent(cmd.Process.Pid)
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return rep, err
		}
		_, _ = syncW.Write([]byte{1})
		syncW.Close()
	}
	line, rerr := bufio.NewReader(out).ReadBytes('\n')
	werr := cmd.Wait()
	if agent != nil {
		_ = agent.Process.Signal(syscall.SIGTERM)
		_ = agent.Wait()
	}
	if len(line) == 0 {
		if werr != nil {
			return rep, fmt.Errorf("the terminal decider failed: %v", werr)
		}
		return rep, fmt.Errorf("the terminal decider answered nothing: %v", rerr)
	}
	if err := json.Unmarshal(line, &rep); err != nil {
		return rep, err
	}
	return rep, nil
}

// startAgent registers pkttyagent for the helper process and waits until
// it is ready.
func startAgent(pid int) (*exec.Cmd, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	agent := exec.Command("pkttyagent", "--process", strconv.Itoa(pid), "--notify-fd", "3", "--fallback")
	agent.Stdin, agent.Stdout, agent.Stderr = os.Stdin, os.Stderr, os.Stderr
	agent.ExtraFiles = []*os.File{w}
	if err := agent.Start(); err != nil {
		w.Close()
		return nil, fmt.Errorf("pkttyagent: %w", err)
	}
	w.Close()
	ready := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(r)
		ready <- err
	}()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		_ = agent.Process.Kill()
		_ = agent.Wait()
		return nil, errors.New("pkttyagent did not register in time")
	}
	return agent, nil
}

func cmdQueue(op string, limit int) (int, error) {
	rep, err := viaTTY(gate.Request{Op: op, Limit: limit}, false)
	if err != nil {
		return 1, err
	}
	if !rep.OK {
		return 1, errors.New(rep.Error)
	}
	if len(rep.Requests) == 0 {
		if op == "pending" {
			fmt.Println("Nothing is waiting for you.")
		} else {
			fmt.Println("No decided requests yet.")
		}
		return 0, nil
	}
	for _, v := range rep.Requests {
		printRow(v)
	}
	return 0, nil
}

func printRow(v gate.View) {
	when := v.Created
	if t, err := time.Parse(time.RFC3339, v.Created); err == nil {
		when = t.Local().Format("Jan 02 15:04")
	}
	extra := ""
	if v.Decision != gate.Asked {
		extra = "  " + v.Decision + " by " + v.By
	} else if v.Code != "" {
		extra = "  code " + v.Code
	}
	fmt.Printf("%s  %s  %-2s %-20s %s  %s%s\n", v.ID, when, v.Class, v.ClassName, strings.Join(v.Actions, ","), v.Who, extra)
}

func cmdShow(id string) (int, error) {
	rep, err := viaTTY(gate.Request{Op: "status", ID: id}, false)
	if err != nil {
		return 1, err
	}
	if !rep.OK || rep.Request == nil {
		return 1, errors.New(rep.Error)
	}
	v := rep.Request
	fmt.Printf("Request %s: %s\n", v.ID, v.Decision)
	fmt.Printf("  who asks:   %s (uid %d, taint %s, origin %s)\n", v.Who, v.UID, v.Taint, v.Origin)
	fmt.Printf("  what:       %s, %s (%s)\n", strings.Join(v.Actions, ", "), v.ClassName, v.Class)
	pv, _ := json.Marshal(v.Preview.TitleArgs)
	fmt.Printf("  preview:    %s %s\n", v.Preview.TitleKey, string(pv))
	for _, l := range v.Preview.Lines {
		la, _ := json.Marshal(l.Args)
		fmt.Printf("              %s %s\n", l.Key, string(la))
	}
	for _, c := range v.Preview.Commands {
		fmt.Printf("  command:    %s\n", c)
	}
	for _, r := range v.Resources {
		fmt.Printf("  touches:    %s %s\n", r.Kind, r.Value)
	}
	if v.Reversible != "" {
		fmt.Printf("  undo:       %s\n", v.Reversible)
	}
	fmt.Printf("  why it asks: %s\n", v.Reason)
	if v.Code != "" {
		fmt.Printf("  code:       %s\n", v.Code)
	}
	if v.Remember != "" {
		fmt.Printf("  remember:   approve --remember lets it run without asking for %s\n", v.Remember)
	}
	if v.NeedsAdmin {
		fmt.Println("  approving asks for administrator authentication")
	}
	fmt.Printf("  expires:    %s\n", v.Expires)
	return 0, nil
}

func cmdDecide(approve bool, args []string) (int, error) {
	remember := false
	var ids []string
	for _, a := range args {
		switch a {
		case "--remember":
			remember = true
		default:
			ids = append(ids, a)
		}
	}
	if len(ids) == 0 {
		return 1, errors.New("which request? (basalt-gate queue)")
	}
	rep, err := viaTTY(gate.Request{Op: "decide", IDs: ids, Approve: &approve, Remember: remember}, approve)
	if err != nil {
		return 1, err
	}
	if !rep.OK {
		return 1, errors.New(rep.Error)
	}
	keys := make([]string, 0, len(rep.Decided))
	for k := range rep.Decided {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%s: %s\n", k, rep.Decided[k])
	}
	return 0, nil
}

func cmdStop(reason string) (int, error) {
	c, err := dial("requester")
	if err != nil {
		return 1, err
	}
	defer c.Close()
	rep, err := c.Stop(reason)
	return report(rep, err, "All automation is stopped: every request asks a person until `basalt-gate resume`.")
}

func cmdRules(args []string) (int, error) {
	if len(args) == 0 {
		return 1, errors.New("basalt-gate rules list | add FILE | remove ID | draft FILE | unpause KEY")
	}
	fs := flag.NewFlagSet("rules", flag.ContinueOnError)
	scope := fs.String("scope", "user", "user or system")
	since := fs.String("since", "7d", "dry run period")
	sub := args[0]
	// Flags may come before or after the file: parse around positionals.
	var pos []string
	rest := args[1:]
	for {
		if err := fs.Parse(rest); err != nil {
			return 1, err
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	switch sub {
	case "list":
		c, err := dial("requester")
		if err != nil {
			return 1, err
		}
		defer c.Close()
		rep, err := c.RulesList()
		if err != nil {
			return 1, err
		}
		printRules(rep.Rules)
		return 0, nil
	case "add", "draft":
		if len(pos) != 1 {
			return 1, fmt.Errorf("usage: basalt-gate rules %s FILE [--scope user|system]", sub)
		}
		return rulesFromFile(sub, pos[0], *scope, *since)
	case "remove":
		if len(pos) != 1 {
			return 1, errors.New("usage: basalt-gate rules remove ID [--scope user|system]")
		}
		return ruleChange(map[string]any{"op": "remove", "scope": *scope, "id": pos[0]}, "remove "+pos[0])
	case "unpause":
		if len(pos) != 1 {
			return 1, errors.New("usage: basalt-gate rules unpause KEY (system/r-x or user/UID/r-x)")
		}
		rep, err := viaTTY(gate.Request{Op: "rules.unpause", ID: pos[0]}, true)
		return report(rep, err, "The rule allows again.")
	}
	return 1, fmt.Errorf("unknown rules command %q", sub)
}

func printRules(rules []gate.RuleView) {
	if len(rules) == 0 {
		fmt.Println("No rules: every request asks a person.")
		return
	}
	for _, r := range rules {
		var x struct {
			ID     string `json:"id"`
			Effect string `json:"effect"`
		}
		_ = json.Unmarshal(r.Rule, &x)
		line := fmt.Sprintf("%-24s %-6s %s.", x.ID, r.Scope, r.Sentence)
		if r.Paused != "" {
			line += " PAUSED: " + r.Paused
		}
		fmt.Println(line)
	}
}

// readRuleFile parses a rule file into JSON objects, one per rule (the
// gate validates them).
func readRuleFile(file string) ([]map[string]any, string, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, "", err
	}
	text := string(b)
	m, err := parseRuleText(text)
	if err != nil {
		return nil, "", err
	}
	return m, text, nil
}

func rulesFromFile(sub, file, scope, since string) (int, error) {
	rules, _, err := readRuleFile(file)
	if err != nil {
		return 1, err
	}
	if len(rules) == 0 {
		return 1, errors.New("no [[rule]] in " + file)
	}
	code := 0
	for _, r := range rules {
		raw, _ := json.Marshal(r)
		if sub == "draft" {
			rep, err := viaTTY(gate.Request{Op: "rules.draft", Rule: raw, Scope: scope, Since: since}, false)
			if err != nil {
				return 1, err
			}
			if !rep.OK {
				return 1, errors.New(rep.Error)
			}
			for _, v := range rep.Rules {
				if v.Error != "" {
					fmt.Printf("%v: cannot be saved: %s\n", r["id"], v.Error)
					code = 1
					continue
				}
				fmt.Printf("%s.\n", v.Sentence)
			}
			if rep.Simulation != nil {
				printSim(*rep.Simulation)
			}
			continue
		}
		c, err := ruleChange(map[string]any{"op": "add", "scope": scope, "rule": r}, fmt.Sprint("add ", r["id"]))
		if c != 0 {
			code = c
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "basalt-gate:", err)
		}
	}
	return code, nil
}

// ruleChange: a tightening change takes effect at once through the
// terminal decider (polkit); anything else is a gate request to approve.
func ruleChange(args map[string]any, what string) (int, error) {
	eff := ""
	if r, ok := args["rule"].(map[string]any); ok {
		eff, _ = r["effect"].(string)
	}
	if args["op"] == "add" && (eff == "ask" || eff == "refuse") {
		raw, _ := json.Marshal(args)
		rep, err := viaTTY(gate.Request{Op: "rules.apply", Rule: raw}, true)
		return report(rep, err, what+": in force now (it only narrows what runs without asking).")
	}
	if args["op"] == "remove" {
		raw, _ := json.Marshal(args)
		rep, err := viaTTY(gate.Request{Op: "rules.apply", Rule: raw}, true)
		if err == nil && rep.OK {
			fmt.Println(what + ": removed now (it only narrows what runs without asking).")
			return 0, nil
		}
	}
	c, err := dial("requester")
	if err != nil {
		return 1, err
	}
	defer c.Close()
	rep, err := c.Propose(gate.Proposal{Calls: []gate.Call{{Action: "gate.rule.change", Args: args}}})
	if err != nil {
		return 1, err
	}
	switch rep.Decision {
	case gate.Asked:
		fmt.Printf("%s: request %s waits for approval (basalt-gate approve %s)\n", what, rep.ID, rep.ID)
		return 2, nil
	case gate.Allowed:
		fmt.Printf("%s: done\n", what)
		return 0, nil
	}
	return exitFor(rep.Decision), fmt.Errorf("%s: %s: %s", what, rep.Decision, rep.Reason)
}

func cmdSimulate(args []string) (int, error) {
	fs := flag.NewFlagSet("simulate", flag.ContinueOnError)
	rules := fs.String("rules", "", "rule file (TOML or JSON)")
	since := fs.String("since", "7d", "period")
	scope := fs.String("scope", "system", "user or system")
	records := fs.String("records", "", "replay these records (a JSON array of ledger records, or a basalt-ledger export) instead of the ledger")
	if err := fs.Parse(args); err != nil {
		return 1, err
	}
	if *rules == "" {
		return 1, errors.New("--rules FILE is required")
	}
	b, err := os.ReadFile(*rules)
	if err != nil {
		return 1, err
	}
	var recs json.RawMessage
	if *records != "" {
		rb, err := os.ReadFile(*records)
		if err != nil {
			return 1, err
		}
		recs, err = recordsOf(rb)
		if err != nil {
			return 1, err
		}
	}
	text, _ := json.Marshal(string(b))
	rep, err := viaTTY(gate.Request{Op: "rules.simulate", Rules: text, Scope: *scope, Since: *since, Records: recs}, false)
	if err != nil {
		return 1, err
	}
	if !rep.OK {
		return 1, errors.New(rep.Error)
	}
	printSim(*rep.Simulation)
	return 0, nil
}

// recordsOf accepts a JSON array of records or a basalt-ledger export.
func recordsOf(b []byte) (json.RawMessage, error) {
	var export struct {
		Records json.RawMessage `json:"records"`
	}
	if err := json.Unmarshal(b, &export); err == nil && len(export.Records) > 0 {
		return export.Records, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(b, &arr); err != nil {
		return nil, errors.New("records: a JSON array of ledger records or a basalt-ledger export")
	}
	return b, nil
}

func printSim(s gate.Sim) {
	fmt.Printf("Since %s: %d requests; with these rules %d would be allowed, %d asked, %d refused (%d differ from what happened).\n",
		s.Since, s.Total, s.Allowed, s.Asked, s.Refused, s.Changed)
	if s.Note != "" {
		fmt.Println(s.Note)
	}
	for _, it := range s.Items {
		was := ""
		if it.Was != "" {
			was = " (was " + it.Was + ")"
		}
		fmt.Printf("  %s %s %s: %s by %s%s\n", it.Time, it.ID, strings.Join(it.Actions, ","), it.Would, it.By, was)
	}
}

// cmdFakeServer runs the conformance fake gate until interrupted.
func cmdFakeServer(args []string) (int, error) {
	if len(args) != 1 {
		return 1, errors.New("usage: basalt-gate fake-server SOCKET")
	}
	_ = os.Remove(args[0])
	ln, err := net.Listen("unix", args[0])
	if err != nil {
		return 1, err
	}
	defer ln.Close()
	f := conformance.NewFakeServer()
	go f.Serve(ln)
	fmt.Printf("Fake gate on %s (BASALT_GATE_SOCKET=%s). Ctrl-C prints the verdict.\n", args[0], args[0])
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	seen := 0
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			l := f.Log()
			for _, x := range l[seen:] {
				fmt.Println("<-", x)
			}
			seen = len(l)
		case <-sig:
			if err := f.Check(); err != nil {
				fmt.Println("NOT CONFORMING:", err)
				return 1, nil
			}
			fmt.Println("conforming: no protocol violations")
			return 0, nil
		}
	}
}
