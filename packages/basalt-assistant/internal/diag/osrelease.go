package diag

import (
	"strconv"
	"strings"
)

// OSRelease is the system identity from /etc/os-release. On Basalt OS
// VERSION_ID is the Fedora release the system runs on; the Basalt version
// is in BASALT_VERSION and the build in BUILD_ID.
type OSRelease struct {
	Name          string `json:"name"`
	ID            string `json:"id"`
	VersionID     string `json:"version_id"` // the Fedora release
	Version       string `json:"version"`
	BasaltVersion string `json:"basalt_version,omitempty"`
	BuildID       string `json:"build_id,omitempty"`
}

// String is the one-line form used by `basalt status`.
func (o OSRelease) String() string {
	if o.Name == "" {
		return "unknown"
	}
	s := o.Name
	if o.Version != "" {
		s += " " + o.Version
	} else if o.VersionID != "" {
		s += " " + o.VersionID
	}
	if o.BuildID != "" {
		s += ", build " + o.BuildID
	}
	return s
}

// ParseOSRelease reads os-release(5) content: KEY=value lines, values
// optionally quoted with shell quoting.
func ParseOSRelease(b []byte) OSRelease {
	m := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			if v[0] == '"' {
				if u, err := strconv.Unquote(v); err == nil {
					v = u
				} else {
					v = v[1 : len(v)-1]
				}
			} else {
				v = v[1 : len(v)-1]
			}
		}
		m[k] = v
	}
	return OSRelease{Name: m["NAME"], ID: m["ID"], VersionID: m["VERSION_ID"], Version: m["VERSION"],
		BasaltVersion: m["BASALT_VERSION"], BuildID: m["BUILD_ID"]}
}

// osRelease reads /etc/os-release, falling back to /usr/lib/os-release.
func (e *Env) osRelease() OSRelease {
	if e.ReadFile == nil {
		return OSRelease{}
	}
	for _, p := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		if b, err := e.ReadFile(p); err == nil {
			return ParseOSRelease(b)
		}
	}
	return OSRelease{}
}
