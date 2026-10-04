package knowledge

// A small, strict OpenPGP (RFC 4880) reader: just enough to verify a
// detached signature over a knowledge manifest with the Go standard
// library, without gpg and without a third-party module.
//
// Accepted: version 4 RSA keys and signatures, SHA-256, SHA-384 or SHA-512,
// armored or binary. A certificate is trusted only through pinned
// fingerprints: the primary key's and the signing subkey's (v4
// fingerprints, SHA-1 over the key packet as RFC 4880 12.2 defines them).
// The subkey must be bound to the primary by a valid subkey binding
// signature that grants signing and carries a valid primary key binding
// signature made by the subkey itself (the "back signature" that stops a
// subkey from being attached to someone else's primary), must not be
// revoked, and the data signature must have been made while the subkey was
// valid. Anything else is refused.

import (
	"bufio"
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"math/big"
	"strings"
	"time"
)

const (
	tagSignature = 2
	tagPublicKey = 6
	tagUserID    = 13
	tagSubkey    = 14

	sigBinary        = 0x00
	sigSubkeyBinding = 0x18
	sigPrimaryBind   = 0x19
	sigKeyRevoke     = 0x20
	sigSubkeyRevoke  = 0x28

	algoRSA = 1

	spCreated     = 2
	spSigExpires  = 3
	spKeyExpires  = 9
	spIssuerKeyID = 16
	spKeyFlags    = 27
	spEmbedded    = 32
	spIssuerFpr   = 33

	flagSign = 0x02
)

type pgpPacket struct {
	tag  int
	body []byte
}

// pgpKey is a v4 RSA public key (primary or subkey).
type pgpKey struct {
	body    []byte
	created time.Time
	pub     *rsa.PublicKey
	fpr     string // upper-case hex, 40 characters
}

type pgpSig struct {
	version, typ, pkAlgo, hashAlgo int
	hashedPart                     []byte // version .. end of the hashed subpackets
	hashed, unhashed               []subpacket
	left16                         []byte
	sig                            []byte
}

type subpacket struct {
	typ      int
	critical bool
	data     []byte
}

// dearmor returns the binary content of an ASCII-armored block, or the
// input when it is not armored.
func dearmor(in []byte) ([]byte, error) {
	s := bytes.TrimSpace(in)
	if !bytes.HasPrefix(s, []byte("-----BEGIN PGP ")) {
		return in, nil
	}
	sc := bufio.NewScanner(bytes.NewReader(s))
	sc.Buffer(make([]byte, 1<<16), 1<<22)
	sc.Scan() // BEGIN line
	inHeaders := true
	var b64 strings.Builder
	var crc string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "-----END PGP ") {
			data, err := base64.StdEncoding.DecodeString(b64.String())
			if err != nil {
				return nil, fmt.Errorf("armor: %v", err)
			}
			if crc != "" {
				want, err := base64.StdEncoding.DecodeString(crc)
				if err != nil || len(want) != 3 {
					return nil, errors.New("armor: bad checksum line")
				}
				c := crc24(data)
				if byte(c>>16) != want[0] || byte(c>>8) != want[1] || byte(c) != want[2] {
					return nil, errors.New("armor: checksum mismatch")
				}
			}
			return data, nil
		}
		if inHeaders {
			if line == "" {
				inHeaders = false
			} else if !strings.Contains(line, ": ") {
				inHeaders = false
				b64.WriteString(line)
			}
			continue
		}
		if strings.HasPrefix(line, "=") && len(line) == 5 {
			crc = line[1:]
			continue
		}
		b64.WriteString(line)
	}
	return nil, errors.New("armor: no END line")
}

func crc24(data []byte) uint32 {
	crc := uint32(0xB704CE)
	for _, b := range data {
		crc ^= uint32(b) << 16
		for i := 0; i < 8; i++ {
			crc <<= 1
			if crc&0x1000000 != 0 {
				crc ^= 0x1864CFB
			}
		}
	}
	return crc & 0xFFFFFF
}

