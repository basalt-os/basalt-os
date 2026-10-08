package sources

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/knowledge"
)

// Paths of the files this package reads and writes. Tests point them at a
// temporary directory.
type Paths struct {
	RepoDir     string // dnf repository definitions
	OverrideDir string // dnf5 config-manager overrides (setopt)
	// VendorOverrideDir holds the overrides packages ship (basalt-release
	// enforces the Basalt repositories' signature checks there); a file of
	// the same name in OverrideDir replaces one here, as in dnf.
	VendorOverrideDir string
	KeyDir            string // repository keys
	SourcesDir        string // Basalt's record of the sources it added
	VarsDir           string // dnf variables (basalt_repo_url and the others)
	Flatpak           string // the flatpak program ("" when not installed)
}

// System are the real paths.
var System = Paths{RepoDir: "/etc/yum.repos.d", OverrideDir: "/etc/dnf/repos.override.d",
	VendorOverrideDir: "/usr/share/dnf5/repos.override.d", KeyDir: "/etc/pki/rpm-gpg",
	SourcesDir: "/etc/basalt/sources.d", VarsDir: "/etc/dnf/vars", Flatpak: "/usr/bin/flatpak"}

// ReleaseKey is the fingerprint of the OpenBasalt release key, which signs
// every Basalt repository.
var ReleaseKey = knowledge.OpenBasaltKnowledge.Primary

// Channel ids: Basalt's own repositories.
const (
	ChanBasalt          = "basalt"
	ChanTools           = "basalt-tools"
	ChanTesting         = "basalt-testing"
	ChanNonfree         = "basalt-nonfree"
	ChanNonfreeTesting  = "basalt-nonfree-testing"
	NonfreeReleasePkg   = "basalt-nonfree-release"
	TestingConsentToken = "preview-builds-1"
)

// Channels is the closed list of Basalt channels, in display order.
var Channels = []string{ChanBasalt, ChanTools, ChanTesting, ChanNonfree, ChanNonfreeTesting}

// Toggleable reports a channel a person may turn on or off from
// Settings, Updates and channels (repo.enable, repo.disable). basalt
// stays on; basalt-nonfree is turned on by Additional drivers, with the
// NVIDIA license.
func Toggleable(id string) bool {
	return id == ChanTools || id == ChanTesting || id == ChanNonfreeTesting
}

// Testing reports a preview channel: turning it on needs the person's
// consent to preview builds.
func Testing(id string) bool { return id == ChanTesting || id == ChanNonfreeTesting }

// Section is one repository section of a .repo file.
type Section struct {
	ID   string
	File string
	Keys map[string]string
}

// ReadRepos reads every [section] of the .repo files and applies the
// overrides in dnf's order: the vendor overrides and the administrator's
// (dnf5 writes `setopt` there) together, sorted by file name, a file in
// OverrideDir masking the vendor file of the same name.
func ReadRepos(p Paths) map[string]*Section {
	out := map[string]*Section{}
	files, _ := filepath.Glob(filepath.Join(p.RepoDir, "*.repo"))
	sort.Strings(files)
	for _, f := range files {
		for _, s := range parseINI(f) {
			if _, dup := out[s.ID]; !dup {
				out[s.ID] = s
			}
		}
	}
	for _, f := range overrideFiles(p) {
		for _, s := range parseINI(f) {
			if base, ok := out[s.ID]; ok {
				for k, v := range s.Keys {
					base.Keys[k] = v
				}
			}
		}
	}
	return out
}

// overrideFiles lists the override files dnf applies, in its order.
func overrideFiles(p Paths) []string {
	byName := map[string]string{}
	for _, dir := range []string{p.VendorOverrideDir, p.OverrideDir} {
		if dir == "" {
			continue
		}
		fs, _ := filepath.Glob(filepath.Join(dir, "*.repo"))
		for _, f := range fs {
			byName[filepath.Base(f)] = f
		}
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, byName[n])
	}
	return out
}

