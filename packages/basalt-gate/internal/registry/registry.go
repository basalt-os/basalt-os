// Package registry loads the action registry: the closed sets of actions
// a request may name, from /usr/share/basalt/gate/actions.d/*.json. The
// registry, never the client, defines an action: the schema of its
// arguments, its risk class, which resources it touches (paths, units,
// packages, hosts, recipients), whether only a person or only an agent
// may propose it, which executor runs it, and the preview the gate builds
// when the requester sends none.
package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/proposal"
)

// DefaultDir holds the registry files.
const DefaultDir = "/usr/share/basalt/gate/actions.d"

// FileVersion is the registry file format version.
const FileVersion = 1

// Arg is the schema of one argument.
type Arg struct {
	Type     string   `json:"type"` // string, integer, boolean, strings, list (array of objects), object
	Required bool     `json:"required,omitempty"`
	Pattern  string   `json:"pattern,omitempty"` // string, strings: anchored regular expression
	Enum     []string `json:"enum,omitempty"`
	MaxLen   int      `json:"max_len,omitempty"`   // string (default 4096)
	Min      *int64   `json:"min,omitempty"`       // integer
	Max      *int64   `json:"max,omitempty"`       // integer
	MaxItems int      `json:"max_items,omitempty"` // strings, list (default 1000)
	// Fields is the schema of each object of a list, or of an object.
	Fields map[string]Arg `json:"fields,omitempty"`
	re     *regexp.Regexp
}

// Extractor names the arguments that are resources of a kind.
type Extractor struct {
	Kind string `json:"kind"`
	// Arg is an argument name, "name[]" for each element of a strings or
	// list argument, "name[].field" for a field of each list element, or
	// "=value" for a fixed resource.
	Arg        string `json:"arg"`
	Beneath    bool   `json:"beneath,omitempty"`     // a path and everything below it
	ExpandHome bool   `json:"expand_home,omitempty"` // ~ is the requester's home
	BytesArg   string `json:"bytes_arg,omitempty"`   // the size of the resource, when known
}

// PreviewDef says how the gate builds a preview when the requester sends
// none: a title key and the arguments it shows.
type PreviewDef struct {
	TitleKey string   `json:"title_key,omitempty"`
	Show     []string `json:"show,omitempty"`
	// Commands is the argument holding the exact command line (argv),
	// shown to the person as one shell-quoted line.
	Commands string `json:"commands,omitempty"`
}

// Action is one registered action.
type Action struct {
	ID          string         `json:"id"`
	Class       string         `json:"class"`
	Description string         `json:"description,omitempty"`
	Args        map[string]Arg `json:"args"`
	// Open: arguments outside Args are accepted (bounded in size) and kept;
	// for actions whose planner owns the schema (desktop actions).
	Open       bool        `json:"open,omitempty"`
	Resources  []Extractor `json:"resources,omitempty"`
	System     bool        `json:"system,omitempty"`      // a root-level change: only system rules may cover it
	PersonOnly bool        `json:"person_only,omitempty"` // only the person's own words may ask for it
	AgentOnly  bool        `json:"agent_only,omitempty"`  // only an agent connection may ask for it
	Leaves     bool        `json:"leaves,omitempty"`      // something leaves the machine
	Reversible string      `json:"reversible,omitempty"`  // trash, snapshot, undo-log, none
	Snapshot   bool        `json:"snapshot,omitempty"`    // the executor takes a snapshot first
	Executor   string      `json:"executor"`
	Preview    PreviewDef  `json:"preview,omitempty"`
	// Requesters limits who may propose it (kinds); empty: anyone.
	Requesters []string `json:"requesters,omitempty"`
	// ClassFromHint: the call's "class_hint" argument may raise the class
	// (never lower it); a missing hint counts as HintDefault.
	ClassFromHint bool   `json:"class_from_hint,omitempty"`
	HintDefault   string `json:"hint_default,omitempty"`
	// DestructiveArg: when this boolean argument is true, the class is C5.
	DestructiveArg string `json:"destructive_arg,omitempty"`
	Source         string `json:"-"`
}