// readPackets splits binary OpenPGP data into packets (old and new
// formats; partial and indeterminate lengths are refused).
func readPackets(b []byte) ([]pgpPacket, error) {
	var out []pgpPacket
	for len(b) > 0 {
		h := b[0]
		if h&0x80 == 0 {
			return nil, errors.New("openpgp: not a packet header")
		}
		var tag, n, hl int
		if h&0x40 == 0 { // old format
			tag = int(h>>2) & 0x0F
			switch h & 3 {
			case 0:
				if len(b) < 2 {
					return nil, errors.New("openpgp: short header")
				}
				n, hl = int(b[1]), 2
			case 1:
				if len(b) < 3 {
					return nil, errors.New("openpgp: short header")
				}
				n, hl = int(binary.BigEndian.Uint16(b[1:3])), 3
			case 2:
				if len(b) < 5 {
					return nil, errors.New("openpgp: short header")
				}
				n, hl = int(binary.BigEndian.Uint32(b[1:5])), 5
			default:
				return nil, errors.New("openpgp: indeterminate length")
			}
		} else { // new format
			tag = int(h & 0x3F)
			if len(b) < 2 {
				return nil, errors.New("openpgp: short header")
			}
			o := int(b[1])
			switch {
			case o < 192:
				n, hl = o, 2
			case o < 224:
				if len(b) < 3 {
					return nil, errors.New("openpgp: short header")
				}
				n, hl = ((o-192)<<8)+int(b[2])+192, 3
			case o == 255:
				if len(b) < 6 {
					return nil, errors.New("openpgp: short header")
				}
				n, hl = int(binary.BigEndian.Uint32(b[2:6])), 6
			default:
				return nil, errors.New("openpgp: partial lengths are not accepted")
			}
		}
		if n < 0 || hl+n > len(b) {
			return nil, errors.New("openpgp: truncated packet")
		}
		out = append(out, pgpPacket{tag: tag, body: b[hl : hl+n]})
		b = b[hl+n:]
	}
	return out, nil
}

func readMPI(b []byte) (*big.Int, []byte, error) {
	if len(b) < 2 {
		return nil, nil, errors.New("openpgp: short MPI")
	}
	bits := int(binary.BigEndian.Uint16(b))
	n := (bits + 7) / 8
	if len(b) < 2+n {
		return nil, nil, errors.New("openpgp: short MPI")
	}
	return new(big.Int).SetBytes(b[2 : 2+n]), b[2+n:], nil
}

func parseKey(body []byte) (*pgpKey, error) {
	if len(body) < 6 || body[0] != 4 {
		return nil, errors.New("openpgp: only version 4 keys are accepted")
	}
	if body[5] != algoRSA {
		return nil, fmt.Errorf("openpgp: key algorithm %d (only RSA is accepted)", body[5])
	}
	n, rest, err := readMPI(body[6:])
	if err != nil {
		return nil, err
	}
	e, rest, err := readMPI(rest)
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 {
		return nil, errors.New("openpgp: trailing bytes in a key packet")
	}
	if n.BitLen() < 2048 || !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 {
		return nil, errors.New("openpgp: RSA key too small or bad exponent")
	}
	h := sha1.New()
	h.Write([]byte{0x99, byte(len(body) >> 8), byte(len(body))})
	h.Write(body)
	return &pgpKey{body: body, created: time.Unix(int64(binary.BigEndian.Uint32(body[1:5])), 0).UTC(),
		pub: &rsa.PublicKey{N: n, E: int(e.Int64())}, fpr: strings.ToUpper(hex.EncodeToString(h.Sum(nil)))}, nil
}