func parseINI(path string) []*Section {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []*Section
	var cur *Section
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			cur = &Section{ID: strings.TrimSpace(line[1 : len(line)-1]), File: path, Keys: map[string]string{}}
			out = append(out, cur)
			continue
		}
		if cur == nil {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			cur.Keys[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	return out
}

// truthy reads a dnf boolean.
func truthy(v string) bool {
	switch strings.ToLower(v) {
	case "1", "yes", "true", "on":
		return true
	}
	return false
}

// Enabled reports a section's enabled option (dnf's default is on).
func (s *Section) Enabled() bool {
	v, ok := s.Keys["enabled"]
	return !ok || truthy(v)
}

// Signature is how a repository is verified.
type Signature struct {
	GPGCheck     bool     `json:"gpgcheck"`
	RepoGPGCheck bool     `json:"repo_gpgcheck"`
	KeyFiles     []string `json:"key_files,omitempty"`
	Keys         []string `json:"fingerprints,omitempty"`
	Owners       []string `json:"owners,omitempty"`
	// OpenBasalt: every key is the OpenBasalt release key.
	OpenBasalt bool   `json:"openbasalt"`
	Short      string `json:"short,omitempty"`
	Problem    string `json:"problem,omitempty"`
}

// signature reads a section's key files (file:// only) and their keys.
func signature(s *Section) Signature {
	g := Signature{GPGCheck: truthy(s.Keys["gpgcheck"]), RepoGPGCheck: truthy(s.Keys["repo_gpgcheck"])}
	for _, u := range strings.FieldsFunc(s.Keys["gpgkey"], func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		if !strings.HasPrefix(u, "file://") {
			g.Problem = "a key is fetched from the network at first use: " + u
			continue
		}
		f := strings.TrimPrefix(u, "file://")
		g.KeyFiles = append(g.KeyFiles, f)
		b, err := os.ReadFile(f)
		if err != nil {
			g.Problem = "key file " + f + ": " + err.Error()
			continue
		}
		ks, err := knowledge.CertKeys(b)
		if err != nil {
			g.Problem = "key file " + f + ": " + err.Error()
			continue
		}
		for _, k := range ks {
			g.Keys = append(g.Keys, k.Fingerprint)
			g.Owners = append(g.Owners, k.UserIDs...)
		}
	}
	g.OpenBasalt = len(g.Keys) > 0
	for _, k := range g.Keys {
		if k != ReleaseKey {
			g.OpenBasalt = false
		}
	}
	if len(g.Keys) > 0 {
		g.Short = Short(g.Keys[0])
	}
	if !g.GPGCheck {
		g.Problem = "package signatures are not checked (gpgcheck is off)"
	}
	return g
}

// ChannelState is one Basalt channel.
type ChannelState struct {
	ID         string    `json:"id"`
	Defined    bool      `json:"defined"`
	Enabled    bool      `json:"enabled"`
	Toggle     bool      `json:"toggle"`
	Testing    bool      `json:"testing"`
	Nonfree    bool      `json:"nonfree"`
	URL        string    `json:"url,omitempty"`
	Signature  Signature `json:"signature"`
	Definition string    `json:"definition,omitempty"` // the package that defines it
}

// Record is Basalt's record of a source it added
// (/etc/basalt/sources.d/ID.json), written by `basalt __source add`.
type Record struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	Catalog      string `json:"catalog"`
	Group        string `json:"group"`
	URLType      string `json:"url_type"`
	URL          string `json:"url"`
	KeyURL       string `json:"key_url"`
	Fingerprint  string `json:"fingerprint"`
	KeyOwner     string `json:"key_owner"`
	RepoGPGCheck bool   `json:"repo_gpgcheck"`
	Added        string `json:"added"`
	// Proposal and By are filled in by basalt apply once the proposal that
	// added it finished: which proposal, and who decided.
	Proposal string `json:"proposal,omitempty"`
	By       string `json:"by,omitempty"`
}

// RecordPath is where a source's record lives.
func (p Paths) RecordPath(id string) string { return filepath.Join(p.SourcesDir, id+".json") }

// RepoPath is the .repo file Basalt writes for an RPM source.
func (p Paths) RepoPath(id string) string {
	return filepath.Join(p.RepoDir, "basalt-source-"+id+".repo")
}

// KeyPath is the key file of a source group.
func (p Paths) KeyPath(group string) string {
	return filepath.Join(p.KeyDir, "basalt-source-"+group+".asc")
}

// Records lists the sources Basalt added.
func Records(p Paths) []Record {
	files, _ := filepath.Glob(filepath.Join(p.SourcesDir, "*.json"))
	sort.Strings(files)
	var out []Record
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var r Record
		if json.Unmarshal(b, &r) == nil && ReID.MatchString(r.ID) && filepath.Base(f) == r.ID+".json" {
			out = append(out, r)
		}
	}
	return out
}

// HasRecord reports a source Basalt added (repo.enable, repo.disable and
// source.remove act only on those).
func HasRecord(p Paths, id string) bool {
	if !ReID.MatchString(id) {
		return false
	}
	st, err := os.Stat(p.RecordPath(id))
	return err == nil && st.Mode().IsRegular()
}

