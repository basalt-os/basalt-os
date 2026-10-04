// Package secrets reads the API keys a profile asks for. The store is a
// per-profile file, ~/.config/basalt-agent/secrets/PROFILE.env, with
// KEY=VALUE lines; it must be a regular file owned by the user with no
// group or other access. A key missing from the file is looked up in the
// desktop keyring with secret-tool when that is installed
// (secret-tool store --label=... service basalt-agent profile PROFILE name KEY).
//
// Only the names a profile lists are read, and values are never logged:
// the audit log records the names that were injected.
package secrets

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// Load returns the values of names for profile from file (and the keyring).
// Names with no value are returned in missing.
func Load(file, profile string, names []string) (vals map[string]string, missing []string, err error) {
	vals = map[string]string{}
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	if fromFile, err := readFile(file); err == nil {
		for k, v := range fromFile {
			if want[k] {
				vals[k] = v
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, nil, err
	}
	for _, n := range names {
		if _, ok := vals[n]; ok {
			continue
		}
		if v, ok := keyring(profile, n); ok {
			vals[n] = v
			continue
		}
		missing = append(missing, n)
	}
	return vals, missing, nil
}

func readFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	sys, _ := st.Sys().(*syscall.Stat_t)
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 || sys == nil || int(sys.Uid) != os.Getuid() {
		return nil, fmt.Errorf("%s: must be a regular file owned by you with mode 0600 (chmod 600 %s)", path, path)
	}
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE", path, n)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if strings.ContainsAny(v, "\n\x00") {
			return nil, fmt.Errorf("%s:%d: bad value", path, n)
		}
		out[k] = v
	}
	return out, sc.Err()
}

func keyring(profile, name string) (string, bool) {
	st, err := exec.LookPath("secret-tool")
	if err != nil {
		return "", false
	}
	out, err := exec.Command(st, "lookup", "service", "basalt-agent", "profile", profile, "name", name).Output()
	if err != nil {
		return "", false
	}
	v := strings.TrimRight(string(out), "\n")
	return v, v != ""
}

// EnvFile renders vals as a KEY=VALUE file for the container entry point.
func EnvFile(vals map[string]string) []byte {
	var b strings.Builder
	for k, v := range vals {
		fmt.Fprintf(&b, "%s=%s\n", k, v)
	}
	return []byte(b.String())
}

// ParseEnvFile is the inverse of EnvFile (used inside the container).
func ParseEnvFile(data []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && k != "" {
			out[k] = v
		}
	}
	return out
}
