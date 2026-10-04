package sign

import (
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/record"
)

func TestSignVerify(t *testing.T) {
	dir := t.TempDir()
	k, err := LoadOrCreate(filepath.Join(dir, "k"))
	if err != nil {
		t.Fatal(err)
	}
	k2, err := LoadOrCreate(filepath.Join(dir, "k"))
	if err != nil || !k2.Public.Equal(k.Public) {
		t.Fatal("key not reloaded")
	}
	pub, err := ReadPublic(filepath.Join(dir, "k.pub"))
	if err != nil || !pub.Equal(k.Public) {
		t.Fatal("public key file")
	}
	r1 := record.Record{V: 1, Seq: 1, Producer: "basalt-agent", Event: "session.start", Outcome: "ok", Prev: record.ZeroHash}
	r1.Hash = record.HashOf(r1)
	r2 := record.Record{V: 1, Seq: 2, Producer: "basalt-agent", Event: "session.end", Outcome: "ok", Prev: r1.Hash}
	r2.Hash = record.HashOf(r2)
	e := Export{Generated: "2026-10-04T00:00:00Z", Host: "lab", Records: []record.Record{r1, r2}}
	if err := Sign(&e, k); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(e, pub); err != nil {
		t.Fatal(err)
	}
	// Another trusted key: refused.
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := Verify(e, other); err == nil {
		t.Fatal("verified against the wrong key")
	}
	// Changed host, removed record, broken chain: refused.
	e1 := e
	e1.Host = "elsewhere"
	if _, err := Verify(e1, pub); err == nil {
		t.Fatal("changed export verified")
	}
	e2 := e
	e2.Records = e.Records[:1]
	if _, err := Verify(e2, pub); err == nil {
		t.Fatal("export with a removed record verified")
	}
	// A re-signed export with a broken chain is refused on the records.
	e3 := e
	bad := r2
	bad.Prev = record.ZeroHash
	bad.Hash = record.HashOf(bad)
	e3.Records = []record.Record{r1, bad}
	_ = Sign(&e3, k)
	if _, err := Verify(e3, pub); err == nil {
		t.Fatal("broken chain verified")
	}
}
