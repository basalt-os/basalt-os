// Package tpm is the small TPM 2.0 client basalt-ledger needs to sign
// exports with a key the TPM holds: create a signing primary key under
// the owner hierarchy and sign a digest with it (ECDSA P-256, SHA-256).
// Nothing else; the standard library only.
//
// The key is a primary key derived from the TPM's owner seed and a fixed
// template (with a Basalt label in its unique field), so the same TPM
// always gives the same key and nothing has to be stored: the private
// half never leaves the TPM (fixedTPM, fixedParent, sensitiveDataOrigin).
// Clearing the TPM changes the owner seed and with it the key.
package tpm

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"syscall"
	"time"
)

// DefaultDevice is the kernel's TPM resource manager, which flushes the
// objects a connection created when it closes.
const DefaultDevice = "/dev/tpmrm0"

// TPM 2.0 constants (TCG TPM 2.0 Library, Part 2).
const (
	stNoSessions = 0x8001
	stSessions   = 0x8002
	stHashCheck  = 0x8024

	ccCreatePrimary = 0x00000131
	ccSign          = 0x0000015D
	ccFlushContext  = 0x00000165

	rhOwner = 0x40000001
	rhNull  = 0x40000007
	rsPW    = 0x40000009

	algECC    = 0x0023
	algSHA256 = 0x000B
	algNull   = 0x0010
	algECDSA  = 0x0018
	curveP256 = 0x0003

	// Object attributes: fixedTPM, fixedParent, sensitiveDataOrigin,
	// userWithAuth, noDA, sign. Not restricted, so it signs any digest
	// with a null ticket; not decrypt.
	attrFixedTPM            = 0x00000002
	attrFixedParent         = 0x00000010
	attrSensitiveDataOrigin = 0x00000020
	attrUserWithAuth        = 0x00000040
	attrNoDA                = 0x00000400
	attrSign                = 0x00040000
	keyAttributes           = attrFixedTPM | attrFixedParent | attrSensitiveDataOrigin | attrUserWithAuth | attrNoDA | attrSign

	// Warnings that ask the caller to try again.
	rcRetry   = 0x922
	rcYielded = 0x908
	rcTesting = 0x90A
)

// Label goes into the key template's unique field, so this key differs
// from any other primary key made from the same seed.
var Label = []byte("basalt-ledger export key v1")

// TPM is a connection to a TPM: each Write sends one command, each Read
// returns one response (the kernel's character device works this way).
type TPM struct {
	rw io.ReadWriter
	c  io.Closer
}

// OpenFunc opens the TPM device; tests replace it with a simulator.
var OpenFunc = openDevice

