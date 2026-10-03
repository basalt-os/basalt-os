package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
)

// Case schemas this tool reads. v1.1 adds evidence.snapshots (optional for
// v1 readers, which ignore unknown evidence keys) and requires it for every
// expected action that names a snapshot.
const (
	SchemaV1  = "basalt-case/v1"
	SchemaV11 = "basalt-case/v1.1"
)

var knownSchemas = map[string]bool{SchemaV1: true, SchemaV11: true}

// Roles of a snapshot in a case's evidence (docs/eval-suite.md).
const (
	RoleTransactionPre  = "transaction_pre"  // taken just before a dnf transaction or an apply
	RoleTransactionPost = "transaction_post" // taken just after it
	RoleRestoreSource   = "restore_source"   // holds another copy of a file the unit needs
	RoleSpaceHolder     = "space_holder"     // holds space exclusively (disk cases)
)

var snapshotRoles = map[string]bool{RoleTransactionPre: true, RoleTransactionPost: true, RoleRestoreSource: true, RoleSpaceHolder: true}

// SnapshotEvidence is one snapshot the diagnosers saw for a case.
type SnapshotEvidence struct {
	Number         int    `json:"number"`
	Role           string `json:"role"`
	Type           string `json:"type,omitempty"`       // snapper type: pre, post, single
	PreNumber      int    `json:"pre_number,omitempty"` // for a post snapshot: its pre snapshot
	Date           string `json:"date,omitempty"`
	Description    string `json:"description,omitempty"`
	Path           string `json:"path,omitempty"` // restore_source: the file it holds
	How            string `json:"how,omitempty"`  // restore_source: how the copy differs
	ExclusiveBytes int64  `json:"exclusive_bytes,omitempty"`
}

type checkedCase struct {
	ID       string `json:"id"`
	Schema   string `json:"schema"`
	Evidence struct {
		Snapshots []SnapshotEvidence `json:"snapshots"`
	} `json:"evidence"`
	Expected struct {
		Actions  []action.Action `json:"actions"`
		Proposal string          `json:"proposal"`
	} `json:"expected"`
}

// checkCase validates one case line: known schema, every expected action
// passes the assistant's own validator (action.Validate), the snapshot
// evidence is well formed, and (v1.1) every snapshot an action names is in
// the evidence with the role that action needs.
func checkCase(line []byte) (string, error) {
	var c checkedCase
	if err := json.Unmarshal(line, &c); err != nil {
		return "", err
	}
	if c.ID == "" {
		return "", errors.New("case without id")
	}
	var errs []error
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if !knownSchemas[c.Schema] {
		fail("schema %q", c.Schema)
	}
	if c.Expected.Proposal != "propose" && c.Expected.Proposal != "review" {
		fail("proposal %q", c.Expected.Proposal)
	}
	byNumber := map[int][]SnapshotEvidence{}
	for _, s := range c.Evidence.Snapshots {
		if s.Number <= 0 {
			fail("snapshot number %d", s.Number)
		}
		if !snapshotRoles[s.Role] {
			fail("snapshot %d: role %q", s.Number, s.Role)
		}
		if s.Type != "" && s.Type != "pre" && s.Type != "post" && s.Type != "single" {
			fail("snapshot %d: type %q", s.Number, s.Type)
		}
		if s.PreNumber != 0 && (s.Type != "post" || s.PreNumber >= s.Number) {
			fail("snapshot %d: pre_number %d", s.Number, s.PreNumber)
		}
		if s.Role == RoleRestoreSource && s.Path == "" {
			fail("snapshot %d: restore_source without path", s.Number)
		}
		byNumber[s.Number] = append(byNumber[s.Number], s)
	}
	// A transaction_post names its transaction_pre, when both are recorded.
	for _, s := range c.Evidence.Snapshots {
		if s.Role == RoleTransactionPost && s.PreNumber != 0 && !hasRole(byNumber[s.PreNumber], RoleTransactionPre, "") {
			fail("snapshot %d: pre snapshot %d not in the evidence", s.Number, s.PreNumber)
		}
	}
	for i, a := range c.Expected.Actions {
		if err := a.Validate(); err != nil {
			fail("action %d (%s): %v", i, a.Kind, err)
			continue
		}
		if c.Schema == SchemaV1 {
			continue // v1 cases carry no snapshot evidence
		}
		n, _ := strconv.Atoi(a.Params["snapshot"])
		switch a.Kind {
		case action.FileRestore:
			if !hasRole(byNumber[n], RoleRestoreSource, a.Params["path"]) {
				fail("action %d (%s): snapshot %d holding %s not in evidence.snapshots", i, a.Kind, n, a.Params["path"])
			}
		case action.SnapshotRollback:
			if !hasRole(byNumber[n], RoleTransactionPre, "") {
				fail("action %d (%s): pre snapshot %d not in evidence.snapshots", i, a.Kind, n)
			}
		case action.SnapshotDelete:
			if !hasRole(byNumber[n], RoleSpaceHolder, "") {
				fail("action %d (%s): snapshot %d holding space not in evidence.snapshots", i, a.Kind, n)
			}
		}
	}
	return c.ID, errors.Join(errs...)
}

func hasRole(ss []SnapshotEvidence, role, path string) bool {
	for _, s := range ss {
		if s.Role == role && (path == "" || s.Path == path) {
			return true
		}
	}
	return false
}

// checkDir validates every case of every *.jsonl file in dir; ids must be
// unique across files. It returns the number of cases checked.
func checkDir(dir string) (int, error) {
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(files) == 0 {
		return 0, fmt.Errorf("no case files in %s", dir)
	}
	seen := map[string]string{}
	var errs []error
	n := 0
	for _, f := range files {
		line := 0
		if err := readJSONL(f, func(b []byte) error {
			line++
			id, err := checkCase(b)
			if id != "" {
				if prev, dup := seen[id]; dup {
					errs = append(errs, fmt.Errorf("%s: id %s also in %s", f, id, prev))
				}
				seen[id] = f
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %s: %w", filepath.Base(f), id, err))
			}
			n++
			return nil
		}); err != nil {
			errs = append(errs, err)
		}
	}
	return n, errors.Join(errs...)
}

func runCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	dir := fs.String("cases", "eval/cases", "directory of *.jsonl case files")
	_ = fs.Parse(args)
	n, err := checkDir(*dir)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "%d cases ok\n", n)
	return nil
}
