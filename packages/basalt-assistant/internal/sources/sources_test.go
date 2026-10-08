package sources

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const msFpr = "BC528686B50D79E339D3721CEB3E94ADBE1229CF"

func vscode() Params {
	e, _ := Lookup("vscode")
	return Params{ID: "vscode", Name: e.Repos[0].Name, Kind: KindRPM, URLType: URLBase, URL: e.Repos[0].URL, KeyURL: e.KeyURL,
		Fingerprint: e.Fingerprint, GPGCheck: "1", RepoGPGCheck: "1", Catalog: "vscode", Group: "vscode"}
}

func custom() Params {
	return Params{ID: "example", Name: "Example", Kind: KindRPM, URLType: URLBase, URL: "https://repo.example.org/fedora/$releasever/",
		KeyURL: "https://repo.example.org/key.asc", Fingerprint: msFpr, GPGCheck: "1", RepoGPGCheck: "1", Catalog: CatalogCustom, Group: "example"}
}

func TestSourceAddValidator(t *testing.T) {
	if err := vscode().Validate(); err != nil {
		t.Fatalf("catalog entry refused: %v", err)
	}
	if err := custom().Validate(); err != nil {
		t.Fatalf("custom refused: %v", err)
	}
	for name, mod := range map[string]func(*Params){
		"plain http":           func(p *Params) { p.URL = "http://repo.example.org/fedora/" },
		"plain http key":       func(p *Params) { p.KeyURL = "http://repo.example.org/key.asc" },
		"missing key":          func(p *Params) { p.KeyURL = "" },
		"no fingerprint":       func(p *Params) { p.Fingerprint = "" },
		"gpgcheck off":         func(p *Params) { p.GPGCheck = "0" },
		"custom metadata off":  func(p *Params) { p.RepoGPGCheck = "0" },
		"shadows fedora":       func(p *Params) { p.ID = "updates-testing" },
		"shadows basalt":       func(p *Params) { p.ID = "basalt-extra" },
		"shell in url":         func(p *Params) { p.URL = "https://repo.example.org/`id`" },
		"newline in name":      func(p *Params) { p.Name = "a\n[evil]" },
		"file key":             func(p *Params) { p.KeyURL = "file:///etc/passwd" },
		"unknown kind":         func(p *Params) { p.Kind = "deb" },
		"custom flatpak":       func(p *Params) { p.Kind = KindFlatpak },
		"bad metadata setting": func(p *Params) { p.RepoGPGCheck = "maybe" },
	} {
		p := custom()
		mod(&p)
		if err := p.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A catalog entry with anything but its pinned values is refused.
	for name, mod := range map[string]func(*Params){
		"fingerprint mismatch": func(p *Params) { p.Fingerprint = "060A61C51B558A7F742B77AAC52FEB6B621E9F35" },
		"other url":            func(p *Params) { p.URL = "https://mirror.example.org/vscode" },
		"other key url":        func(p *Params) { p.KeyURL = "https://mirror.example.org/microsoft.asc" },
		"metadata unsigned":    func(p *Params) { p.RepoGPGCheck = "0" },
		"other group":          func(p *Params) { p.Group = "docker-ce" },
		"not in the entry":     func(p *Params) { p.ID = "vscode-insiders" },
	} {
		p := vscode()
		mod(&p)
		err := p.Validate()
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
		if name == "fingerprint mismatch" && !strings.Contains(err.Error(), "not the one Basalt pins") {
			t.Errorf("fingerprint mismatch: %v", err)
		}
	}
	// COPR: the project's own URLs only.
	url, key, err := CoprURLs("someone/tool")
	if err != nil {
		t.Fatal(err)
	}
	cp := Params{ID: CoprID("someone/tool"), Name: "COPR someone/tool", Kind: KindRPM, URLType: URLBase, URL: url, KeyURL: key,
		Fingerprint: msFpr, GPGCheck: "1", RepoGPGCheck: "0", Catalog: CatalogCopr, Group: CoprID("someone/tool")}
	if err := cp.Validate(); err != nil {
		t.Fatalf("copr refused: %v", err)
	}
	cp.KeyURL = "https://evil.example.org/pubkey.gpg"
	if cp.Validate() == nil {
		t.Error("copr with a foreign key accepted")
	}
	if _, _, err := CoprURLs("../etc"); err == nil {
		t.Error("bad copr project accepted")
	}
}

func TestCatalogPins(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range Catalog {
		if !reFpr.MatchString(e.Fingerprint) {
			t.Errorf("%s: fingerprint %q", e.ID, e.Fingerprint)
		}
		for _, r := range e.Repos {
			if seen[r.ID] {
				t.Errorf("repo id %s twice", r.ID)
			}
			seen[r.ID] = true
			rg := map[bool]string{true: "1", false: "0"}[e.RepoGPGCheck]
			p := Params{ID: r.ID, Name: r.Name, Kind: e.Kind, URLType: r.URLType, URL: r.URL, KeyURL: e.KeyURL, Fingerprint: e.Fingerprint,
				GPGCheck: "1", RepoGPGCheck: rg, Catalog: e.ID, Group: e.ID}
			if err := p.Validate(); err != nil {
				t.Errorf("%s/%s: %v", e.ID, r.ID, err)
			}
		}
	}
	if Short("3601734842BD4E482D19DE4AE4EED5ECA395B302") != "E4EE D5EC A395 B302" {
		t.Error(Short("3601734842BD4E482D19DE4AE4EED5ECA395B302"))
	}
}

func tempPaths(t *testing.T) Paths {
	d := t.TempDir()
	p := Paths{RepoDir: filepath.Join(d, "yum.repos.d"), OverrideDir: filepath.Join(d, "override"), KeyDir: filepath.Join(d, "keys"),
		VendorOverrideDir: filepath.Join(d, "vendor-override"),
		SourcesDir:        filepath.Join(d, "sources.d"), VarsDir: filepath.Join(d, "vars"), Flatpak: filepath.Join(d, "flatpak")}
	for _, dir := range []string{p.RepoDir, p.OverrideDir, p.VendorOverrideDir, p.KeyDir, p.SourcesDir} {
		_ = os.MkdirAll(dir, 0o755)
	}
	return p
}

func TestChannelsReport(t *testing.T) {
	p := tempPaths(t)
	key, _ := os.ReadFile("testdata/RPM-GPG-KEY-basalt")
	_ = os.WriteFile(filepath.Join(p.KeyDir, "RPM-GPG-KEY-basalt"), key, 0o644)
	// A copy of packages/basalt-release/basalt.repo: the package builds
	// from its own directory only.
	repo, _ := os.ReadFile("testdata/basalt.repo")
	repo = []byte(strings.ReplaceAll(string(repo), "file:///etc/pki/rpm-gpg/", "file://"+p.KeyDir+"/"))
	_ = os.WriteFile(filepath.Join(p.RepoDir, "basalt.repo"), repo, 0o644)
	_ = os.WriteFile(filepath.Join(p.RepoDir, "fedora.repo"), []byte("[fedora]\nname=Fedora\nenabled=1\n"), 0o644)
	_ = os.WriteFile(filepath.Join(p.RepoDir, "hand.repo"), []byte("[handmade]\nname=By hand\nenabled=0\ngpgcheck=0\n"), 0o644)
	// dnf5 config-manager setopt turned testing on.
	_ = os.WriteFile(filepath.Join(p.OverrideDir, "99-config_manager.repo"), []byte("[basalt-testing]\nenabled=1\n"), 0o644)

	r := Build(p)
	byID := map[string]ChannelState{}
	for _, c := range r.Channels {
		byID[c.ID] = c
	}
	if !byID["basalt"].Enabled || byID["basalt"].Toggle {
		t.Errorf("basalt: %+v", byID["basalt"])
	}
	if !byID["basalt-testing"].Enabled || !byID["basalt-testing"].Testing {
		t.Errorf("override not applied: %+v", byID["basalt-testing"])
	}
	sig := byID["basalt"].Signature
	if !sig.OpenBasalt || sig.Short != "E4EE D5EC A395 B302" || !sig.GPGCheck || !sig.RepoGPGCheck {
		t.Errorf("signature: %+v", sig)
	}
	if byID["basalt-nonfree-testing"].Defined || r.NonfreeTestingDefined {
		t.Error("nonfree-testing defined")
	}
	if len(r.Other) != 1 || r.Other[0].ID != "handmade" || r.Other[0].Signed {
		t.Errorf("other: %+v", r.Other)
	}
	for _, c := range r.Catalog {
		if c.ID == "flathub" && c.Available {
			t.Error("flathub available without flatpak")
		}
	}
}

// An older or hand-written basalt.repo without repo_gpgcheck (kept by
// %config(noreplace)) is overridden by basalt-release's vendor override, as
// dnf does; a file of the same name under /etc/dnf/repos.override.d masks
// the vendor one, and config-manager's 99- file is applied last.
func TestVendorOverrideEnforcesSignatures(t *testing.T) {
	p := tempPaths(t)
	_ = os.WriteFile(filepath.Join(p.RepoDir, "basalt.repo"),
		[]byte("[basalt]\nname=bootstrap\nbaseurl=http://lab/\ngpgcheck=0\n"), 0o644)
	vendor, err := os.ReadFile("../../../basalt-release/20-basalt-signatures.repo")
	if err != nil {
		// The package builds from its own directory only: keep a copy.
		vendor = []byte("[basalt]\ngpgcheck=1\nrepo_gpgcheck=1\n")
	}
	_ = os.WriteFile(filepath.Join(p.VendorOverrideDir, "20-basalt-signatures.repo"), vendor, 0o644)
	s := ReadRepos(p)["basalt"]
	if s == nil || !truthy(s.Keys["gpgcheck"]) || !truthy(s.Keys["repo_gpgcheck"]) || s.Keys["baseurl"] != "http://lab/" {
		t.Fatalf("vendor override not applied: %+v", s)
	}
	_ = os.WriteFile(filepath.Join(p.OverrideDir, "99-config_manager.repo"), []byte("[basalt]\nenabled=0\n"), 0o644)
	if s := ReadRepos(p)["basalt"]; s.Keys["enabled"] != "0" || !truthy(s.Keys["repo_gpgcheck"]) {
		t.Errorf("config-manager override: %+v", s)
	}
	_ = os.WriteFile(filepath.Join(p.OverrideDir, "20-basalt-signatures.repo"), []byte("[basalt]\nname=masked\n"), 0o644)
	if s := ReadRepos(p)["basalt"]; truthy(s.Keys["gpgcheck"]) || s.Keys["name"] != "masked" {
		t.Errorf("an /etc file of the same name must mask the vendor override: %+v", s)
	}
}

// fakeWeb serves fixed files.
func fakeWeb(files map[string]string) Fetcher {
	return func(ctx context.Context, url string) ([]byte, error) {
		if err := CheckURL(url); err != nil {
			return nil, err
		}
		if b, ok := files[url]; ok {
			return []byte(b), nil
		}
		return nil, errors.New("404 " + url)
	}
}

func TestWriterAddRemove(t *testing.T) {
	p := tempPaths(t)
	ms, _ := os.ReadFile("testdata/microsoft.asc")
	docker, _ := os.ReadFile("testdata/docker.asc")
	var ran []string
	w := Writer{Paths: p, Run: func(ctx context.Context, argv ...string) (string, error) {
		ran = append(ran, strings.Join(argv, " "))
		return "", nil
	}, Now: func() time.Time { return time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC) }}

	// The key published now differs from the one confirmed: nothing added.
	w.Fetch = fakeWeb(map[string]string{"https://packages.microsoft.com/keys/microsoft.asc": string(docker)})
	if err := w.Add(context.Background(), vscode()); err == nil || !strings.Contains(err.Error(), "nothing was added") {
		t.Fatalf("mismatch: %v", err)
	}
	if HasRecord(p, "vscode") {
		t.Fatal("recorded after a mismatch")
	}

	w.Fetch = fakeWeb(map[string]string{"https://packages.microsoft.com/keys/microsoft.asc": string(ms)})
	if err := w.Add(context.Background(), vscode()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p.RepoPath("vscode"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[vscode]", "gpgcheck=1", "repo_gpgcheck=1", "gpgkey=file://" + p.KeyPath("vscode"), "baseurl=https://packages.microsoft.com/yumrepos/vscode"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("repo file lacks %q:\n%s", want, b)
		}
	}
	if len(ran) == 0 || !strings.HasPrefix(ran[0], "rpm --import") {
		t.Errorf("ran: %v", ran)
	}
	if err := Stamp(p, "vscode", "p-abc123", "root at a terminal, user basalt"); err != nil {
		t.Fatal(err)
	}
	r := Build(p)
	if len(r.Sources) != 1 || r.Sources[0].KeyOwner != "Microsoft (Release signing) <gpgsecurity@microsoft.com>" ||
		r.Sources[0].Proposal != "p-abc123" || !r.Sources[0].Enabled {
		t.Fatalf("sources: %+v", r.Sources)
	}
	for _, c := range r.Catalog {
		if c.ID == "vscode" && !c.Added {
			t.Error("catalog does not know it was added")
		}
	}
	if err := w.Add(context.Background(), vscode()); err == nil {
		t.Error("added twice")
	}
	ran = nil
	if err := w.Remove(context.Background(), "vscode"); err != nil {
		t.Fatal(err)
	}
	if HasRecord(p, "vscode") {
		t.Error("record left")
	}
	if _, err := os.Stat(p.KeyPath("vscode")); !os.IsNotExist(err) {
		t.Error("key file left")
	}
	if len(ran) == 0 || !strings.Contains(ran[0], strings.ToLower(msFpr)) {
		t.Errorf("key not removed from rpm: %v", ran)
	}
	if err := w.Remove(context.Background(), "fedora"); err == nil {
		t.Error("removed a repository Basalt did not add")
	}
}

