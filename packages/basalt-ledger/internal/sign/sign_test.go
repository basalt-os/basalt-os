package sign

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/record"
	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/tpm"
	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/tpm/tpmsim"
)

func twoRecords() []record.Record {
	r1 := record.Record{V: 1, Seq: 1, Producer: "basalt-agent", Event: "session.start", Outcome: "ok", Prev: record.ZeroHash}
	r1.Hash = record.HashOf(r1)
	r2 := record.Record{V: 1, Seq: 2, Producer: "basalt-agent", Event: "session.end", Outcome: "ok", Prev: r1.Hash}
	r2.Hash = record.HashOf(r2)
	return []record.Record{r1, r2}
}

// checkKey signs an export with k and runs the tamper checks against pub.
func checkKey(t *testing.T, k *Key, pub crypto.PublicKey) Export {
	t.Helper()
	recs := twoRecords()
	e := Export{Generated: "2026-10-04T00:00:00Z", Host: "lab", Records: recs}
	if err := Sign(&e, k); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(e, pub); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(e, nil); err != nil {
		t.Fatal("integrity-only check:", err)
	}
	// Another trusted key: refused.
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := Verify(e, other); err == nil {
		t.Fatal("verified against the wrong key")
	}
	otherEC, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if _, err := Verify(e, otherEC.Public()); err == nil {
		t.Fatal("verified against the wrong ECDSA key")
	}
	// Changed host, changed key kind, removed record, broken chain: refused.
	e1 := e
	e1.Host = "elsewhere"
	if _, err := Verify(e1, pub); err == nil {
		t.Fatal("changed export verified")
	}
	e1 = e
	e1.KeyKind = "tpm-claimed"
	if _, err := Verify(e1, pub); err == nil {
		t.Fatal("export with a changed key kind verified")
	}
	e2 := e
	e2.Records = e.Records[:1]
	if _, err := Verify(e2, pub); err == nil {
		t.Fatal("export with a removed record verified")
	}
	e3 := e
	bad := recs[1]
	bad.Prev = record.ZeroHash
	bad.Hash = record.HashOf(bad)
	e3.Records = []record.Record{recs[0], bad}
	_ = Sign(&e3, k)
	if _, err := Verify(e3, pub); err == nil {
		t.Fatal("broken chain verified")
	}
	return e
}

func TestSoftwareKey(t *testing.T) {
	dir := t.TempDir()
	k, err := LoadOrCreate(filepath.Join(dir, "k"))
	if err != nil {
		t.Fatal(err)
	}
	if k.Kind != KindSoftware || k.Alg != AlgEd25519 {
		t.Fatalf("kind %s alg %s", k.Kind, k.Alg)
	}
	k2, err := LoadOrCreate(filepath.Join(dir, "k"))
	if err != nil || !k2.Public.(ed25519.PublicKey).Equal(k.Public) {
		t.Fatal("key not reloaded")
	}
	pub, err := ReadPublic(filepath.Join(dir, "k.pub"))
	if err != nil || !pub.(ed25519.PublicKey).Equal(k.Public) {
		t.Fatal("public key file")
	}
	e := checkKey(t, k, pub)
	if e.KeyAlg != "" {
		t.Fatalf("Ed25519 exports keep the 0.1.0 form (no key_alg), got %q", e.KeyAlg)
	}
	// The standard PEM form of the same key works too.
	p, _ := PublicPEM(k.Public)
	_ = os.WriteFile(filepath.Join(dir, "k.pem"), p, 0o644)
	pub2, err := ReadPublic(filepath.Join(dir, "k.pem"))
	if err != nil || ID(pub2) != ID(k.Public) {
		t.Fatal("PKIX Ed25519 public key")
	}
}

// An export written by basalt-ledger 0.1.0 (key_kind "development", no
// key_alg) still verifies.
func TestLegacyExport(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	k := NewForTest(priv, KindDevelopment)
	e := Export{Generated: "2026-10-04T00:00:00Z", Host: "lab", Records: twoRecords()}
	if err := Sign(&e, k); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(e)
	if strings.Contains(string(b), "key_alg") {
		t.Fatal("Ed25519 export carries key_alg")
	}
	var back Export
	_ = json.Unmarshal(b, &back)
	if _, err := Verify(back, priv.Public()); err != nil {
		t.Fatal(err)
	}
}

func TestECDSAKey(t *testing.T) {
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	k := NewForTest(priv, KindTPM)
	e := checkKey(t, k, priv.Public())
	if e.KeyAlg != AlgECDSAP256 || e.KeyKind != KindTPM {
		t.Fatalf("alg %q kind %q", e.KeyAlg, e.KeyKind)
	}
	// The public key file in the standard PEM form verifies it.
	dir := t.TempDir()
	p, err := PublicPEM(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "export.pub"), p, 0o644)
	pub, err := ReadPublic(filepath.Join(dir, "export.pub"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(e, pub); err != nil {
		t.Fatal(err)
	}
	// Claiming Ed25519 for an ECDSA export fails.
	e1 := e
	e1.KeyAlg = ""
	if _, err := Verify(e1, nil); err == nil {
		t.Fatal("algorithm swap verified")
	}
	// Another curve is not accepted as a ledger key.
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	p, _ = PublicPEM(p384.Public())
	_ = os.WriteFile(filepath.Join(dir, "p384.pub"), p, 0o644)
	if _, err := ReadPublic(filepath.Join(dir, "p384.pub")); err == nil {
		t.Fatal("P-384 key accepted")
	}
}

func TestTPMKey(t *testing.T) {
	sim := tpmsim.New()
	old := tpm.OpenFunc
	tpm.OpenFunc = func(string) (io.ReadWriteCloser, error) { return sim, nil }
	t.Cleanup(func() { tpm.OpenFunc = old })
	k, err := TPMKey("")
	if err != nil {
		t.Fatal(err, sim.Err)
	}
	if k.Kind != KindTPM || k.Alg != AlgECDSAP256 {
		t.Fatalf("kind %s alg %s", k.Kind, k.Alg)
	}
	e := checkKey(t, k, k.Public)
	if e.KeyKind != KindTPM || e.KeyAlg != AlgECDSAP256 {
		t.Fatalf("%+v", e)
	}
	// The same TPM gives the same key on the next start.
	k2, err := TPMKey("")
	if err != nil || ID(k2.Public) != ID(k.Public) {
		t.Fatal("TPM key not stable")
	}
	// A cleared TPM: signing with the old key refuses, a restart gets a new key.
	sim.Seed = []byte("cleared")
	ex := Export{Generated: "2026-10-04T00:00:00Z", Host: "lab", Records: twoRecords()}
	if err := Sign(&ex, k); err == nil || !strings.Contains(err.Error(), "cleared") {
		t.Fatalf("signing after a TPM clear: %v", err)
	}
	k3, err := TPMKey("")
	if err != nil || ID(k3.Public) == ID(k.Public) {
		t.Fatal("new TPM key after a clear")
	}
	// No TPM device: an error (the daemon falls back to the software key).
	tpm.OpenFunc = func(string) (io.ReadWriteCloser, error) { return nil, os.ErrNotExist }
	if _, err := TPMKey(""); err == nil {
		t.Fatal("TPM key without a TPM")
	}
	if sim.Err != nil {
		t.Fatal("encoding:", sim.Err)
	}
}
