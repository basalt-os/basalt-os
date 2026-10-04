// Package tpmsim is a TPM 2.0 stand-in for tests: it understands exactly
// the commands package tpm sends (CreatePrimary of an ECC signing key
// under the owner hierarchy, Sign with ECDSA SHA-256, FlushContext),
// checks their encoding strictly, and answers like a TPM, backed by a
// software ECDSA key derived from the owner seed and the template's
// unique field (so the same template gives the same key, as a primary key
// does). It is not a TPM and protects nothing; the lab runs the real one.
package tpmsim

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"sync"
)

// Sim is one simulated TPM. Each Write takes one command; the next Read
// returns its response.
type Sim struct {
	// Seed is the owner seed (change it to simulate a cleared TPM).
	Seed []byte
	// Retries makes the next commands answer TPM_RC_RETRY first.
	Retries int
	// Err is the first encoding problem seen in a command.
	Err error

	mu   sync.Mutex
	rsp  []byte
	keys map[uint32]*ecdsa.PrivateKey
	next uint32
}

// New returns a simulator with a fixed owner seed.
func New() *Sim {
	return &Sim{Seed: []byte("tpmsim owner seed"), keys: map[uint32]*ecdsa.PrivateKey{}, next: 0x80000000}
}

// Read returns the pending response.
func (s *Sim) Read(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rsp == nil {
		return 0, errors.New("tpmsim: no response pending")
	}
	n := copy(b, s.rsp)
	s.rsp = nil
	return n, nil
}

// Close does nothing (the simulator outlives connections, like a TPM).
func (s *Sim) Close() error { return nil }

type dec struct {
	b   []byte
	err error
}

func (d *dec) take(n int) []byte {
	if d.err != nil {
		return nil
	}
	if n < 0 || len(d.b) < n {
		d.err = errors.New("truncated")
		return nil
	}
	v := d.b[:n]
	d.b = d.b[n:]
	return v
}
func (d *dec) u8() byte {
	if v := d.take(1); v != nil {
		return v[0]
	}
	return 0
}
func (d *dec) u16() uint16 {
	if v := d.take(2); v != nil {
		return binary.BigEndian.Uint16(v)
	}
	return 0
}
func (d *dec) u32() uint32 {
	if v := d.take(4); v != nil {
		return binary.BigEndian.Uint32(v)
	}
	return 0
}
func (d *dec) b2() []byte { return d.take(int(d.u16())) }

func b2(b []byte) []byte { return append(binary.BigEndian.AppendUint16(nil, uint16(len(b))), b...) }

func (s *Sim) reply(tag uint16, rc uint32, body []byte) {
	r := binary.BigEndian.AppendUint16(nil, tag)
	r = binary.BigEndian.AppendUint32(r, uint32(10+len(body)))
	r = binary.BigEndian.AppendUint32(r, rc)
	s.rsp = append(r, body...)
}

func (s *Sim) bad(format string, a ...any) {
	if s.Err == nil {
		s.Err = fmt.Errorf(format, a...)
	}
	s.reply(0x8001, 0x101, nil) // TPM_RC_FAILURE
}

