// Package sign makes and checks signed ledger exports: a JSON document
// with the selected records, the chain position they come from and a
// signature over the document's canonical form.
//
// Two kinds of host key sign exports (docs/ledger.md):
//
//   - "tpm": an ECDSA P-256 key held by the host's TPM 2.0 (a primary key
//     under the owner hierarchy, package tpm). The private half never
//     leaves the TPM, so an export signed with it was made on this
//     machine. Used whenever a TPM is present.
//   - "software": an Ed25519 key generated on first start and kept with
//     the ledger (it survives a root rollback), for machines without a
//     TPM. Anyone who can read the key file as root can sign with it.
//
// Exports made before key kinds existed say "development"; they are
// software keys and still verify. The format does not change: the
// key_alg field (absent means Ed25519) says how to check the signature.
package sign

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/record"
	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/tpm"
)

// Format names the export document format.
const Format = "basalt-ledger-export/1"

// Key kinds and signature algorithms.
const (
	KindTPM         = "tpm"
	KindSoftware    = "software"
	KindDevelopment = "development" // exports made by 0.1.0 (a software key)

	AlgEd25519   = "ed25519"
	AlgECDSAP256 = "ecdsa-p256-sha256"
)

// Export is a signed export.
type Export struct {
	Format    string          `json:"format"`
	Generated string          `json:"generated"`
	Host      string          `json:"host"`
	Filter    json.RawMessage `json:"filter,omitempty"`
	Chain     ChainInfo       `json:"chain"`
	Records   []record.Record `json:"records"`
	KeyID     string          `json:"key_id"`
	KeyKind   string          `json:"key_kind"`          // tpm or software ("development" in 0.1.0 exports)
	KeyAlg    string          `json:"key_alg,omitempty"` // empty: ed25519
	PublicKey string          `json:"public_key"`        // ed25519: raw key; ecdsa: PKIX DER (base64)
	Signature string          `json:"signature,omitempty"`
}

// ChainInfo places the export in the ledger's chain.
type ChainInfo struct {
	Verified bool   `json:"verified"`
	FirstSeq int64  `json:"first_seq"`
	LastSeq  int64  `json:"last_seq"`
	HeadSeq  int64  `json:"head_seq"`
	HeadHash string `json:"head_hash"`
	Error    string `json:"error,omitempty"`
}

// Key is the export signing key.
type Key struct {
	Public crypto.PublicKey
	Kind   string
	Alg    string
	// sign signs the SHA-256 digest of the export payload.
	sign func(digest []byte) ([]byte, error)
}

// Sign signs a SHA-256 digest.
func (k *Key) Sign(digest []byte) ([]byte, error) { return k.sign(digest) }