// Executor is who may claim a decision to run it.
type Executor struct {
	// Types are the SELinux types allowed to claim; empty with
	// Requester set means the requester itself runs the action.
	Types []string `json:"types,omitempty"`
	// UID the executor must run as (-1: any).
	UID       *int   `json:"uid,omitempty"`
	Requester bool   `json:"requester,omitempty"` // the requester runs it itself (tool.exec)
	Internal  bool   `json:"internal,omitempty"`  // the gate itself (rule changes)
	Note      string `json:"note,omitempty"`
}

// ToolOperation is an operation a tool's package declares (tool-NAME.json):
// the class the gate uses for tool.exec calls naming it, never lower than
// the tool's own hint.
type ToolOperation struct {
	Class string `json:"class"`
}

// File is one registry file.
type File struct {
	Registry  int                                 `json:"registry"`
	Source    string                              `json:"source"`
	Executors map[string]Executor                 `json:"executors,omitempty"`
	Groups    map[string][]string                 `json:"groups,omitempty"`
	Actions   []Action                            `json:"actions"`
	Tools     map[string]map[string]ToolOperation `json:"tools,omitempty"`
}

// Registry is the loaded set.
type Registry struct {
	Actions   map[string]*Action
	Executors map[string]Executor
	Groups    map[string][]string
	Tools     map[string]map[string]ToolOperation
	Files     []string
}

var (
	argNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)
	kindRe    = regexp.MustCompile(`^(path|unit|package|host|recipient|boolean|user|disk|rule|module|key|device|setting|window|app|session|model)$`)
	execRe    = regexp.MustCompile(`^[a-z][a-z0-9-]{1,40}$`)
)

// Load reads every *.json file of dir (sorted). An action defined twice
// is an error: one id, one meaning.
func Load(dir string) (*Registry, error) {
	names, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	r := New()
	for _, n := range names {
		b, err := os.ReadFile(n)
		if err != nil {
			return nil, err
		}
		if err := r.Add(filepath.Base(n), b); err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
	}
	return r, r.Check()
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{Actions: map[string]*Action{}, Executors: map[string]Executor{}, Groups: map[string][]string{},
		Tools: map[string]map[string]ToolOperation{}}
}

// Add parses one registry file into r.
func (r *Registry) Add(name string, b []byte) error {
	var f File
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return err
	}
	if f.Registry != FileVersion {
		return fmt.Errorf("registry format %d, want %d", f.Registry, FileVersion)
	}
	for n, e := range f.Executors {
		if !execRe.MatchString(n) {
			return fmt.Errorf("executor name %q", n)
		}
		if _, dup := r.Executors[n]; dup {
			return fmt.Errorf("executor %s defined twice", n)
		}
		r.Executors[n] = e
	}
	for g, ids := range f.Groups {
		r.Groups[g] = append(r.Groups[g], ids...)
	}
	for tool, ops := range f.Tools {
		if r.Tools[tool] == nil {
			r.Tools[tool] = map[string]ToolOperation{}
		}
		for op, d := range ops {
			if proposal.ClassRank(d.Class) < 0 {
				return fmt.Errorf("tool %s operation %s: class %q", tool, op, d.Class)
			}
			r.Tools[tool][op] = d
		}
	}
	for i := range f.Actions {
		a := f.Actions[i]
		a.Source = name
		if err := a.compile(); err != nil {
			return fmt.Errorf("action %s: %w", a.ID, err)
		}
		if _, dup := r.Actions[a.ID]; dup {
			return fmt.Errorf("action %s defined twice", a.ID)
		}
		r.Actions[a.ID] = &a
	}
	r.Files = append(r.Files, name)
	return nil
}

// Check verifies cross references: executors and group members exist.
func (r *Registry) Check() error {
	for _, a := range r.Actions {
		if _, ok := r.Executors[a.Executor]; !ok {
			return fmt.Errorf("action %s: unknown executor %q", a.ID, a.Executor)
		}
	}
	for g, ids := range r.Groups {
		for _, id := range ids {
			if r.Actions[id] == nil {
				return fmt.Errorf("group %s: unknown action %s", g, id)
			}
		}
	}
	return nil
}

