// Package mcp is a Model Context Protocol server over stdio (JSON-RPC 2.0,
// one message per line). It exposes the diagnosers to a future local model
// or to an external MCP client. Read tools return diagnoses. Write tools
// never change the system: they store a proposal and return its id and
// exact commands; a person applies it with `sudo basalt apply <id>`, which
// shows the commands again and asks for confirmation.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/audit"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/decide"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/diag"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/report"
)

// ProtocolVersion this server speaks.
const ProtocolVersion = "2025-06-18"

// Server holds the tools' dependencies.
type Server struct {
	Env     *diag.Env
	Store   proposal.Store
	Audit   *audit.Log
	Decide  *decide.Layer
	Disk    diag.DiskThresholds
	Version string
}

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

// Tool describes one tool.
type Tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
	handler     func(ctx context.Context, args map[string]any) (string, any, error)
	write       bool
}

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }

// Tools lists the server's tools.
func (s *Server) Tools() []Tool {
	ro := map[string]any{"readOnlyHint": true, "openWorldHint": false}
	prop := map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": false, "openWorldHint": false}
	return []Tool{
		{Name: "basalt_status", Title: "System health", Description: "Health summary: SELinux mode, failed units, recent denials, disk, snapshots, pending proposals.",
			InputSchema: obj(map[string]any{}), Annotations: ro, handler: s.status},
		{Name: "basalt_why_unit", Title: "Diagnose a service", Description: "Diagnose a systemd unit: state, journal errors, SELinux denials for its domain, failed dependencies, port conflicts, full disks, config errors. Returns the cause with probabilities and the proposed actions (not applied).",
			InputSchema: obj(map[string]any{"unit": str("unit name, e.g. nginx or nginx.service")}, "unit"), Annotations: ro, handler: s.whyUnit},
		{Name: "basalt_selinux_denials", Title: "Analyze SELinux denials", Description: "Group recent AVC denials and map each to a known fix (restorecon, semanage fcontext, semanage port, setsebool) or to review. Never generates policy.",
			InputSchema: obj(map[string]any{"since_minutes": num("look back this many minutes (default 60)")}), Annotations: ro, handler: s.denials},
		{Name: "basalt_disk", Title: "Disk usage", Description: "Root file system usage, journal and package cache size, fullness forecast, and what to reclaim.",
			InputSchema: obj(map[string]any{}), Annotations: ro, handler: s.disk},
		{Name: "basalt_snapshots", Title: "List snapshots", Description: "Snapper snapshots of the root (number, type, date, description).",
			InputSchema: obj(map[string]any{}), Annotations: ro, handler: s.snapshots},
		{Name: "basalt_snapshot_diff", Title: "Compare snapshots", Description: "Package differences between two snapshots (0 is the running system).",
			InputSchema: obj(map[string]any{"from": num("snapshot number"), "to": num("snapshot number, 0 for now")}, "from"), Annotations: ro, handler: s.snapDiff},
		{Name: "basalt_pending", Title: "Pending proposals", Description: "Proposals waiting for a person (or with another status).",
			InputSchema: obj(map[string]any{"status": str("pending (default), applied, failed, ignored, or all")}), Annotations: ro, handler: s.pending},
		{Name: "basalt_proposal", Title: "Show a proposal", Description: "One proposal: report, evidence, decision, exact commands.",
			InputSchema: obj(map[string]any{"id": str("proposal id")}, "id"), Annotations: ro, handler: s.show},
		{Name: "basalt_audit_tail", Title: "Audit log", Description: "The last records of the assistant's hash-chained audit log and whether the chain verifies.",
			InputSchema: obj(map[string]any{"n": num("records (default 20)")}), Annotations: ro, handler: s.auditTail},

		{Name: "basalt_propose_unit_fix", Title: "Propose a service fix", write: true,
			Description: "Diagnose a unit and store the proposed fix as a proposal. Nothing is executed: a person must run `sudo basalt apply <id>` and confirm.",
			InputSchema: obj(map[string]any{"unit": str("unit name")}, "unit"), Annotations: prop, handler: s.proposeUnit},
		{Name: "basalt_propose_selinux_fix", Title: "Propose SELinux fixes", write: true,
			Description: "Store a proposal for each recent denial with a confident known fix. Nothing is executed.",
			InputSchema: obj(map[string]any{"since_minutes": num("look back this many minutes (default 60)")}), Annotations: prop, handler: s.proposeSELinux},
		{Name: "basalt_propose_rollback", Title: "Propose a rollback", write: true,
			Description: "Store a proposal to make a snapshot the root at the next boot, with the package differences. Nothing is executed.",
			InputSchema: obj(map[string]any{"snapshot": num("snapshot number"), "reason": str("why, shown to the person")}, "snapshot"), Annotations: prop, handler: s.proposeRollback},
		{Name: "basalt_propose_disk_cleanup", Title: "Propose disk cleanup", write: true,
			Description: "Store the disk report's proposed cleanup (journal vacuum, package cache, a snapshot) as a proposal. Nothing is executed.",
			InputSchema: obj(map[string]any{}), Annotations: prop, handler: s.proposeDisk},
		{Name: "basalt_propose_action", Title: "Propose a typed action", write: true,
			Description: "Store a proposal for one action of the closed set (" + strings.Join(kinds(), ", ") + ") with validated parameters. Nothing is executed.",
			InputSchema: obj(map[string]any{
				"kind":   map[string]any{"type": "string", "enum": kinds()},
				"params": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
				"reason": str("why, shown to the person")}, "kind", "params", "reason"),
			Annotations: prop, handler: s.proposeAction},
	}
}

