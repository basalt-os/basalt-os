// Package sign makes and checks signed ledger exports: a JSON document
// with the selected records, the chain position they come from and an
// Ed25519 signature over the document's canonical form.
//
// The signing key is the host's ledger export key, generated on first
// start and kept with the ledger (it survives a root rollback). It is a
// development-grade key: a release would sign with a key whose public
// half is published and pinned (an OpenBasalt host or release subkey,
// docs/ledger.md); the format does not change.
package sign

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/record"
)

// Format names the export document format.
const Format = "basalt-ledger-export/1"

// Export is a signed export.
type Export struct {
	Format    string          `json:"format"`
	Generated string          `json:"generated"`
	Host      string          `json:"host"`
	Filter    json.RawMessage `json:"filter,omitempty"`
	Chain     ChainInfo       `json:"chain"`
	Records   []record.Record `json:"records"`
	KeyID     string          `json:"key_id"`
	KeyKind   string          `json:"key_kind"` // "development" for the per-host generated key
	PublicKey string          `json:"public_key"`
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
	Private ed25519.PrivateKey
	Public  ed25519.PublicKey
	Kind    string
}

// ID is a short fingerprint of a public key.
func ID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// LoadOrCreate reads the key at path (PEM, PKCS#8-like raw seed block) or
// creates it (mode 0600) with its public half next to it (path + ".pub").
func LoadOrCreate(path string) (*Key, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		blk, _ := pem.Decode(b)
		if blk == nil || blk.Type != "BASALT LEDGER ED25519 SEED" || len(blk.Bytes) != ed25519.SeedSize {
			return nil, fmt.Errorf("%s: not a ledger export key", path)
		}
		priv := ed25519.NewKeyFromSeed(blk.Bytes)
		return &Key{Private: priv, Public: priv.Public().(ed25519.PublicKey), Kind: "development"}, nil
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
	return &Key{Private: priv, Public: pub, Kind: "development"}, nil
}

// ReadPublic reads a public key file written by LoadOrCreate.
func ReadPublic(path string) (ed25519.PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "BASALT LEDGER ED25519 PUBLIC KEY" || len(blk.Bytes) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%s: not a ledger public key", path)
	}
	return ed25519.PublicKey(blk.Bytes), nil
}

// payload is the signed form: the document without its signature.
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
	e.PublicKey = base64.StdEncoding.EncodeToString(k.Public)
	p, err := payload(*e)
	if err != nil {
		return err
	}
	e.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(k.Private, p))
	return nil
}

// Verify checks the signature (against trusted, or the embedded key when
// trusted is nil, which only proves integrity, not origin) and every
// record's own hash, and that consecutive records in the export are
// chained where their sequence numbers are consecutive.
func Verify(e Export, trusted ed25519.PublicKey) (string, error) {
	if e.Format != Format {
		return "", fmt.Errorf("unknown format %q", e.Format)
	}
	emb, err := base64.StdEncoding.DecodeString(e.PublicKey)
	if err != nil || len(emb) != ed25519.PublicKeySize {
		return "", errors.New("bad embedded public key")
	}
	key := ed25519.PublicKey(emb)
	origin := "integrity only: signed by the key embedded in the export (pass the host's public key to check origin)"
	if trusted != nil {
		if !key.Equal(trusted) {
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
	if !ed25519.Verify(key, p, sig) {
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