func (a *Action) compile() error {
	if !proposal.ActionRe.MatchString(a.ID) {
		return errors.New("bad action id")
	}
	if proposal.ClassRank(a.Class) < 0 {
		return fmt.Errorf("class %q", a.Class)
	}
	if a.PersonOnly && a.AgentOnly {
		return errors.New("person_only and agent_only together")
	}
	if !execRe.MatchString(a.Executor) {
		return fmt.Errorf("executor %q", a.Executor)
	}
	for _, k := range a.Requesters {
		if !proposal.Kinds[k] {
			return fmt.Errorf("requester kind %q", k)
		}
	}
	if a.HintDefault != "" && proposal.ClassRank(a.HintDefault) < 0 {
		return fmt.Errorf("hint_default %q", a.HintDefault)
	}
	switch a.Reversible {
	case "", "trash", "snapshot", "undo-log", "none":
	default:
		return fmt.Errorf("reversible %q", a.Reversible)
	}
	if err := compileArgs(a.Args); err != nil {
		return err
	}
	for _, e := range a.Resources {
		if !kindRe.MatchString(e.Kind) {
			return fmt.Errorf("resource kind %q", e.Kind)
		}
		if e.Arg == "" {
			return errors.New("resource without arg")
		}
	}
	return nil
}

func compileArgs(m map[string]Arg) error {
	for n, s := range m {
		if !argNameRe.MatchString(n) {
			return fmt.Errorf("argument name %q", n)
		}
		switch s.Type {
		case "string", "integer", "boolean", "strings", "list", "object":
		default:
			return fmt.Errorf("argument %s: type %q", n, s.Type)
		}
		if s.Pattern != "" {
			re, err := regexp.Compile("^(?:" + s.Pattern + ")$")
			if err != nil {
				return fmt.Errorf("argument %s: %w", n, err)
			}
			s.re = re
		}
		if err := compileArgs(s.Fields); err != nil {
			return fmt.Errorf("argument %s: %w", n, err)
		}
		m[n] = s
	}
	return nil
}

// ErrUnknown is returned for an action that is not registered.
var ErrUnknown = errors.New("unknown action")

// Lookup returns the action definition.
func (r *Registry) Lookup(id string) (*Action, error) {
	a := r.Actions[id]
	if a == nil {
		return nil, fmt.Errorf("%w %q", ErrUnknown, id)
	}
	return a, nil
}

// Expand returns the action ids a rule selector names: an id, "group:G",
// "class:Cn" or "*". Unknown ids are kept (a rule may name an action a
// later package registers).
func (r *Registry) Expand(sel string) []string {
	switch {
	case sel == "*":
		return r.ids(func(*Action) bool { return true })
	case strings.HasPrefix(sel, "group:"):
		return append([]string(nil), r.Groups[strings.TrimPrefix(sel, "group:")]...)
	case strings.HasPrefix(sel, "class:"):
		c := strings.TrimPrefix(sel, "class:")
		return r.ids(func(a *Action) bool { return a.Class == c })
	}
	return []string{sel}
}

