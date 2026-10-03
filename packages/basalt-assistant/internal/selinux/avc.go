// Package selinux parses AVC denials and maps them to known, reviewable
// fixes: a relabel (restorecon), a file context rule (semanage fcontext), a
// port label (semanage port) or a boolean (setsebool). It never generates
// policy (no audit2allow): a denial without a known fix is reported as such.
package selinux

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AVC is one parsed denial.
type AVC struct {
	Time       time.Time `json:"time"`
	Perms      []string  `json:"perms"`
	PID        int       `json:"pid,omitempty"`
	Comm       string    `json:"comm,omitempty"`
	Name       string    `json:"name,omitempty"`
	Path       string    `json:"path,omitempty"`
	Dev        string    `json:"dev,omitempty"`
	Ino        uint64    `json:"ino,omitempty"`
	Port       int       `json:"port,omitempty"`
	SContext   string    `json:"scontext"`
	TContext   string    `json:"tcontext"`
	Class      string    `json:"tclass"`
	Permissive bool      `json:"permissive"`
	Raw        string    `json:"raw"`
}

var (
	reDenied = regexp.MustCompile(`avc:\s+denied\s+\{([^}]*)\}\s+for\s+(.*)`)
	reKV     = regexp.MustCompile(`(\w+)=("[^"]*"|\S+)`)
)

// ParseAVC parses an AVC record (kernel or journal form). It returns false
// for anything that is not a denial (granted records, other audit types).
func ParseAVC(line string) (AVC, bool) {
	m := reDenied.FindStringSubmatch(line)
	if m == nil {
		return AVC{}, false
	}
	a := AVC{Perms: strings.Fields(m[1]), Raw: strings.TrimSpace(line)}
	for _, kv := range reKV.FindAllStringSubmatch(m[2], -1) {
		k, v := kv[1], strings.Trim(kv[2], `"`)
		switch k {
		case "pid":
			a.PID, _ = strconv.Atoi(v)
		case "comm":
			a.Comm = v
		case "name":
			a.Name = v
		case "path":
			a.Path = v
		case "dev":
			a.Dev = v
		case "ino":
			a.Ino, _ = strconv.ParseUint(v, 10, 64)
		case "src", "dest", "lport":
			if a.Port == 0 {
				a.Port, _ = strconv.Atoi(v)
			}
		case "scontext":
			a.SContext = v
		case "tcontext":
			a.TContext = v
		case "tclass":
			a.Class = v
		case "permissive":
			a.Permissive = v == "1"
		}
	}
	if a.SContext == "" || a.TContext == "" || a.Class == "" {
		return AVC{}, false
	}
	return a, true
}

// Type returns the type field of a context (user:role:type:level).
func Type(ctx string) string {
	parts := strings.Split(ctx, ":")
	if len(parts) >= 3 {
		return parts[2]
	}
	return ""
}

// SType and TType are the source and target types.
func (a AVC) SType() string { return Type(a.SContext) }
func (a AVC) TType() string { return Type(a.TContext) }

// IsPort reports a socket bind/connect denial on a port.
func (a AVC) IsPort() bool {
	if a.Class != "tcp_socket" && a.Class != "udp_socket" && a.Class != "sctp_socket" {
		return false
	}
	for _, p := range a.Perms {
		if p == "name_bind" || p == "name_connect" {
			return true
		}
	}
	return false
}

// IsFile reports a denial on a file system object.
func (a AVC) IsFile() bool {
	switch a.Class {
	case "file", "dir", "lnk_file", "sock_file", "fifo_file", "chr_file", "blk_file":
		return true
	}
	return false
}

// Proto is tcp, udp or sctp for a port denial.
func (a AVC) Proto() string { return strings.TrimSuffix(a.Class, "_socket") }

// Key groups repeated denials: same domain, target type, class, object and
// permissions are one problem.
func (a AVC) Key() string {
	perms := append([]string(nil), a.Perms...)
	sort.Strings(perms)
	obj := a.Name
	if a.Path != "" {
		obj = a.Path
	}
	if a.IsPort() {
		obj = strconv.Itoa(a.Port)
	}
	return strings.Join([]string{a.SType(), a.TType(), a.Class, strings.Join(perms, ","), obj}, "|")
}

// Group is a set of identical denials.
type Group struct {
	AVC   AVC       `json:"avc"`
	Count int       `json:"count"`
	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
}

// GroupAVCs merges denials by Key, newest last.
func GroupAVCs(avcs []AVC) []Group {
	idx := map[string]int{}
	var out []Group
	for _, a := range avcs {
		k := a.Key()
		if i, ok := idx[k]; ok {
			out[i].Count++
			if a.Time.After(out[i].Last) {
				out[i].Last = a.Time
			}
			if a.Path != "" && out[i].AVC.Path == "" {
				out[i].AVC.Path = a.Path
			}
			continue
		}
		idx[k] = len(out)
		out = append(out, Group{AVC: a, Count: 1, First: a.Time, Last: a.Time})
	}
	return out
}
