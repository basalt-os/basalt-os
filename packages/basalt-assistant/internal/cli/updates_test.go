package cli

import (
	"errors"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/runner"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/sources"
)

// The command a confirmed source.add runs reads back exactly the
// parameters the person confirmed.
func TestSourceHelperArgvRoundTrip(t *testing.T) {
	e, _ := sources.Lookup("docker-ce")
	want := sources.Params{ID: "docker-ce-stable", Name: e.Repos[0].Name, Kind: sources.KindRPM, URLType: sources.URLBase,
		URL: e.Repos[0].URL, KeyURL: e.KeyURL, Fingerprint: e.Fingerprint, GPGCheck: "1", RepoGPGCheck: "1",
		Catalog: "docker-ce", Group: "docker-ce"}
	argv := want.Argv()
	if argv[0] != "basalt" {
		t.Fatal(argv)
	}
	o, err := parse(argv[1:])
	if err != nil {
		t.Fatal(err)
	}
	if got := sourceParams(o); got != want {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	if err := sourceParams(o).Validate(); err != nil {
		t.Error(err)
	}
}

// Turning basalt-nonfree-testing on is proposed only when its definition
// can be installed: no proposal for a change that cannot work.
func TestNonfreeTestingDefinition(t *testing.T) {
	offered := &runner.Fake{Prefixes: map[string]runner.Result{
		"dnf -q --cacheonly repoquery": {Out: "basalt-nonfree-release\n"}}}
	missing := &runner.Fake{Prefixes: map[string]runner.Result{
		"dnf -q --cacheonly repoquery": {Out: ""}}}
	broken := &runner.Fake{} // no dnf cache: exit 127
	ctx := t.Context()
	if def, err := nonfreeTestingDefinition(ctx, missing, true); def != "" || err != nil {
		t.Errorf("already defined: %q %v", def, err)
	}
	if def, err := nonfreeTestingDefinition(ctx, offered, false); def != "install" || err != nil {
		t.Errorf("offered: %q %v", def, err)
	}
	for name, r := range map[string]runner.Reader{"missing": missing, "no cache": broken, "nil": nil} {
		if def, err := nonfreeTestingDefinition(ctx, r, false); def != "" || !errors.Is(err, ErrNonfreeTestingUnavailable) {
			t.Errorf("%s: %q %v", name, def, err)
		}
	}
}