func (r *Registry) ids(f func(*Action) bool) []string {
	var out []string
	for id, a := range r.Actions {
		if f(a) {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// InGroup reports whether action id is in group g.
func (r *Registry) InGroup(g, id string) bool {
	for _, x := range r.Groups[g] {
		if x == id {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Validation

// Validate checks a call's arguments against the schema and returns them
// normalized (numbers as int64, unknown arguments refused unless Open).
func (a *Action) Validate(args map[string]any) (map[string]any, error) {
	if args == nil {
		args = map[string]any{}
	}
	out := map[string]any{}
	for n, s := range a.Args {
		v, ok := args[n]
		if !ok || v == nil {
			if s.Required {
				return nil, fmt.Errorf("argument %s is required", n)
			}
			continue
		}
		nv, err := checkArg(n, s, v, 0)
		if err != nil {
			return nil, err
		}
		out[n] = nv
	}
	for n, v := range args {
		if _, known := a.Args[n]; known {
			continue
		}
		if !a.Open {
			return nil, fmt.Errorf("unknown argument %s", n)
		}
		if !argNameRe.MatchString(n) {
			return nil, fmt.Errorf("argument name %q", n)
		}
		nv, err := normalizeAny(v, 0)
		if err != nil {
			return nil, fmt.Errorf("argument %s: %w", n, err)
		}
		out[n] = nv
	}
	if b, err := json.Marshal(out); err != nil || len(b) > 64<<10 {
		return nil, errors.New("arguments unencodable or larger than 64 KiB")
	}
	return out, nil
}

func checkArg(n string, s Arg, v any, depth int) (any, error) {
	if depth > 4 {
		return nil, fmt.Errorf("argument %s nested too deep", n)
	}
	switch s.Type {
	case "string":
		str, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("argument %s must be a string", n)
		}
		return str, checkString(n, s, str)
	case "integer":
		i, err := toInt(v)
		if err != nil {
			return nil, fmt.Errorf("argument %s: %w", n, err)
		}
		if (s.Min != nil && i < *s.Min) || (s.Max != nil && i > *s.Max) {
			return nil, fmt.Errorf("argument %s out of range", n)
		}
		return i, nil
	case "boolean":
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("argument %s must be true or false", n)
		}
		return b, nil
	case "strings":
		l, ok := v.([]any)
		if !ok {
			if sl, ok2 := v.([]string); ok2 {
				l = make([]any, len(sl))
				for i, x := range sl {
					l[i] = x
				}
			} else {
				return nil, fmt.Errorf("argument %s must be a list of strings", n)
			}
		}
		if len(l) > maxItems(s) {
			return nil, fmt.Errorf("argument %s has more than %d items", n, maxItems(s))
		}
		out := make([]any, len(l))
		for i, e := range l {
			str, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("argument %s must be a list of strings", n)
			}
			if err := checkString(n, s, str); err != nil {
				return nil, err
			}
			out[i] = str
		}
		return out, nil
	case "list":
		l, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("argument %s must be a list", n)
		}
		if len(l) > maxItems(s) {
			return nil, fmt.Errorf("argument %s has more than %d items", n, maxItems(s))
		}
		out := make([]any, len(l))
		for i, e := range l {
			m, ok := e.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("argument %s must be a list of objects", n)
			}
			nm, err := checkObject(n, s, m, depth)
			if err != nil {
				return nil, err
			}
			out[i] = nm
		}
		return out, nil
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("argument %s must be an object", n)
		}
		return checkObject(n, s, m, depth)
	}
	return nil, fmt.Errorf("argument %s: no schema", n)
}

func checkObject(n string, s Arg, m map[string]any, depth int) (map[string]any, error) {
	if s.Fields == nil {
		v, err := normalizeAny(m, depth+1)
		if err != nil {
			return nil, fmt.Errorf("argument %s: %w", n, err)
		}
		return v.(map[string]any), nil
	}
	out := map[string]any{}
	for fn, fs := range s.Fields {
		fv, ok := m[fn]
		if !ok || fv == nil {
			if fs.Required {
				return nil, fmt.Errorf("argument %s.%s is required", n, fn)
			}
			continue
		}
		nv, err := checkArg(n+"."+fn, fs, fv, depth+1)
		if err != nil {
			return nil, err
		}
		out[fn] = nv
	}
	for fn := range m {
		if _, ok := s.Fields[fn]; !ok {
			return nil, fmt.Errorf("unknown field %s.%s", n, fn)
		}
	}
	return out, nil
}

func maxItems(s Arg) int {
	if s.MaxItems > 0 {
		return s.MaxItems
	}
	return 1000
}

func checkString(n string, s Arg, str string) error {
	max := s.MaxLen
	if max == 0 {
		max = 4096
	}
	if len(str) > max {
		return fmt.Errorf("argument %s longer than %d bytes", n, max)
	}
	if strings.ContainsRune(str, 0) {
		return fmt.Errorf("argument %s contains a NUL byte", n)
	}
	if len(s.Enum) > 0 {
		ok := false
		for _, e := range s.Enum {
			ok = ok || e == str
		}
		if !ok {
			return fmt.Errorf("argument %s: %q is not one of %s", n, str, strings.Join(s.Enum, ", "))
		}
	}
	if s.re != nil && !s.re.MatchString(str) {
		return fmt.Errorf("argument %s: %q has an unexpected form", n, str)
	}
	return nil
}

