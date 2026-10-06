package cli

import (
	"testing"

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
