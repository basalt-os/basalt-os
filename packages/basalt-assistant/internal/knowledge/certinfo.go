package knowledge

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"strings"
	"unicode/utf8"
)

// CertKey is one primary key of an OpenPGP certificate as a person sees
// it: its fingerprint and the user ids bound after it.
type CertKey struct {
	Fingerprint string   `json:"fingerprint"` // 40 hex digits, upper case
	UserIDs     []string `json:"user_ids"`
}

// CertKeys lists the primary keys of a certificate (armored or binary),
// any public key algorithm, version 4 only. It reads, it does not verify:
// the software sources code pins the fingerprint, and rpm and dnf check
// the signatures made with the key.
func CertKeys(cert []byte) ([]CertKey, error) {
	bin, err := dearmor(cert)
	if err != nil {
		return nil, err
	}
	pkts, err := readPackets(bin)
	if err != nil {
		return nil, err
	}
	var out []CertKey
	for _, p := range pkts {
		switch p.tag {
		case tagPublicKey:
			if len(p.body) < 6 || p.body[0] != 4 {
				return nil, errors.New("openpgp: only version 4 keys are accepted")
			}
			h := sha1.New()
			h.Write([]byte{0x99, byte(len(p.body) >> 8), byte(len(p.body))})
			h.Write(p.body)
			out = append(out, CertKey{Fingerprint: strings.ToUpper(hex.EncodeToString(h.Sum(nil)))})
		case tagUserID:
			if len(out) > 0 && utf8.Valid(p.body) && len(p.body) <= 512 {
				out[len(out)-1].UserIDs = append(out[len(out)-1].UserIDs, string(p.body))
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("openpgp: no public key in the file")
	}
	return out, nil
}