func toInt(v any) (int64, error) {
	switch x := v.(type) {
	case json.Number:
		i, err := strconv.ParseInt(string(x), 10, 64)
		if err != nil {
			return 0, errors.New("not an integer")
		}
		return i, nil
	case float64:
		if x != float64(int64(x)) || x > 1<<53 || x < -(1<<53) {
			return 0, errors.New("not an integer")
		}
		return int64(x), nil
	case int:
		return int64(x), nil
	case int64:
		return x, nil
	}
	return 0, errors.New("not a number")
}

// normalizeAny turns numbers into int64 (refusing fractions) for open
// arguments, so the digest is defined.
func normalizeAny(v any, depth int) (any, error) {
	if depth > 6 {
		return nil, errors.New("nested too deep")
	}
	switch x := v.(type) {
	case nil, bool, string:
		return x, nil
	case json.Number, float64, int, int64:
		return toInt(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			n, err := normalizeAny(e, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			n, err := normalizeAny(e, depth+1)
			if err != nil {
				return nil, err
			}
			out[k] = n
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported value %T", v)
}

// ---------------------------------------------------------------------------
// Resources

// HomeFunc returns a user's home directory.
type HomeFunc func(uid int) (string, error)

// Extract returns the resources a validated call touches.
func (a *Action) Extract(args map[string]any, uid int, home HomeFunc) ([]proposal.Resource, error) {
	var out []proposal.Resource
	for _, e := range a.Resources {
		vals, sizes := argValues(args, e.Arg, e.BytesArg)
		for i, v := range vals {
			res := proposal.Resource{Kind: e.Kind, Value: v, Beneath: e.Beneath}
			if i < len(sizes) {
				res.Bytes = sizes[i]
			}
			if e.Kind == "path" {
				p, err := cleanPath(v, e.ExpandHome, uid, home)
				if err != nil {
					return nil, err
				}
				res.Value = p
			}
			if e.Kind == "host" || e.Kind == "recipient" {
				res.Value = strings.ToLower(strings.TrimSuffix(v, "."))
			}
			out = append(out, res)
		}
	}
	return out, nil
}

// argValues resolves an extractor path to string values.
func argValues(args map[string]any, spec, bytesArg string) ([]string, []int64) {
	if v, ok := strings.CutPrefix(spec, "="); ok {
		return []string{v}, nil
	}
	name, rest, list := strings.Cut(spec, "[]")
	v := args[name]
	if !list {
		s := scalar(v)
		if s == "" {
			return nil, nil
		}
		var sizes []int64
		if bytesArg != "" {
			if n, err := toInt(args[bytesArg]); err == nil {
				sizes = []int64{n}
			}
		}
		return []string{s}, sizes
	}
	l, _ := v.([]any)
	var out []string
	for _, e := range l {
		if f, ok := strings.CutPrefix(rest, "."); ok {
			m, _ := e.(map[string]any)
			if s := scalar(m[f]); s != "" {
				out = append(out, s)
			}
			continue
		}
		if s := scalar(e); s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

func scalar(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	case bool:
		return strconv.FormatBool(x)
	}
	return ""
}

func cleanPath(p string, expand bool, uid int, home HomeFunc) (string, error) {
	if expand && (p == "~" || strings.HasPrefix(p, "~/")) {
		if home == nil {
			return "", errors.New("no home directory lookup")
		}
		h, err := home(uid)
		if err != nil {
			return "", err
		}
		p = h + strings.TrimPrefix(p, "~")
	}
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("path %q is not absolute", p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return "", fmt.Errorf("path %q goes up with ..", p)
		}
	}
	return path.Clean(p), nil
}

// ExpandHome expands a leading ~ of a rule path for uid.
func ExpandHome(p string, uid int, home HomeFunc) (string, error) {
	return cleanPath(p, true, uid, home)
}
