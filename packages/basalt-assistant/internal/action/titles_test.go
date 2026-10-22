package action

import (
	"testing"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/sources"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/titles"
)

// TestDescriptionsAreKnownTitles: the description of every command an
// action runs is a form internal/titles knows, so a proposal titled with
// Describe (the MCP server's typed actions) is shown translated, and what
// every check of an action proves is a phrase it knows. A new or changed
// description needs its form there and its translation in po/.
func TestDescriptionsAreKnownTitles(t *testing.T) {
	withSources(t)
	pk := "kernel-core-6.17.3-200.fc44.x86_64 openssl-libs-1:3.5.9-1.fc44.x86_64"
	e, _ := sources.Lookup("docker-ce")
	src := sources.Params{ID: "docker-ce-stable", Name: e.Repos[0].Name, Kind: sources.KindRPM, URLType: sources.URLBase, URL: e.Repos[0].URL,
		KeyURL: e.KeyURL, Fingerprint: e.Fingerprint, GPGCheck: "1", RepoGPGCheck: "1", Catalog: "docker-ce", Group: "docker-ce"}
	all := []Action{
		{SELinuxFcontext, map[string]string{"path": "/srv/nginx-logs", "type": "httpd_log_t"}},
		{SELinuxRestorecon, map[string]string{"path": "/srv/www"}},
		{SELinuxPort, map[string]string{"proto": "tcp", "port": "8099", "type": "http_port_t", "mode": "add"}},
		{SELinuxBoolean, map[string]string{"name": "httpd_can_network_connect", "value": "on"}},
		{UnitRestart, map[string]string{"unit": "nginx.service"}},
		{FileRestore, map[string]string{"path": "/etc/nginx/nginx.conf", "snapshot": "3"}},
		{SnapshotRollback, map[string]string{"snapshot": "12"}},
		{SnapshotDelete, map[string]string{"snapshot": "5"}},
		{JournalVacuum, map[string]string{"size": "200M"}},
		{DnfClean, map[string]string{}},
		{DriverInstall, map[string]string{"driver": "nvidia", "variant": "display", "kernel": "6.17.3-200.fc44.x86_64", "license": NvidiaLicenseSHA256}},
		{UpdateCheck, map[string]string{}},
		{UpdateInstall, map[string]string{"scope": "all", "count": "2", "packages": pk, "digest": UpdateDigest(pk), "mode": "offline"}},
		{UpdateInstall, map[string]string{"scope": "security", "count": "1", "packages": "openssl-libs-1:3.5.9-1.fc44.x86_64", "digest": UpdateDigest("openssl-libs-1:3.5.9-1.fc44.x86_64"), "mode": "live"}},
		{UpdateRollback, map[string]string{"snapshot": "42", "proposal": "p-a1b2c3"}},
		{RepoEnable, map[string]string{"repo": "basalt-nonfree-testing", "consent": sources.TestingConsentToken, "definition": "install"}},
		{RepoDisable, map[string]string{"repo": "basalt-testing"}},
		{SourceAdd, src.Map()},
		{SourceRemove, map[string]string{"id": "example"}},
		{KeyboardSystem, map[string]string{"layouts": "br,us(intl)", "options": "grp:alt_shift_toggle", "model": "pc105", "keymap": "br"}},
		{AuditRun, map[string]string{"run": "2026-10-08-101500", "install": "yes"}},
		{RiskAccept, map[string]string{"item": "encryption", "by": "edimar"}},
		{RiskReview, map[string]string{"item": "tpm"}},
	}
	kinds := map[string]bool{}
	for _, a := range all {
		cmds, err := a.Commands()
		if err != nil {
			t.Errorf("%s: %v", a.Kind, err)
			continue
		}
		kinds[a.Kind] = true
		for _, c := range cmds {
			if !titles.Known(c.Description) {
				t.Errorf("%s: %q is not a form of internal/titles", a.Kind, c.Description)
			}
		}
		// What each check proves is stored in the result and shown
		// translated (basalt show, basalt apply).
		for _, c := range append(a.Verify(time.Now()), a.InstalledChecks()...) {
			if !titles.KnownPhrase(c.Description) {
				t.Errorf("%s: check %q is not a phrase of internal/titles", a.Kind, c.Description)
			}
		}
	}
	if len(kinds) < 22 {
		t.Errorf("only %d kinds checked", len(kinds))
	}
}