// ID is a short fingerprint of a public key: SHA-256 of the raw Ed25519
// key, or of the PKIX encoding for other keys, first 8 bytes in hex.
func ID(pub crypto.PublicKey) string {
	var b []byte
	switch k := pub.(type) {
	case ed25519.PublicKey:
		b = k
	default:
		der, err := x509.MarshalPKIXPublicKey(pub)
		if err != nil {
			return ""
		}
		b = der
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// PublicPEM is the public key in the standard PEM form ("PUBLIC KEY",
// PKIX), which OpenSSL reads too.
func PublicPEM(pub crypto.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

// LoadOrCreate reads the software key at path (PEM, raw Ed25519 seed) or
// creates it (mode 0600) with its public half next to it (path + ".pub").
func LoadOrCreate(path string) (*Key, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		blk, _ := pem.Decode(b)
		if blk == nil || blk.Type != "BASALT LEDGER ED25519 SEED" || len(blk.Bytes) != ed25519.SeedSize {
			return nil, fmt.Errorf("%s: not a ledger export key", path)
		}
		return softwareKey(ed25519.NewKeyFromSeed(blk.Bytes)), nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	seed := pem.EncodeToMemory(&pem.Block{Type: "BASALT LEDGER ED25519 SEED", Bytes: priv.Seed()})
	if err := os.WriteFile(path, seed, 0o600); err != nil {
		return nil, err
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "BASALT LEDGER ED25519 PUBLIC KEY", Bytes: pub})
	if err := os.WriteFile(path+".pub", pubPEM, 0o644); err != nil {
		return nil, err
	}
	return softwareKey(priv), nil
}

func softwareKey(priv ed25519.PrivateKey) *Key {
	return &Key{Public: priv.Public(), Kind: KindSoftware, Alg: AlgEd25519,
		sign: func(d []byte) ([]byte, error) { return ed25519.Sign(priv, d), nil }}
}

// TPMKey returns the host's TPM-held signing key (device: the TPM
// resource manager, tpm.DefaultDevice when empty). It creates the key
// once to learn its public half and signs a test digest; each signature
// later opens the TPM, recreates the same primary key, checks it is still
// the same key (a cleared TPM gives another one) and signs.
func TPMKey(device string) (*Key, error) {
	pub, err := tpmPublic(device)
	if err != nil {
		return nil, err
	}
	k := &Key{Public: pub, Kind: KindTPM, Alg: AlgECDSAP256}
	k.sign = func(digest []byte) ([]byte, error) {
		t, err := tpm.Open(device)
		if err != nil {
			return nil, err
		}
		defer t.Close()
		h, now, err := t.CreateSigningKey()
		if err != nil {
			return nil, err
		}
		defer t.Flush(h)
		if !now.Equal(pub) {
			return nil, fmt.Errorf("the TPM key changed (was %s, now %s): the TPM was cleared; restart basalt-ledger to use the new key", ID(pub), ID(now))
		}
		return t.Sign(h, digest)
	}
	test := sha256.Sum256([]byte("basalt-ledger TPM key self-test"))
	sig, err := k.Sign(test[:])
	if err != nil {
		return nil, err
	}
	if !ecdsa.VerifyASN1(pub, test[:], sig) {
		return nil, errors.New("the TPM's test signature does not verify")
	}
	return k, nil
}

func tpmPublic(device string) (*ecdsa.PublicKey, error) {
	t, err := tpm.Open(device)
	if err != nil {
		return nil, err
	}
	defer t.Close()
	h, pub, err := t.CreateSigningKey()
	if err != nil {
		return nil, err
	}
	_ = t.Flush(h)
	return pub, nil
}

// ReadPublic reads a public key file: the standard PEM form (PUBLIC KEY,
// Ed25519 or ECDSA P-256) or the Ed25519 form written by LoadOrCreate.
func ReadPublic(path string) (crypto.PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	switch {
	case blk == nil:
	case blk.Type == "BASALT LEDGER ED25519 PUBLIC KEY" && len(blk.Bytes) == ed25519.PublicKeySize:
		return ed25519.PublicKey(blk.Bytes), nil
	case blk.Type == "PUBLIC KEY":
		pub, err := x509.ParsePKIXPublicKey(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		switch k := pub.(type) {
		case ed25519.PublicKey:
			return k, nil
		case *ecdsa.PublicKey:
			if k.Curve == elliptic.P256() {
				return k, nil
			}
		}
	}
	return nil, fmt.Errorf("%s: not a ledger public key", path)
}

// payload is the signed form: the SHA-256 of the format name and the
// document without its signature.
func payload(e Export) ([]byte, error) {
	e.Signature = ""
	b, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(append([]byte(Format+"\n"), b...))
	return sum[:], nil
}

// Sign fills the key fields and the signature.
func Sign(e *Export, k *Key) error {
	e.Format, e.KeyID, e.KeyKind = Format, ID(k.Public), k.Kind
	switch pub := k.Public.(type) {
	case ed25519.PublicKey:
		e.KeyAlg = "" // the 0.1.0 form: absent means Ed25519
		e.PublicKey = base64.StdEncoding.EncodeToString(pub)
	default:
		der, err := x509.MarshalPKIXPublicKey(pub)
		if err != nil {
			return err
		}
		e.KeyAlg = k.Alg
		e.PublicKey = base64.StdEncoding.EncodeToString(der)
	}
	p, err := payload(*e)
	if err != nil {
		return err
	}
	sig, err := k.Sign(p)
	if err != nil {
		return fmt.Errorf("signing with the %s key: %w", k.Kind, err)
	}
	e.Signature = base64.StdEncoding.EncodeToString(sig)
	return nil
}

// embeddedKey decodes the export's public key for its algorithm.
func embeddedKey(e Export) (crypto.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(e.PublicKey)
	if err != nil {
		return nil, errors.New("bad embedded public key")
	}
	switch e.KeyAlg {
	case "", AlgEd25519:
		if len(raw) != ed25519.PublicKeySize {
			return nil, errors.New("bad embedded public key")
		}
		return ed25519.PublicKey(raw), nil
	case AlgECDSAP256:
		pub, err := x509.ParsePKIXPublicKey(raw)
		if k, ok := pub.(*ecdsa.PublicKey); err == nil && ok && k.Curve == elliptic.P256() {
			return k, nil
		}
		return nil, errors.New("bad embedded public key")
	}
	return nil, fmt.Errorf("unknown key algorithm %q", e.KeyAlg)
}

type equaler interface {
	Equal(crypto.PublicKey) bool
}

// Verify checks the signature (against trusted, or the embedded key when
// trusted is nil, which only proves integrity, not origin) and every
// record's own hash, and that consecutive records in the export are
// chained where their sequence numbers are consecutive.
func Verify(e Export, trusted crypto.PublicKey) (string, error) {
	if e.Format != Format {
		return "", fmt.Errorf("unknown format %q", e.Format)
	}
	key, err := embeddedKey(e)
	if err != nil {
		return "", err
	}
	origin := "integrity only: signed by the key embedded in the export (pass the host's public key to check origin)"
	if trusted != nil {
		if eq, ok := key.(equaler); !ok || !eq.Equal(trusted) {
			return "", fmt.Errorf("signed by key %s, not by the trusted key %s", ID(key), ID(trusted))
		}
		origin = "signed by the trusted key " + ID(trusted)
	}
	sig, err := base64.StdEncoding.DecodeString(e.Signature)
	if err != nil {
		return "", errors.New("bad signature encoding")
	}
	p, err := payload(e)
	if err != nil {
		return "", err
	}
	var ok bool
	switch k := key.(type) {
	case ed25519.PublicKey:
		ok = ed25519.Verify(k, p, sig)
	case *ecdsa.PublicKey:
		ok = ecdsa.VerifyASN1(k, p, sig)
	}
	if !ok {
		return "", errors.New("signature does not match: the export was changed")
	}
	for i, r := range e.Records {
		if record.HashOf(r) != r.Hash {
			return "", fmt.Errorf("record %d: content does not match its hash", r.Seq)
		}
		if i > 0 && r.Seq == e.Records[i-1].Seq+1 && r.Prev != e.Records[i-1].Hash {
			return "", fmt.Errorf("record %d: not chained to record %d", r.Seq, e.Records[i-1].Seq)
		}
	}
	return origin, nil
}

// NewForTest returns a key that signs with priv (tests in other packages).
func NewForTest(priv crypto.Signer, kind string) *Key {
	k := &Key{Public: priv.Public(), Kind: kind}
	switch p := priv.(type) {
	case ed25519.PrivateKey:
		k.Alg = AlgEd25519
		k.sign = func(d []byte) ([]byte, error) { return ed25519.Sign(p, d), nil }
	case *ecdsa.PrivateKey:
		k.Alg = AlgECDSAP256
		k.sign = func(d []byte) ([]byte, error) { return ecdsa.SignASN1(rand.Reader, p, d) }
	}
	return k
}