func kinds() []string {
	return []string{action.SELinuxFcontext, action.SELinuxRestorecon, action.SELinuxPort, action.SELinuxBoolean,
		action.UnitRestart, action.FileRestore, action.SnapshotRollback, action.SnapshotDelete, action.JournalVacuum, action.DnfClean}
}

// Serve handles requests until EOF.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	enc := json.NewEncoder(out)
	tools := map[string]Tool{}
	for _, t := range s.Tools() {
		tools[t.Name] = t
	}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req rpcReq
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			_ = enc.Encode(rpcResp{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcErr{-32700, "parse error"}})
			continue
		}
		if len(req.ID) == 0 {
			continue // notification (notifications/initialized, cancelled)
		}
		resp := rpcResp{JSONRPC: "2.0", ID: req.ID}
		switch req.Method {
		case "initialize":
			resp.Result = map[string]any{
				"protocolVersion": ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
				"serverInfo":      map[string]any{"name": "basalt-assistant", "title": "Basalt OS assistant", "version": s.Version},
				"instructions": "Diagnosis tools are read-only. Tools named basalt_propose_* only store a proposal; " +
					"no tool executes a change. Tell the person the proposal id: they review it and run `sudo basalt apply <id>`.",
			}
		case "ping":
			resp.Result = map[string]any{}
		case "tools/list":
			list := s.Tools()
			resp.Result = map[string]any{"tools": list}
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil {
				resp.Error = &rpcErr{-32602, "invalid params"}
				break
			}
			t, ok := tools[p.Name]
			if !ok {
				resp.Error = &rpcErr{-32602, "unknown tool " + p.Name}
				break
			}
			cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			text, structured, err := t.handler(cctx, p.Arguments)
			cancel()
			if err != nil {
				resp.Result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "error: " + err.Error()}}, "isError": true}
				break
			}
			r := map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
			if structured != nil {
				r["structuredContent"] = structured
			}
			resp.Result = r
		default:
			resp.Error = &rpcErr{-32601, "method not found: " + req.Method}
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

// --- read tools ------------------------------------------------------------------

