// SPDX-License-Identifier: MIT OR Apache-2.0

package gate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// Digest returns the digest an executor checks at claim time:
// "sha256:" and the hex SHA-256 of the canonical JSON (PROTOCOL.md,
// "Digest") of the calls, the preview, the resources and, when set, the
// reference, after the normalization of normalizeDigest.
func Digest(calls []Call, pv Preview, res []Resource, ref string) (string, error) {
	if res == nil {
		res = []Resource{}
	}
	cs := make([]Call, len(calls))
	for i, c := range calls {
		cs[i] = c
		if cs[i].Args == nil {
			cs[i].Args = map[string]any{}
		}
	}
	in := struct {
		Calls     []Call     `json:"calls"`
		Preview   Preview    `json:"preview"`
		Resources []Resource `json:"resources"`
		Ref       string     `json:"ref,omitempty"`
	}{cs, pv, res, ref}
	raw, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	b, err := json.Marshal(normalizeDigest(v))
	if err != nil {
		return "", err
	}
	c, err := canonicalize(b)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// normalizeDigest applies the digest's normalization (PROTOCOL.md,
// "Digest"): in the preview, in each preview line and in each resource,
// members whose value is null, false, 0, "", [] or {} are removed; ref is
// removed when empty; nothing is removed inside call arguments, title
// arguments or line arguments.
func normalizeDigest(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	if pv, ok := m["preview"].(map[string]any); ok {
		dropEmpty(pv)
		if l, ok := pv["lines"].([]any); ok {
			for _, e := range l {
				if lm, ok := e.(map[string]any); ok {
					dropEmpty(lm)
				}
			}
		}
	}
	if l, ok := m["resources"].([]any); ok {
		for _, e := range l {
			if rm, ok := e.(map[string]any); ok {
				dropEmpty(rm)
			}
		}
	}
	if r, ok := m["ref"]; ok && (r == nil || r == "") {
		delete(m, "ref")
	}
	return m
}

func dropEmpty(m map[string]any) {
	for k, v := range m {
		empty := false
		switch x := v.(type) {
		case nil:
			empty = true
		case bool:
			empty = !x
		case float64:
			empty = x == 0
		case json.Number:
			empty = x == "0"
		case string:
			empty = x == ""
		case []any:
			empty = len(x) == 0
		case map[string]any:
			empty = len(x) == 0
		}
		if empty {
			delete(m, k)
		}
	}
}

// ShortCode is the 8 hex digits people read (PROTOCOL.md, "Short code").
func ShortCode(digest, ref string, commands []string) string {
	if ref != "" && len(commands) > 0 {
		sum := sha256.Sum256([]byte(ref + "\n" + strings.Join(commands, "\n")))
		return hex.EncodeToString(sum[:])[:8]
	}
	h := strings.TrimPrefix(digest, "sha256:")
	if len(h) < 8 {
		return ""
	}
	return h[:8]
}