// SourceState is a source Basalt added, with its state now.
type SourceState struct {
	Record
	Enabled bool   `json:"enabled"`
	Short   string `json:"short"`
	Present bool   `json:"present"` // its repository file (or remote) is still there
}

// Other is a repository that is neither Fedora's nor Basalt's and was not
// added through Basalt (by hand, by a package): shown, not changed here.
type Other struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	File    string `json:"file"`
	Enabled bool   `json:"enabled"`
	Signed  bool   `json:"signed"`
}

// CatalogState is a catalog entry and whether it was added.
type CatalogState struct {
	Entry
	Short     string `json:"short"`
	Added     bool   `json:"added"`
	Available bool   `json:"available"` // flatpak entries need flatpak installed
}

// Report is `basalt channels --json`.
type Report struct {
	Channels   []ChannelState `json:"channels"`
	Sources    []SourceState  `json:"sources"`
	Other      []Other        `json:"other"`
	Catalog    []CatalogState `json:"catalog"`
	ReleaseKey string         `json:"release_key"`
	ShortKey   string         `json:"release_key_short"`
	// NonfreeDefined: basalt-nonfree-release is installed (its file is
	// there); NonfreeTestingDefined: it already has the testing section.
	NonfreeDefined        bool   `json:"nonfree_defined"`
	NonfreeTestingDefined bool   `json:"nonfree_testing_defined"`
	Docs                  string `json:"docs"`
}

// fedoraRepo reports Fedora's own repositories (shown nowhere: they are
// the base system).
func fedoraRepo(id string) bool {
	for _, p := range []string{"fedora", "updates", "rawhide"} {
		if id == p || strings.HasPrefix(id, p+"-") {
			return true
		}
	}
	return false
}

// Build reads the channels, the sources Basalt added, other repositories
// and the catalog. It only reads files.
func Build(p Paths) Report {
	repos := ReadRepos(p)
	r := Report{ReleaseKey: Spaced(ReleaseKey), ShortKey: Short(ReleaseKey),
		Docs: "https://github.com/basalt-os/basalt-os/blob/main/docs/security/updates.md"}
	for _, id := range Channels {
		c := ChannelState{ID: id, Toggle: Toggleable(id), Testing: Testing(id), Nonfree: strings.HasPrefix(id, ChanNonfree)}
		if c.Nonfree {
			c.Definition = NonfreeReleasePkg
		} else {
			c.Definition = "basalt-release"
		}
		if s, ok := repos[id]; ok {
			c.Defined, c.Enabled = true, s.Enabled()
			c.URL = s.Keys["baseurl"]
			c.Signature = signature(s)
		}
		r.Channels = append(r.Channels, c)
	}
	_, r.NonfreeDefined = repos[ChanNonfree]
	_, r.NonfreeTestingDefined = repos[ChanNonfreeTesting]

	recs := Records(p)
	added := map[string]bool{}
	for _, rec := range recs {
		added[rec.ID], added[rec.Group] = true, true
		st := SourceState{Record: rec, Short: Short(rec.Fingerprint)}
		if rec.Kind == KindRPM {
			if s, ok := repos[rec.ID]; ok {
				st.Present, st.Enabled = true, s.Enabled()
			}
		} else {
			st.Present, st.Enabled = true, true
		}
		r.Sources = append(r.Sources, st)
	}
	ids := make([]string, 0, len(repos))
	for id := range repos {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s := repos[id]
		if fedoraRepo(id) || strings.HasPrefix(id, "basalt") || added[id] ||
			strings.HasSuffix(id, "-source") || strings.HasSuffix(id, "-debuginfo") {
			continue
		}
		r.Other = append(r.Other, Other{ID: id, Name: s.Keys["name"], File: s.File, Enabled: s.Enabled(), Signed: truthy(s.Keys["gpgcheck"])})
	}
	for _, e := range Catalog {
		cs := CatalogState{Entry: e, Short: Short(e.Fingerprint), Added: added[e.ID], Available: true}
		if e.Kind == KindFlatpak {
			_, err := os.Stat(p.Flatpak)
			cs.Available = p.Flatpak != "" && err == nil
		}
		r.Catalog = append(r.Catalog, cs)
	}
	return r
}

// Group returns the records of one group (a catalog entry adds one record
// per repository: RPM Fusion has two).
func Group(p Paths, group string) []Record {
	var out []Record
	for _, r := range Records(p) {
		if r.Group == group {
			out = append(out, r)
		}
	}
	return out
}
