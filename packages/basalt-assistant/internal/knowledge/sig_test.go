package knowledge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Throwaway test key (testdata/sig/README): the secret part was discarded.
var testTrust = TrustAnchor{
	Primary: "D986 089B 64B7 3112 72AC A248 EADD 0248 F2A7 AA92",
	Signer:  "96B79DC85FD762B956BA11C4012C95042E303AFC",
}

const revokedSubkey = "EE5BD285241E21E52769DC623B944285AA68E0C1"

// signedIndex copies the test index and the named signature into a
// temporary directory as manifest.json.sig.
func signedIndex(t *testing.T, sig string) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{"cases.jsonl", "index.bin", "manifest.json"} {
		b, err := os.ReadFile(filepath.Join("testdata/index", f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if sig != "" {
		b, err := os.ReadFile(filepath.Join("testdata/sig", sig))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, SigFile), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func verifier(cert string, tr TrustAnchor) Verifier {
	return Verifier{KeyFile: filepath.Join("testdata/sig", cert), Trust: tr,
		Now: func() time.Time { return time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC) }}
}

// The real OpenBasalt release key (basalt-release): the pinned knowledge
// subkey is bound to the pinned primary (binding and back signature made
// by gpg at the key ceremony, checked here with the standard library).
func TestReleaseKeyBindsKnowledgeSubkey(t *testing.T) {
	b, err := os.ReadFile("testdata/sig/openbasalt-release-key.asc")
	if err != nil {
		t.Fatal(err)
	}
	// The copy here must be the key basalt-release installs (checked when
	// the whole repository is there; the RPM build has only this package).
	for _, p := range []string{"../../../basalt-release/RPM-GPG-KEY-basalt", "../../../basalt-knowledge/openbasalt-release-key.asc"} {
		if rel, err := os.ReadFile(p); err == nil && string(rel) != string(b) {
			t.Errorf("testdata/sig/openbasalt-release-key.asc differs from %s", p)
		}
	}
	k, err := loadSigningKey(b, OpenBasaltKnowledge)
	if err != nil {
		t.Fatal(err)
	}
	if k.key.fpr != OpenBasaltKnowledge.Signer || k.primary.fpr != OpenBasaltKnowledge.Primary {
		t.Errorf("fingerprints %s %s", k.key.fpr, k.primary.fpr)
	}
	if got := k.expires.Format("2006-01-02"); got != "2028-10-03" {
		t.Errorf("subkey expires %s", got)
	}
	if k.key.pub.N.BitLen() != 4096 {
		t.Errorf("subkey has %d bits", k.key.pub.N.BitLen())
	}
	// Another primary pinned: refused, whatever the subkey.
	if _, err := loadSigningKey(b, TrustAnchor{Primary: testTrust.Primary, Signer: OpenBasaltKnowledge.Signer}); err == nil {
		t.Error("a certificate with another primary key was accepted")
	}
}

func TestSignedIndexOpens(t *testing.T) {
	for _, sig := range []string{"manifest.json.sig", "manifest.json.sig.asc", "manifest.json.sig-sha512"} {
		ix, err := OpenSigned(signedIndex(t, sig), verifier("cert.asc", testTrust))
		if err != nil {
			t.Fatalf("%s: %v", sig, err)
		}
		if ix.Signature == nil || ix.Signature.Signer != testTrust.Signer || ix.Signature.Created.Year() != 2026 {
			t.Errorf("%s: %+v", sig, ix.Signature)
		}
		if !strings.Contains(ix.Version(), "signed by 012C95042E303AFC") {
			t.Errorf("version %q", ix.Version())
		}
	}
}

func TestSignatureRefusals(t *testing.T) {
	v := verifier("cert.asc", testTrust)
	cases := []struct {
		name string
		dir  func() string
		v    Verifier
		want string
	}{
		{"unsigned", func() string { return signedIndex(t, "") }, v, "not signed"},
		{"manifest changed", func() string {
			d := signedIndex(t, "manifest.json.sig")
			b, _ := os.ReadFile(filepath.Join(d, "manifest.json"))
			_ = os.WriteFile(filepath.Join(d, "manifest.json"), append(b, ' '), 0o644)
			return d
		}, v, "signature"},
		{"cases changed", func() string {
			d := signedIndex(t, "manifest.json.sig")
			b, _ := os.ReadFile(filepath.Join(d, "cases.jsonl"))
			_ = os.WriteFile(filepath.Join(d, "cases.jsonl"), append(b, '\n'), 0o644)
			return d
		}, v, "SHA-256"},
		{"index changed", func() string {
			d := signedIndex(t, "manifest.json.sig")
			b, _ := os.ReadFile(filepath.Join(d, "index.bin"))
			b[len(b)-1] ^= 1
			_ = os.WriteFile(filepath.Join(d, "index.bin"), b, 0o644)
			return d
		}, v, "index.bin"},
		{"SHA-1 signature", func() string { return signedIndex(t, "manifest.json.sig-sha1") }, v, "hash algorithm"},
		{"manifest without index_sha256", func() string {
			d := signedIndex(t, "manifest-noindex.json.sig")
			b, _ := os.ReadFile("testdata/sig/manifest-noindex.json")
			_ = os.WriteFile(filepath.Join(d, "manifest.json"), b, 0o644)
			return d
		}, v, "index_sha256"},
		{"revoked subkey pinned", func() string { return signedIndex(t, "manifest.json.sig") },
			verifier("cert.asc", TrustAnchor{Primary: testTrust.Primary, Signer: revokedSubkey}), "revoked"},
		{"another subkey pinned", func() string { return signedIndex(t, "manifest.json.sig") },
			verifier("cert-before-revoke.asc", TrustAnchor{Primary: testTrust.Primary, Signer: revokedSubkey}), "another key"},
		{"another primary pinned", func() string { return signedIndex(t, "manifest.json.sig") },
			verifier("cert.asc", OpenBasaltKnowledge), "primary"},
		{"real key file, test signature", func() string { return signedIndex(t, "manifest.json.sig") },
			Verifier{KeyFile: "testdata/sig/openbasalt-release-key.asc", Trust: OpenBasaltKnowledge}, "another key"},
		{"no key file", func() string { return signedIndex(t, "manifest.json.sig") },
			verifier("missing.asc", testTrust), "knowledge key"},
	}
	for _, c := range cases {
		_, err := OpenSigned(c.dir(), c.v)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v (want an error with %q)", c.name, err, c.want)
		}
	}
}

func TestSignatureTimes(t *testing.T) {
	cert, _ := os.ReadFile("testdata/sig/cert.asc")
	sig, _ := os.ReadFile("testdata/sig/manifest.json.sig")
	mb, _ := os.ReadFile("testdata/index/manifest.json")
	k, err := loadSigningKey(cert, testTrust)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	if _, err := k.verifyDetached(sig, mb, now); err != nil {
		t.Fatal(err)
	}
	expired := *k
	expired.expires = k.key.created.Add(time.Second)
	if _, err := expired.verifyDetached(sig, mb, now); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("made after the subkey expired: %v", err)
	}
	if _, err := k.verifyDetached(sig, mb, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Error("a signature from the future was accepted")
	}
}

func TestArmorAndPackets(t *testing.T) {
	if _, err := dearmor([]byte("-----BEGIN PGP SIGNATURE-----\n\nAAAA\n=AAAA\n-----END PGP SIGNATURE-----\n")); err == nil {
		t.Error("bad armor checksum accepted")
	}
	for _, b := range [][]byte{{0x88}, {0x8b, 0, 0}, {0xc2, 0xe0}, {0x7f}} {
		if _, err := readPackets(b); err == nil {
			t.Errorf("packets %x accepted", b)
		}
	}
}