func parseSubpackets(b []byte) ([]subpacket, error) {
	var out []subpacket
	for len(b) > 0 {
		var n, hl int
		o := int(b[0])
		switch {
		case o < 192:
			n, hl = o, 1
		case o < 255:
			if len(b) < 2 {
				return nil, errors.New("openpgp: short subpacket")
			}
			n, hl = ((o-192)<<8)+int(b[1])+192, 2
		default:
			if len(b) < 5 {
				return nil, errors.New("openpgp: short subpacket")
			}
			n, hl = int(binary.BigEndian.Uint32(b[1:5])), 5
		}
		if n < 1 || hl+n > len(b) {
			return nil, errors.New("openpgp: bad subpacket length")
		}
		t := b[hl]
		out = append(out, subpacket{typ: int(t & 0x7F), critical: t&0x80 != 0, data: b[hl+1 : hl+n]})
		b = b[hl+n:]
	}
	return out, nil
}

func parseSig(body []byte) (*pgpSig, error) {
	if len(body) < 6 || body[0] != 4 {
		return nil, errors.New("openpgp: only version 4 signatures are accepted")
	}
	s := &pgpSig{version: 4, typ: int(body[1]), pkAlgo: int(body[2]), hashAlgo: int(body[3])}
	hl := int(binary.BigEndian.Uint16(body[4:6]))
	if len(body) < 6+hl+2 {
		return nil, errors.New("openpgp: truncated signature")
	}
	s.hashedPart = body[:6+hl]
	var err error
	if s.hashed, err = parseSubpackets(body[6 : 6+hl]); err != nil {
		return nil, err
	}
	rest := body[6+hl:]
	ul := int(binary.BigEndian.Uint16(rest[:2]))
	if len(rest) < 2+ul+2 {
		return nil, errors.New("openpgp: truncated signature")
	}
	if s.unhashed, err = parseSubpackets(rest[2 : 2+ul]); err != nil {
		return nil, err
	}
	rest = rest[2+ul:]
	s.left16 = rest[:2]
	m, rest, err := readMPI(rest[2:])
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 {
		return nil, errors.New("openpgp: trailing bytes in a signature")
	}
	s.sig = m.Bytes()
	if s.pkAlgo != algoRSA {
		return nil, fmt.Errorf("openpgp: signature algorithm %d (only RSA is accepted)", s.pkAlgo)
	}
	known := map[int]bool{spCreated: true, spSigExpires: true, spKeyExpires: true, spIssuerKeyID: true,
		spKeyFlags: true, spEmbedded: true, spIssuerFpr: true}
	for _, sp := range s.hashed {
		if sp.critical && !known[sp.typ] {
			return nil, fmt.Errorf("openpgp: unknown critical subpacket %d", sp.typ)
		}
	}
	return s, nil
}

func (s *pgpSig) hashed1(typ int) []byte {
	for _, sp := range s.hashed {
		if sp.typ == typ {
			return sp.data
		}
	}
	return nil
}

func (s *pgpSig) any1(typ int) []byte {
	if d := s.hashed1(typ); d != nil {
		return d
	}
	for _, sp := range s.unhashed {
		if sp.typ == typ {
			return sp.data
		}
	}
	return nil
}

// created is the signature's creation time (a hashed subpacket, required).
func (s *pgpSig) created() (time.Time, error) {
	d := s.hashed1(spCreated)
	if len(d) != 4 {
		return time.Time{}, errors.New("openpgp: signature without a hashed creation time")
	}
	return time.Unix(int64(binary.BigEndian.Uint32(d)), 0).UTC(), nil
}

func (s *pgpSig) newHash() (hash.Hash, crypto.Hash, error) {
	switch s.hashAlgo {
	case 8:
		return sha256.New(), crypto.SHA256, nil
	case 9:
		return sha512.New384(), crypto.SHA384, nil
	case 10:
		return sha512.New(), crypto.SHA512, nil
	}
	return nil, 0, fmt.Errorf("openpgp: hash algorithm %d (SHA-256, SHA-384 or SHA-512 only)", s.hashAlgo)
}

