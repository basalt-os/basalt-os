package action

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/sources"
)

// withSources points the source actions at a temporary directory with one
// source Basalt added (example).
func withSources(t *testing.T) {
	d := t.TempDir()
	old := SourcePaths
	SourcePaths = sources.Paths{RepoDir: filepath.Join(d, "repos"), SourcesDir: filepath.Join(d, "sources.d"), KeyDir: d}
	t.Cleanup(func() { SourcePaths = old })
	_ = os.MkdirAll(SourcePaths.SourcesDir, 0o755)
	_ = os.WriteFile(SourcePaths.RecordPath("example"), []byte(`{"id":"example"}`), 0o644)
}

func TestRepoToggleValidator(t *testing.T) {
	withSources(t)
	ok := []Action{
		{Kind: RepoEnable, Params: map[string]string{"repo": "basalt-tools"}},
		{Kind: RepoEnable, Params: map[string]string{"repo": "basalt-testing", "consent": sources.TestingConsentToken}},
		{Kind: RepoEnable, Params: map[string]string{"repo": "basalt-nonfree-testing", "consent": sources.TestingConsentToken, "definition": "install"}},
		{Kind: RepoDisable, Params: map[string]string{"repo": "basalt-testing"}},
		{Kind: RepoDisable, Params: map[string]string{"repo": "example"}},
		{Kind: RepoEnable, Params: map[string]string{"repo": "example"}},
	}
	for _, a := range ok {
		if err := a.Validate(); err != nil {
			t.Errorf("%v refused: %v", a, err)
		}
	}
	bad := map[string]Action{
		"unknown repo":              {Kind: RepoEnable, Params: map[string]string{"repo": "evil"}},
		"fedora":                    {Kind: RepoDisable, Params: map[string]string{"repo": "fedora"}},
		"updates":                   {Kind: RepoDisable, Params: map[string]string{"repo": "updates"}},
		"basalt always on":          {Kind: RepoDisable, Params: map[string]string{"repo": "basalt"}},
		"nonfree from drivers only": {Kind: RepoEnable, Params: map[string]string{"repo": "basalt-nonfree"}},
		"testing without consent":   {Kind: RepoEnable, Params: map[string]string{"repo": "basalt-testing"}},
		"testing wrong consent":     {Kind: RepoEnable, Params: map[string]string{"repo": "basalt-testing", "consent": "yes"}},
		"injection":                 {Kind: RepoEnable, Params: map[string]string{"repo": "basalt-tools.gpgcheck=0 x"}},
		"definition elsewhere":      {Kind: RepoEnable, Params: map[string]string{"repo": "basalt-tools", "definition": "install"}},
		"not added through basalt":  {Kind: RepoEnable, Params: map[string]string{"repo": "someone-else"}},
	}
	for name, a := range bad {
		if err := a.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	cmds, _ := Action{Kind: RepoEnable, Params: map[string]string{"repo": "basalt-nonfree-testing", "consent": sources.TestingConsentToken, "definition": "install"}}.Commands()
	var lines []string
	for _, c := range cmds {
		lines = append(lines, c.String())
	}
	want := "dnf -y install basalt-nonfree-release\ndnf -y upgrade basalt-nonfree-release\ndnf config-manager setopt basalt-nonfree-testing.enabled=1"
	if strings.Join(lines, "\n") != want {
		t.Errorf("commands:\n%s", strings.Join(lines, "\n"))
	}
}

func TestUpdateInstallValidator(t *testing.T) {
	pk := "kernel-core-6.17.3-200.fc44.x86_64 openssl-libs-1:3.5.9-1.fc44.x86_64"
	good := Action{Kind: UpdateInstall, Params: map[string]string{"scope": "all", "count": "2", "packages": pk, "digest": UpdateDigest(pk)}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	cmds, _ := good.Commands()
	if len(cmds) != 2 || cmds[0].String() != "dnf -y upgrade --downloadonly "+pk || cmds[1].String() != "dnf -y upgrade "+pk {
		t.Errorf("commands: %v", cmds)
	}
	for name, mod := range map[string]func(m map[string]string){
		"count":  func(m map[string]string) { m["count"] = "3" },
		"digest": func(m map[string]string) { m["packages"] = pk + " bash-5.3-1.fc44.x86_64"; m["count"] = "3" },
		"option": func(m map[string]string) {
			m["packages"] = "--nogpgcheck"
			m["count"] = "1"
			m["digest"] = UpdateDigest("--nogpgcheck")
		},
		"spec glob": func(m map[string]string) { m["packages"] = "*"; m["count"] = "1"; m["digest"] = UpdateDigest("*") },
		"scope":     func(m map[string]string) { m["scope"] = "everything" },
		"empty":     func(m map[string]string) { m["packages"] = ""; m["count"] = "0"; m["digest"] = UpdateDigest("") },
		"duplicates": func(m map[string]string) {
			p := "bash-5.3-1.fc44.x86_64 bash-5.3-1.fc44.x86_64"
			m["packages"] = p
			m["digest"] = UpdateDigest(p)
		},
	} {
		m := map[string]string{}
		for k, v := range good.Params {
			m[k] = v
		}
		mod(m)
		if err := (Action{Kind: UpdateInstall, Params: m}).Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if withoutEpoch("openssl-libs-1:3.5.9-1.fc44.x86_64") != "openssl-libs-3.5.9-1.fc44.x86_64" {
		t.Error(withoutEpoch("openssl-libs-1:3.5.9-1.fc44.x86_64"))
	}
}

func TestUpdateRollbackValidator(t *testing.T) {
	a := Action{Kind: UpdateRollback, Params: map[string]string{"snapshot": "42", "proposal": "p-a1b2c3"}}
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	cmds, _ := a.Commands()
	if len(cmds) != 1 || cmds[0].String() != "basalt-rollback --yes 42" {
		t.Errorf("%v", cmds)
	}
	if !NeedsRootView(UpdateRollback) {
		t.Error("a rollback from the confined view must stay a hint")
	}
	for _, p := range []map[string]string{{"snapshot": "0", "proposal": "p-a1b2c3"}, {"snapshot": "42", "proposal": "x"}, {"snapshot": "4 2", "proposal": "p-a1b2c3"}} {
		if (Action{Kind: UpdateRollback, Params: p}).Validate() == nil {
			t.Errorf("%v accepted", p)
		}
	}
}

func TestSourceActions(t *testing.T) {
	withSources(t)
	if err := (Action{Kind: SourceRemove, Params: map[string]string{"id": "example"}}).Validate(); err != nil {
		t.Error(err)
	}
	if (Action{Kind: SourceRemove, Params: map[string]string{"id": "fedora"}}).Validate() == nil {
		t.Error("removing a repository Basalt did not add was accepted")
	}
	e, _ := sources.Lookup("docker-ce")
	p := sources.Params{ID: "docker-ce-stable", Name: e.Repos[0].Name, Kind: sources.KindRPM, URLType: sources.URLBase, URL: e.Repos[0].URL,
		KeyURL: e.KeyURL, Fingerprint: e.Fingerprint, GPGCheck: "1", RepoGPGCheck: "1", Catalog: "docker-ce", Group: "docker-ce"}
	a := Action{Kind: SourceAdd, Params: p.Map()}
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	cmds, _ := a.Commands()
	if len(cmds) != 1 || cmds[0].Argv[0] != "basalt" || cmds[0].Argv[1] != "__source" {
		t.Errorf("%v", cmds)
	}
	p.Fingerprint = "BC528686B50D79E339D3721CEB3E94ADBE1229CF"
	if (Action{Kind: SourceAdd, Params: p.Map()}).Validate() == nil {
		t.Error("a catalog source with another key was accepted")
	}
}