func toJSON(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

// wrap returns structured content (MCP requires an object).
func wrap(v any) any {
	b, _ := json.Marshal(v)
	var m map[string]any
	if json.Unmarshal(b, &m) == nil {
		return m
	}
	return map[string]any{"items": v}
}

func intArg(args map[string]any, k string, def int) int {
	switch v := args[k].(type) {
	case float64:
		return int(v)
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func strArg(args map[string]any, k string) string {
	if v, ok := args[k].(string); ok {
		return v
	}
	return ""
}

func (s *Server) status(ctx context.Context, _ map[string]any) (string, any, error) {
	ps, _ := s.Store.List(proposal.Pending)
	st := s.Env.GetStatus(ctx, len(ps))
	return toJSON(st), wrap(st), nil
}

func (s *Server) whyUnit(ctx context.Context, args map[string]any) (string, any, error) {
	rep, err := diag.WhyUnit(ctx, s.Env, strArg(args, "unit"))
	if err != nil {
		return "", nil, err
	}
	p := report.FromUnit("mcp", rep)
	p.ID = "preview"
	return mcpText(p), wrap(rep), nil
}

func (s *Server) denials(ctx context.Context, args map[string]any) (string, any, error) {
	since := time.Now().Add(-time.Duration(intArg(args, "since_minutes", 60)) * time.Minute)
	items := s.Env.FixSELinux(ctx, since)
	var b strings.Builder
	if len(items) == 0 {
		b.WriteString("No SELinux denials in that period.\n")
	}
	for _, it := range items {
		p := report.FromSELinux("mcp", it)
		p.ID = "preview"
		b.WriteString(mcpText(p) + "\n")
	}
	return b.String(), map[string]any{"items": items}, nil
}

func (s *Server) disk(ctx context.Context, _ map[string]any) (string, any, error) {
	rep, err := s.Env.Disk(ctx, s.Disk, 30)
	if err != nil {
		return "", nil, err
	}
	p := report.FromDisk("mcp", rep)
	p.ID = "preview"
	return mcpText(p), wrap(rep), nil
}

func (s *Server) snapshots(ctx context.Context, _ map[string]any) (string, any, error) {
	snaps := s.Env.Snapshots(ctx)
	var b strings.Builder
	for _, sn := range snaps {
		fmt.Fprintf(&b, "%4d %-6s %4s %s %s\n", sn.Number, sn.Type, preStr(sn.Pre), sn.Date, sn.Description)
	}
	return b.String(), map[string]any{"snapshots": snaps}, nil
}

func preStr(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func (s *Server) snapDiff(ctx context.Context, args map[string]any) (string, any, error) {
	from, to := intArg(args, "from", -1), intArg(args, "to", 0)
	if from < 0 {
		return "", nil, fmt.Errorf("from is required")
	}
	d := s.Env.DiffPackages(ctx, from, to)
	return toJSON(d), wrap(d), nil
}

func (s *Server) pending(_ context.Context, args map[string]any) (string, any, error) {
	st := strArg(args, "status")
	if st == "" {
		st = proposal.Pending
	}
	if st == "all" {
		st = ""
	}
	ps, err := s.Store.List(st)
	if err != nil {
		return "", nil, err
	}
	var b strings.Builder
	for _, p := range ps {
		b.WriteString(report.Line(p) + "\n")
	}
	if len(ps) == 0 {
		b.WriteString("none\n")
	}
	return b.String(), map[string]any{"proposals": ps}, nil
}

func (s *Server) show(_ context.Context, args map[string]any) (string, any, error) {
	p, err := s.Store.Load(strArg(args, "id"))
	if err != nil {
		return "", nil, err
	}
	return mcpText(p), wrap(p), nil
}

func (s *Server) auditTail(_ context.Context, args map[string]any) (string, any, error) {
	rs, err := audit.Tail(s.Audit.Path, intArg(args, "n", 20))
	if err != nil {
		return "", nil, err
	}
	n, verr := audit.Verify(s.Audit.Path)
	chain := fmt.Sprintf("chain verifies: %d records", n)
	if verr != nil {
		chain = "CHAIN BROKEN: " + verr.Error()
	}
	var b strings.Builder
	b.WriteString(chain + "\n")
	for _, r := range rs {
		fmt.Fprintf(&b, "#%d %s %s %s: %s\n", r.Seq, r.Time.Format(time.RFC3339), r.Actor.Program, r.Type, r.Text)
	}
	return b.String(), map[string]any{"chain": chain, "records": rs}, nil
}

// --- proposal tools --------------------------------------------------------------

func (s *Server) store(ctx context.Context, p *proposal.Proposal) (string, any, error) {
	// The MCP server sees the system through the confined view: a file
	// restore or a rollback is stored as a hint for the root view to confirm.
	p.HoldForRoot("proposed through MCP; the confined view cannot check the snapshot")
	if len(p.Actions) == 0 && len(p.Hints) == 0 {
		return mcpText(p) + "\nNo change to propose; nothing was stored.\n", map[string]any{"stored": false}, nil
	}
	if open := s.Store.FindOpen(p.Key); open != nil {
		return fmt.Sprintf("An equivalent proposal is already pending: %s. A person applies it with: sudo basalt apply %s\n", open.ID, open.ID),
			map[string]any{"stored": false, "id": open.ID}, nil
	}
	for i := range p.Decisions {
		p.Decisions[i] = s.Decide.Record(p.Decisions[i], "propose "+p.ID+" (mcp)")
	}
	if err := s.Store.Save(p); err != nil {
		return "", nil, err
	}
	cmds, _ := p.Commands()
	if s.Audit != nil {
		_, _ = s.Audit.Append("proposal", p.ID+" from mcp: "+p.Title, map[string]any{"proposal": p.ID, "actions": p.Actions, "commands": cmds})
	}
	text := mcpText(p)
	if len(p.Actions) == 0 {
		text += "\nStored as " + p.ID + " as a hint. Nothing was executed. A person confirms the snapshot as root with: sudo basalt confirm " +
			p.ID + " (it becomes a proposal they can apply)\n"
		return text, map[string]any{"stored": true, "id": p.ID, "hint": true, "needs_review": true}, nil
	}
	text += "\nStored as " + p.ID + ". Nothing was executed. A person reviews and applies it with: sudo basalt apply " + p.ID + "\n"
	return text, map[string]any{"stored": true, "id": p.ID, "commands": cmds, "needs_review": p.NeedsReview}, nil
}

// mcpText renders a proposal for an MCP client: the confirmation code is
// for the person at the command line, never for the client.
func mcpText(p *proposal.Proposal) string {
	return report.RenderWith(p, report.Options{NoCode: true}).String()
}

func (s *Server) proposeUnit(ctx context.Context, args map[string]any) (string, any, error) {
	rep, err := diag.WhyUnit(ctx, s.Env, strArg(args, "unit"))
	if err != nil {
		return "", nil, err
	}
	return s.store(ctx, report.FromUnit("mcp", rep))
}

func (s *Server) proposeSELinux(ctx context.Context, args map[string]any) (string, any, error) {
	since := time.Now().Add(-time.Duration(intArg(args, "since_minutes", 60)) * time.Minute)
	var b strings.Builder
	var ids []string
	for _, it := range s.Env.FixSELinux(ctx, since) {
		t, st, err := s.store(ctx, report.FromSELinux("mcp", it))
		if err != nil {
			return "", nil, err
		}
		b.WriteString(t + "\n")
		if m, ok := st.(map[string]any); ok && m["id"] != nil {
			ids = append(ids, fmt.Sprint(m["id"]))
		}
	}
	if b.Len() == 0 {
		b.WriteString("No denials in that period.\n")
	}
	return b.String(), map[string]any{"ids": ids}, nil
}

func (s *Server) proposeRollback(ctx context.Context, args map[string]any) (string, any, error) {
	n := intArg(args, "snapshot", 0)
	plan, err := s.Env.PlanRollback(ctx, n)
	if err != nil {
		return "", nil, err
	}
	why := strArg(args, "reason")
	if why == "" {
		why = "Requested through MCP."
	}
	return s.store(ctx, report.FromRollback("mcp", plan, why, ""))
}

func (s *Server) proposeDisk(ctx context.Context, _ map[string]any) (string, any, error) {
	rep, err := s.Env.Disk(ctx, s.Disk, 30)
	if err != nil {
		return "", nil, err
	}
	return s.store(ctx, report.FromDisk("mcp", rep))
}

func (s *Server) proposeAction(ctx context.Context, args map[string]any) (string, any, error) {
	a := action.Action{Kind: strArg(args, "kind"), Params: map[string]string{}}
	if m, ok := args["params"].(map[string]any); ok {
		for k, v := range m {
			a.Params[k] = fmt.Sprint(v)
		}
	}
	if err := a.Validate(); err != nil {
		return "", nil, err
	}
	now := time.Now().UTC()
	p := &proposal.Proposal{ID: proposal.NewID(), Created: now, Updated: now, LastSeen: now, Seen: 1, Source: "mcp",
		Kind: "action", Subject: a.Kind, Key: "mcp:" + a.Kind + ":" + toJSON(a.Params), Title: a.Describe(),
		Report: "Proposed by an MCP client: " + strArg(args, "reason"), Actions: []action.Action{a},
		NeedsReview: true, Status: proposal.Pending,
		Evidence: []string{"no diagnosis: the client chose this action; review it before applying"}}
	return s.store(ctx, p)
}