// verify checks the signature over prefix (the signed data) with key.
func (s *pgpSig) verify(key *pgpKey, prefix ...[]byte) error {
	h, ch, err := s.newHash()
	if err != nil {
		return err
	}
	for _, p := range prefix {
		h.Write(p)
	}
	h.Write(s.hashedPart)
	var tr [6]byte
	tr[0], tr[1] = 4, 0xFF
	binary.BigEndian.PutUint32(tr[2:], uint32(len(s.hashedPart)))
	h.Write(tr[:])
	digest := h.Sum(nil)
	if digest[0] != s.left16[0] || digest[1] != s.left16[1] {
		return errors.New("openpgp: signature does not match (digest prefix)")
	}
	k := key.pub.Size()
	if len(s.sig) > k {
		return errors.New("openpgp: signature longer than the key")
	}
	sig := make([]byte, k)
	copy(sig[k-len(s.sig):], s.sig)
	if err := rsa.VerifyPKCS1v15(key.pub, ch, digest, sig); err != nil {
		return errors.New("openpgp: bad signature")
	}
	return nil
}

func keyPrefix(k *pgpKey) []byte {
	return append([]byte{0x99, byte(len(k.body) >> 8), byte(len(k.body))}, k.body...)
}

// issuedBy reports whether the signature names key as its issuer (issuer
// fingerprint subpacket, else the issuer key id).
func (s *pgpSig) issuedBy(k *pgpKey) bool {
	if d := s.any1(spIssuerFpr); len(d) == 21 && d[0] == 4 {
		return strings.ToUpper(hex.EncodeToString(d[1:])) == k.fpr
	}
	if d := s.any1(spIssuerKeyID); len(d) == 8 {
		return strings.ToUpper(hex.EncodeToString(d)) == k.fpr[24:]
	}
	return false
}

// signingKey is a subkey checked against its primary.
type signingKey struct {
	key     *pgpKey
	primary *pgpKey
	expires time.Time // zero: no expiration
}

// TrustAnchor pins the certificate a knowledge signature must come from.
type TrustAnchor struct {
	Primary string // v4 fingerprint of the primary key (40 hex)
	Signer  string // v4 fingerprint of the signing subkey (40 hex)
}

func normFpr(s string) string {
	return strings.ToUpper(strings.NewReplacer(" ", "", ":", "").Replace(strings.TrimSpace(s)))
}