// openDevice opens the device for blocking I/O. os.OpenFile would put a
// pollable character device in non-blocking mode, where the kernel runs
// the command asynchronously and a read before it finishes returns 0
// bytes instead of waiting.
func openDevice(path string) (io.ReadWriteCloser, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

// Open opens a TPM device (DefaultDevice when path is empty).
func Open(path string) (*TPM, error) {
	if path == "" {
		path = DefaultDevice
	}
	f, err := OpenFunc(path)
	if err != nil {
		return nil, err
	}
	return &TPM{rw: f, c: f}, nil
}

// New wraps a transport (tests).
func New(rw io.ReadWriter) *TPM { return &TPM{rw: rw} }

// Close closes the device; the resource manager flushes what this
// connection created.
func (t *TPM) Close() error {
	if t.c == nil {
		return nil
	}
	return t.c.Close()
}

// RCError is a TPM response code other than success.
type RCError struct {
	Command uint32
	Code    uint32
}

func (e *RCError) Error() string {
	return fmt.Sprintf("TPM command 0x%x failed: response code 0x%x", e.Command, e.Code)
}

func (t *TPM) run(cc uint32, tag uint16, handles []uint32, auth bool, params []byte) ([]byte, error) {
	var body []byte
	for _, h := range handles {
		body = binary.BigEndian.AppendUint32(body, h)
	}
	if auth {
		// One password session with an empty password: TPMS_AUTH_COMMAND
		// {sessionHandle TPM_RS_PW, nonce empty, attributes 0, hmac empty}.
		a := binary.BigEndian.AppendUint32(nil, rsPW)
		a = append(a, 0, 0, 0, 0, 0)
		body = binary.BigEndian.AppendUint32(body, uint32(len(a)))
		body = append(body, a...)
	}
	body = append(body, params...)
	cmd := binary.BigEndian.AppendUint16(nil, tag)
	cmd = binary.BigEndian.AppendUint32(cmd, uint32(10+len(body)))
	cmd = binary.BigEndian.AppendUint32(cmd, cc)
	cmd = append(cmd, body...)
	for attempt := 0; ; attempt++ {
		if _, err := t.rw.Write(cmd); err != nil {
			return nil, fmt.Errorf("TPM write: %w", err)
		}
		buf := make([]byte, 4096)
		n, err := t.rw.Read(buf)
		if err != nil {
			return nil, fmt.Errorf("TPM read: %w", err)
		}
		rsp := buf[:n]
		if len(rsp) < 10 {
			return nil, errors.New("TPM response too short")
		}
		size := binary.BigEndian.Uint32(rsp[2:6])
		if int(size) != len(rsp) {
			return nil, fmt.Errorf("TPM response size %d, read %d", size, len(rsp))
		}
		rc := binary.BigEndian.Uint32(rsp[6:10])
		if (rc == rcRetry || rc == rcYielded || rc == rcTesting) && attempt < 20 {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if rc != 0 {
			return nil, &RCError{Command: cc, Code: rc}
		}
		return rsp[10:], nil
	}
}

// reader decodes TPM structures (big endian, TPM2B = u16 size + bytes).
type reader struct {
	b   []byte
	err error
}

func (r *reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || len(r.b) < n {
		r.err = errors.New("TPM response truncated")
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

func (r *reader) u16() uint16 {
	v := r.take(2)
	if v == nil {
		return 0
	}
	return binary.BigEndian.Uint16(v)
}

func (r *reader) u32() uint32 {
	v := r.take(4)
	if v == nil {
		return 0
	}
	return binary.BigEndian.Uint32(v)
}

func (r *reader) tpm2b() []byte { return r.take(int(r.u16())) }

func tpm2b(b []byte) []byte { return append(binary.BigEndian.AppendUint16(nil, uint16(len(b))), b...) }

// template is the TPMT_PUBLIC of the signing key.
func template(unique []byte) []byte {
	t := binary.BigEndian.AppendUint16(nil, algECC)
	t = binary.BigEndian.AppendUint16(t, algSHA256)
	t = binary.BigEndian.AppendUint32(t, keyAttributes)
	t = append(t, tpm2b(nil)...)                    // authPolicy
	t = binary.BigEndian.AppendUint16(t, algNull)   // symmetric
	t = binary.BigEndian.AppendUint16(t, algECDSA)  // scheme
	t = binary.BigEndian.AppendUint16(t, algSHA256) //   hash
	t = binary.BigEndian.AppendUint16(t, curveP256) // curveID
	t = binary.BigEndian.AppendUint16(t, algNull)   // kdf
	t = append(t, tpm2b(unique)...)                 // unique.x
	t = append(t, tpm2b(nil)...)                    // unique.y
	return t
}

// CreateSigningKey creates the signing primary key under the owner
// hierarchy (empty owner authorization) and returns its transient handle
// and public key.
func (t *TPM) CreateSigningKey() (uint32, *ecdsa.PublicKey, error) {
	sum := sha256.Sum256(Label)
	var p []byte
	p = append(p, tpm2b(append(tpm2b(nil), tpm2b(nil)...))...) // inSensitive: userAuth, data empty
	p = append(p, tpm2b(template(sum[:]))...)                  // inPublic
	p = append(p, tpm2b(nil)...)                               // outsideInfo
	p = binary.BigEndian.AppendUint32(p, 0)                    // creationPCR: none
	rsp, err := t.run(ccCreatePrimary, stSessions, []uint32{rhOwner}, true, p)
	if err != nil {
		return 0, nil, err
	}
	r := &reader{b: rsp}
	handle := r.u32()
	r.u32() // parameterSize
	pub := &reader{b: r.tpm2b()}
	if r.err != nil {
		return 0, nil, r.err
	}
	key, err := parseECCPublic(pub)
	if err != nil {
		_ = t.Flush(handle)
		return 0, nil, err
	}
	return handle, key, nil
}

func parseECCPublic(r *reader) (*ecdsa.PublicKey, error) {
	if typ := r.u16(); typ != algECC {
		return nil, fmt.Errorf("TPM key type 0x%x, want ECC", typ)
	}
	r.u16() // nameAlg
	r.u32() // attributes
	r.tpm2b()
	if sym := r.u16(); sym != algNull {
		r.u16()
		r.u16()
	}
	if scheme := r.u16(); scheme != algNull {
		r.u16()
	}
	curve := r.u16()
	if kdf := r.u16(); kdf != algNull {
		r.u16()
	}
	x, y := r.tpm2b(), r.tpm2b()
	if r.err != nil {
		return nil, r.err
	}
	if curve != curveP256 {
		return nil, fmt.Errorf("TPM key curve 0x%x, want P-256", curve)
	}
	pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
	if !pub.Curve.IsOnCurve(pub.X, pub.Y) {
		return nil, errors.New("TPM returned a point that is not on P-256")
	}
	return pub, nil
}

// Sign signs a SHA-256 digest with the key at handle and returns the
// signature in ASN.1 DER (what crypto/ecdsa.VerifyASN1 and OpenSSL take).
func (t *TPM) Sign(handle uint32, digest []byte) ([]byte, error) {
	if len(digest) != sha256.Size {
		return nil, errors.New("digest must be SHA-256")
	}
	var p []byte
	p = append(p, tpm2b(digest)...)
	p = binary.BigEndian.AppendUint16(p, algECDSA)
	p = binary.BigEndian.AppendUint16(p, algSHA256)
	// Null hash-check ticket: the key is not restricted.
	p = binary.BigEndian.AppendUint16(p, stHashCheck)
	p = binary.BigEndian.AppendUint32(p, rhNull)
	p = append(p, tpm2b(nil)...)
	rsp, err := t.run(ccSign, stSessions, []uint32{handle}, true, p)
	if err != nil {
		return nil, err
	}
	r := &reader{b: rsp}
	r.u32() // parameterSize
	alg := r.u16()
	r.u16() // hash
	rb, sb := r.tpm2b(), r.tpm2b()
	if r.err != nil {
		return nil, r.err
	}
	if alg != algECDSA {
		return nil, fmt.Errorf("TPM signature algorithm 0x%x, want ECDSA", alg)
	}
	return asn1.Marshal(struct{ R, S *big.Int }{new(big.Int).SetBytes(rb), new(big.Int).SetBytes(sb)})
}

// Flush removes a transient object from the TPM.
func (t *TPM) Flush(handle uint32) error {
	_, err := t.run(ccFlushContext, stNoSessions, nil, false, binary.BigEndian.AppendUint32(nil, handle))
	return err
}