// Write takes one command.
func (s *Sim) Write(cmd []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := &dec{b: cmd}
	tag, size, cc := d.u16(), d.u32(), d.u32()
	if int(size) != len(cmd) {
		s.bad("command size %d, actual %d", size, len(cmd))
		return len(cmd), nil
	}
	if s.Retries > 0 {
		s.Retries--
		s.reply(0x8001, 0x922, nil) // TPM_RC_RETRY
		return len(cmd), nil
	}
	auth := func() bool {
		if tag != 0x8002 {
			s.bad("cc 0x%x: tag 0x%x, want sessions", cc, tag)
			return false
		}
		a := &dec{b: d.take(int(d.u32()))}
		if a.u32() != 0x40000009 || len(a.b2()) != 0 || a.u8() != 0 || len(a.b2()) != 0 || len(a.b) != 0 || a.err != nil {
			s.bad("cc 0x%x: bad authorization area", cc)
			return false
		}
		return true
	}
	switch cc {
	case 0x131: // CreatePrimary
		if h := d.u32(); h != 0x40000001 {
			s.bad("CreatePrimary under 0x%x, want the owner hierarchy", h)
			break
		}
		if !auth() {
			break
		}
		sens := &dec{b: d.b2()}
		if len(sens.b2()) != 0 || len(sens.b2()) != 0 || len(sens.b) != 0 {
			s.bad("inSensitive is not empty")
			break
		}
		t := &dec{b: d.b2()}
		head := t.take(8)
		if t.u16() != 0 || t.u16() != 0x10 || t.u16() != 0x18 || t.u16() != 0x0B || t.u16() != 0x03 || t.u16() != 0x10 {
			s.bad("template is not an ECDSA P-256 SHA-256 key without symmetric or KDF")
			break
		}
		if binary.BigEndian.Uint16(head[0:2]) != 0x23 || binary.BigEndian.Uint16(head[2:4]) != 0x0B {
			s.bad("template type or name algorithm")
			break
		}
		attrs := binary.BigEndian.Uint32(head[4:8])
		if attrs&0x00040000 == 0 || attrs&0x00010000 != 0 || attrs&0x00020000 != 0 || attrs&0x2 == 0 {
			s.bad("attributes 0x%x: want sign and fixedTPM, not restricted, not decrypt", attrs)
			break
		}
		unique := t.b2()
		t.b2()
		if len(t.b) != 0 || t.err != nil || len(d.b2()) != 0 || d.u32() != 0 || len(d.b) != 0 || d.err != nil {
			s.bad("CreatePrimary trailing or missing fields")
			break
		}
		h := sha256.Sum256(append(append([]byte{}, s.Seed...), unique...))
		k := new(big.Int).SetBytes(h[:])
		k.Mod(k, new(big.Int).Sub(elliptic.P256().Params().N, big.NewInt(1)))
		k.Add(k, big.NewInt(1))
		priv := &ecdsa.PrivateKey{D: k}
		priv.PublicKey.Curve = elliptic.P256()
		priv.PublicKey.X, priv.PublicKey.Y = elliptic.P256().ScalarBaseMult(k.FillBytes(make([]byte, 32)))
		s.next++
		s.keys[s.next] = priv
		pub := append([]byte{}, head...)
		pub = append(pub, b2(nil)...)
		for _, w := range []uint16{0x10, 0x18, 0x0B, 0x03, 0x10} {
			pub = binary.BigEndian.AppendUint16(pub, w)
		}
		pub = append(pub, b2(priv.X.FillBytes(make([]byte, 32)))...)
		pub = append(pub, b2(priv.Y.FillBytes(make([]byte, 32)))...)
		params := b2(pub)
		params = append(params, b2([]byte("creation data"))...)
		body := binary.BigEndian.AppendUint32(nil, s.next)
		body = binary.BigEndian.AppendUint32(body, uint32(len(params)))
		body = append(body, params...)
		body = append(body, 0, 0, 1, 0, 0)
		s.reply(0x8002, 0, body)
	case 0x15D: // Sign
		h := d.u32()
		if !auth() {
			break
		}
		priv := s.keys[h]
		if priv == nil {
			s.reply(0x8001, 0x18b, nil) // TPM_RC_HANDLE
			break
		}
		digest := d.b2()
		if len(digest) != 32 || d.u16() != 0x18 || d.u16() != 0x0B || d.u16() != 0x8024 || d.u32() != 0x40000007 || len(d.b2()) != 0 || len(d.b) != 0 {
			s.bad("Sign parameters")
			break
		}
		r, ss, err := ecdsa.Sign(rand.Reader, priv, digest)
		if err != nil {
			s.bad("sign: %v", err)
			break
		}
		sig := binary.BigEndian.AppendUint16(nil, 0x18)
		sig = binary.BigEndian.AppendUint16(sig, 0x0B)
		sig = append(sig, b2(r.Bytes())...)
		sig = append(sig, b2(ss.Bytes())...)
		body := binary.BigEndian.AppendUint32(nil, uint32(len(sig)))
		body = append(body, sig...)
		body = append(body, 0, 0, 1, 0, 0)
		s.reply(0x8002, 0, body)
	case 0x165: // FlushContext
		if tag != 0x8001 {
			s.bad("FlushContext with sessions")
			break
		}
		delete(s.keys, d.u32())
		s.reply(0x8001, 0, nil)
	default:
		s.reply(0x8001, 0x143, nil) // TPM_RC_COMMAND_CODE
	}
	return len(cmd), nil
}