// loadSigningKey finds the pinned subkey in a certificate (armored or
// binary) and checks its binding to the pinned primary.
func loadSigningKey(cert []byte, t TrustAnchor) (*signingKey, error) {
	data, err := dearmor(cert)
	if err != nil {
		return nil, err
	}
	pkts, err := readPackets(data)
	if err != nil {
		return nil, err
	}
	wantP, wantS := normFpr(t.Primary), normFpr(t.Signer)
	var primary, sub *pgpKey
	var subSigs, primarySigs []*pgpSig
	// area: what the following signatures are about: "primary" (directly
	// after the primary key: revocations, direct-key signatures), "uid"
	// (certifications), "sub" (our subkey), "other" (another subkey).
	area := ""
	for _, p := range pkts {
		switch p.tag {
		case tagPublicKey:
			if primary != nil {
				return nil, errors.New("openpgp: more than one certificate in the key file")
			}
			if primary, err = parseKey(p.body); err != nil {
				return nil, err
			}
			area = "primary"
		case tagSubkey:
			area = "other"
			if k, err := parseKey(p.body); err == nil && k.fpr == wantS {
				sub, area = k, "sub"
			}
		case tagUserID:
			area = "uid"
		case tagSignature:
			s, err := parseSig(p.body)
			if err != nil {
				continue // a signature we cannot read never makes a key valid
			}
			switch area {
			case "primary":
				primarySigs = append(primarySigs, s)
			case "sub":
				subSigs = append(subSigs, s)
			}
		default:
			area = "other" // user attributes and anything else
		}
	}
	if primary == nil || primary.fpr != wantP {
		return nil, fmt.Errorf("openpgp: the key file's primary key is not the pinned %s", wantP)
	}
	if sub == nil {
		return nil, fmt.Errorf("openpgp: the pinned signing subkey %s is not in the key file", wantS)
	}
	for _, s := range primarySigs {
		if s.typ == sigKeyRevoke && s.issuedBy(primary) && s.verify(primary, keyPrefix(primary)) == nil {
			return nil, errors.New("openpgp: the primary key is revoked")
		}
	}
	var bound *signingKey
	for _, s := range subSigs {
		switch s.typ {
		case sigSubkeyRevoke:
			if s.issuedBy(primary) && s.verify(primary, keyPrefix(primary), keyPrefix(sub)) == nil {
				return nil, errors.New("openpgp: the signing subkey is revoked")
			}
		case sigSubkeyBinding:
			if bound != nil || !s.issuedBy(primary) || s.verify(primary, keyPrefix(primary), keyPrefix(sub)) != nil {
				continue
			}
			if f := s.hashed1(spKeyFlags); len(f) == 0 || f[0]&flagSign == 0 {
				continue
			}
			emb := s.any1(spEmbedded)
			if emb == nil {
				continue
			}
			back, err := parseSig(emb)
			if err != nil || back.typ != sigPrimaryBind || back.verify(sub, keyPrefix(primary), keyPrefix(sub)) != nil {
				continue
			}
			sk := &signingKey{key: sub, primary: primary}
			if d := s.hashed1(spKeyExpires); len(d) == 4 {
				if v := binary.BigEndian.Uint32(d); v != 0 {
					sk.expires = sub.created.Add(time.Duration(v) * time.Second)
				}
			}
			bound = sk
		}
	}
	if bound == nil {
		return nil, errors.New("openpgp: no valid binding (with a back signature and the signing flag) of the subkey to the primary key")
	}
	return bound, nil
}

// verifyDetached checks a detached binary-document signature over data
// made by the signing key; returns the signature's creation time.
func (k *signingKey) verifyDetached(sigData, data []byte, now time.Time) (time.Time, error) {
	raw, err := dearmor(sigData)
	if err != nil {
		return time.Time{}, err
	}
	pkts, err := readPackets(raw)
	if err != nil {
		return time.Time{}, err
	}
	if len(pkts) != 1 || pkts[0].tag != tagSignature {
		return time.Time{}, errors.New("openpgp: the signature file must hold exactly one signature")
	}
	s, err := parseSig(pkts[0].body)
	if err != nil {
		return time.Time{}, err
	}
	if s.typ != sigBinary {
		return time.Time{}, fmt.Errorf("openpgp: signature type 0x%02x (a binary document signature is required)", s.typ)
	}
	if !s.issuedBy(k.key) {
		return time.Time{}, errors.New("openpgp: signed by another key")
	}
	created, err := s.created()
	if err != nil {
		return time.Time{}, err
	}
	if err := s.verify(k.key, data); err != nil {
		return time.Time{}, err
	}
	if created.Before(k.key.created) {
		return time.Time{}, errors.New("openpgp: signature older than the signing key")
	}
	if !k.expires.IsZero() && created.After(k.expires) {
		return time.Time{}, errors.New("openpgp: signature made after the signing key expired")
	}
	if created.After(now.Add(24 * time.Hour)) {
		return time.Time{}, errors.New("openpgp: signature from the future")
	}
	if d := s.hashed1(spSigExpires); len(d) == 4 {
		if v := binary.BigEndian.Uint32(d); v != 0 && now.After(created.Add(time.Duration(v)*time.Second)) {
			return time.Time{}, errors.New("openpgp: signature expired")
		}
	}
	return created, nil
}
