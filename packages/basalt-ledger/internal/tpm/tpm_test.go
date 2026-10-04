package tpm

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"math/big"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/tpm/tpmsim"
)

func TestCreateSignVerify(t *testing.T) {
	sim := tpmsim.New()
	sim.Retries = 2
	tp := New(sim)
	h, pub, err := tp.CreateSigningKey()
	if err != nil {
		t.Fatal(err, sim.Err)
	}
	h2, pub2, err := tp.CreateSigningKey()
	if err != nil || !pub.Equal(pub2) {
		t.Fatal("the same template must give the same key")
	}
	_ = tp.Flush(h2)
	digest := sha256.Sum256([]byte("export"))
	sig, err := tp.Sign(h, digest[:])
	if err != nil {
		t.Fatal(err, sim.Err)
	}
	if !ecdsa.VerifyASN1(pub, digest[:], sig) {
		t.Fatal("signature does not verify")
	}
	var rs struct{ R, S *big.Int }
	if _, err := asn1.Unmarshal(sig, &rs); err != nil {
		t.Fatal(err)
	}
	other := sha256.Sum256([]byte("other"))
	if ecdsa.VerifyASN1(pub, other[:], sig) {
		t.Fatal("signature verifies another digest")
	}
	if err := tp.Flush(h); err != nil {
		t.Fatal(err)
	}
	var rc *RCError
	if _, err := tp.Sign(h, digest[:]); !errors.As(err, &rc) || rc.Code != 0x18b {
		t.Fatalf("sign with a flushed handle: %v", err)
	}
	if _, err := tp.Sign(h, []byte("short")); err == nil {
		t.Fatal("non SHA-256 digest accepted")
	}
	// A different owner seed (a cleared TPM) gives a different key.
	sim.Seed = []byte("new seed")
	_, pub3, err := tp.CreateSigningKey()
	if err != nil || pub3.Equal(pub) {
		t.Fatal("a new owner seed must change the key")
	}
	if sim.Err != nil {
		t.Fatal("encoding:", sim.Err)
	}
}

func TestTemplateLayout(t *testing.T) {
	sum := sha256.Sum256(Label)
	tmpl := template(sum[:])
	// type, nameAlg, attributes, empty policy, 5 algorithm words, x (2+32), empty y.
	if len(tmpl) != 2+2+4+2+10+34+2 {
		t.Fatalf("template is %d bytes", len(tmpl))
	}
	if !bytes.Equal(tmpl[:2], []byte{0x00, 0x23}) || binary.BigEndian.Uint32(tmpl[4:8]) != 0x00040472 {
		t.Fatalf("% x", tmpl[:8])
	}
}