func TestParseRepoFile(t *testing.T) {
	good := "[example]\nname=Example\nbaseurl=https://repo.example.org/f$releasever/\ngpgcheck=1\nrepo_gpgcheck=1\ngpgkey=https://repo.example.org/key.asc\n"
	r, err := ParseRepoFile([]byte(good))
	if err != nil || r.ID != "example" || r.KeyURL != "https://repo.example.org/key.asc" || !r.RepoGPGCheck {
		t.Fatalf("%+v %v", r, err)
	}
	for name, s := range map[string]string{
		"http":         strings.Replace(good, "https://repo", "http://repo", 1),
		"no key":       strings.Replace(good, "gpgkey=https://repo.example.org/key.asc\n", "", 1),
		"gpgcheck off": strings.Replace(good, "gpgcheck=1\nrepo", "gpgcheck=0\nrepo", 1),
		"file key":     strings.Replace(good, "gpgkey=https://repo.example.org/key.asc", "gpgkey=file:///etc/pki/key", 1),
		"no url":       "[x]\nname=x\ngpgcheck=1\ngpgkey=https://a.example/k\n",
	} {
		if _, err := ParseRepoFile([]byte(s)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestFlatpakrepoKey(t *testing.T) {
	if _, err := FlatpakrepoKey([]byte("[Flatpak Repo]\nUrl=https://x.example/repo/\n")); err == nil {
		t.Error("a remote without a key accepted")
	}
}
