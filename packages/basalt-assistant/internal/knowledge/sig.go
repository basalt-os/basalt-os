package knowledge

// Signed knowledge (ADR 0007: "signed basalt-knowledge packages"): the
// manifest of an index is signed with the OpenBasalt knowledge subkey, a
// detached OpenPGP signature in manifest.json.sig; the manifest addresses
// cases.jsonl and index.bin by SHA-256, so the signature covers the whole
// index. The assistant checks it at load with the standard library
// (openpgp.go) against the OpenBasalt release certificate, trusting only
// the pinned fingerprints below (or the ones the administrator
// configures). Any failure: the index is not used and the rules answer.

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// OpenBasaltKnowledge is the default trust anchor: the OpenBasalt release
// key (primary 3601 7348 42BD 4E48 2D19 DE4A E4EE D5EC A395 B302) and its
// knowledge signing subkey. Both fingerprints are published with the key
// (https://obpkg.org/keys/openbasalt-release-key.asc); see
// docs/assistant.md for how anyone can check the link.
var OpenBasaltKnowledge = TrustAnchor{
	Primary: "3601734842BD4E482D19DE4AE4EED5ECA395B302",
	Signer:  "85D61430B70438680F6EB955E79E4020605A659A",
}

// DefaultKeyFile is the OpenBasalt release key (the public certificate
// with the knowledge subkey) as basalt-knowledge installs it, next to the
// indexes, with their SELinux type (the daemon reads nothing else under
// /etc/pki). It is the same file as basalt-release's RPM-GPG-KEY-basalt;
// the pinned fingerprints, not the file's location, are what is trusted.
const DefaultKeyFile = "/usr/share/basalt/knowledge/openbasalt-release-key.asc"

// SigFile is the detached signature of manifest.json in an index directory.
const SigFile = "manifest.json.sig"

// Signature describes a verified manifest signature.
type Signature struct {
	Signer  string    `json:"signer"` // fingerprint of the signing subkey
	Primary string    `json:"primary"`
	Created time.Time `json:"created"`
}

// Verifier checks knowledge signatures.
type Verifier struct {
	KeyFile string      // OpenPGP certificate (armored or binary)
	Trust   TrustAnchor // pinned fingerprints
	Now     func() time.Time
}

// VerifyManifest checks sig (manifest.json.sig) over mb (manifest.json).
func (v Verifier) VerifyManifest(mb, sig []byte) (*Signature, error) {
	cert, err := os.ReadFile(v.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("knowledge key: %v", err)
	}
	k, err := loadSigningKey(cert, v.Trust)
	if err != nil {
		return nil, fmt.Errorf("knowledge key %s: %v", v.KeyFile, err)
	}
	now := time.Now()
	if v.Now != nil {
		now = v.Now()
	}
	created, err := k.verifyDetached(sig, mb, now)
	if err != nil {
		return nil, fmt.Errorf("manifest signature: %v", err)
	}
	return &Signature{Signer: k.key.fpr, Primary: k.primary.fpr, Created: created}, nil
}

// OpenSigned opens an index only when its manifest carries a valid
// signature (manifest.json.sig) by the trusted key, and the manifest
// addresses every file of the index.
func OpenSigned(dir string, v Verifier) (*Index, error) {
	mb, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	sig, err := os.ReadFile(filepath.Join(dir, SigFile))
	if err != nil {
		return nil, fmt.Errorf("knowledge is not signed: %v", err)
	}
	s, err := v.VerifyManifest(mb, sig)
	if err != nil {
		return nil, err
	}
	ix, err := openManifest(dir, mb, true)
	if err != nil {
		return nil, err
	}
	ix.Signature = s
	return ix, nil
}
