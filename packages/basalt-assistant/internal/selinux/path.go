package selinux

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
)

// AnalyzePath checks a path a service reported "Permission denied" for,
// for the cases where no AVC was logged: Fedora's policy has dontaudit
// rules that hide many denials (a confined service searching an
// administrator's home directory is the classic one). Each existing
// component of the path is compared with its policy default; the first
// one whose label is wrong (and whose default the service may use), or
// generic, gives the fix. label returns a file's context ("" when the file
// does not exist).
func (z Analyzer) AnalyzePath(ctx context.Context, dom, p string, write bool, label func(string) string) (f Fix, found bool) {
	p = path.Clean(p)
	if !strings.HasPrefix(p, "/") || dom == "" || !ValidPath(p) || Unconfined(dom) || PseudoPath(p) {
		return Fix{}, false
	}
	z = z.begin()
	defer func() {
		if found {
			z.finishErrors(&f)
		} else if len(*z.errs) > 0 {
			// A failed query must not hide a problem silently either: report
			// the path with no conclusion.
			f = Fix{Group: Group{AVC: AVC{SContext: "system_u:system_r:" + dom + ":s0", Class: "file", Path: p,
				Raw: "(no AVC logged) label check of " + p}}, Features: map[string]bool{"has_path": true, "no_avc": true},
				Facts: map[string]string{}, Path: p, PathFrom: "journal message, label check"}
			z.finishErrors(&f)
			found = true
		}
	}()
	var comps []string
	for c := p; c != "/"; c = path.Dir(c) {
		comps = append([]string{c}, comps...)
	}
	// Every existing component and the access it needs, first: their
	// rules are then looked up in one walk of the policy.
	type step struct{ c, lab, class, perm string }
	var steps []step
	var qs [][]string
	for i, c := range comps {
		lab := label(c)
		if lab == "" {
			break
		}
		leaf := i == len(comps)-1
		class, perm := "dir", "search"
		switch {
		case leaf && write:
			class, perm = "file", "append"
		case leaf:
			class, perm = "file", "open"
		case i == len(comps)-2 && label(comps[i+1]) == "" && write:
			perm = "add_name" // the file is about to be created here
		}
		if leaf {
			// Directories can be the leaf too (a document root).
			if z.R.Read(ctx, "test", "-d", c).Code == 0 {
				class, perm = "dir", "search"
			}
		}
		steps = append(steps, step{c, lab, class, perm})
		qs = append(qs, AllowQuery(dom, Type(lab), class, perm))
	}
	z.prefetch(ctx, qs...)
	for _, s := range steps {
		c, lab, class, perm := s.c, s.lab, s.class, s.perm
		actual := Type(lab)
		if z.allowed(ctx, dom, actual, class, perm) {
			continue
		}
		a := AVC{SContext: "system_u:system_r:" + dom + ":s0", TContext: lab, Class: class, Perms: []string{perm}, Path: c,
			Raw: fmt.Sprintf("(no AVC logged) %s may not %s %s %s labeled %s", dom, perm, class, c, actual)}
		f := Fix{Group: Group{AVC: a}, Features: map[string]bool{"has_path": true, "no_avc": true}, Facts: map[string]string{},
			Path: c, PathFrom: "journal message, label check"}
		f.Evidence = append(f.Evidence, fmt.Sprintf("%s may not %s the %s %s (labeled %s); no AVC was logged, so a dontaudit rule hides the denial", dom, perm, class, c, actual))
		if sensitiveTypes[actual] {
			// Never propose relabeling a security-sensitive object (found
			// in the lab: nginx told to include /etc/shadow).
			f.Features["sensitive_target"] = true
			f.Explanation = fmt.Sprintf("%s may not %s %s (labeled %s, security-sensitive): do not relabel it; fix what makes the service use it",
				dom, perm, c, actual)
			f.Class = classify(f.Features)
			return f, true
		}
		def := z.defaultType(ctx, c)
		f.DefaultType = def
		// The default label and the candidate types, in one more walk.
		more := [][]string{AllowQuery(dom, "", class, perm)}
		if def != "" && def != actual {
			more = append(more, AllowQuery(dom, def, class, perm))
		}
		if t := preferredType(dom, p, write); t != "" {
			more = append(more, AllowQuery(dom, t, class, perm))
		}
		z.prefetch(ctx, more...)
		if def != "" {
			f.Evidence = append(f.Evidence, fmt.Sprintf("policy default label for %s: %s", c, def))
		}
		if def != "" && def != actual {
			f.Features["default_differs"] = true
			if z.allowed(ctx, dom, def, class, perm) {
				f.Features["default_allowed"] = true
				rec := "no"
				if class == "dir" {
					rec = "yes"
				}
				f.Actions = []action.Action{{Kind: action.SELinuxRestorecon, Params: map[string]string{"path": c, "recursive": rec}}}
				f.Explanation = fmt.Sprintf("%s is labeled %s but the policy says %s, which %s may use; it was probably moved here (mv keeps labels) or created with the wrong label",
					c, actual, def, dom)
				f.Class = classify(f.Features)
				return f, true
			}
		}
		if genericTypes[actual] || genericTypes[def] {
			f.Features["generic_target"] = true
		}
		if want := z.candidateType(ctx, dom, class, perm, p, write); want != "" {
			f.Features["candidate_type"] = true
			dir := labelDir(c, class)
			f.Actions = []action.Action{{Kind: action.SELinuxFcontext, Params: map[string]string{"path": dir, "type": want}}}
			f.Explanation = fmt.Sprintf("%s is labeled %s, which %s may not use; give %s the type %s", c, actual, dom, dir, want)
		} else {
			f.Explanation = fmt.Sprintf("%s may not %s %s (%s) and no known type fits", dom, perm, c, actual)
		}
		f.Class = classify(f.Features)
		return f, true
	}
	return Fix{}, false
}
