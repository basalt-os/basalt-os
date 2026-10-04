// Package pgpkey computes the fingerprint of an OpenPGP public key, so a
// repository key shipped with the installer can be checked against its
// pinned fingerprint without a cryptography library.
package pgpkey

import (
	"bufio"
	"bytes"
	"crypto/sha1" //nolint:gosec // the OpenPGP v4 fingerprint is defined as SHA-1
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Fingerprint returns the upper-case hex fingerprint of the primary key in
// an ASCII-armored OpenPGP public key block (version 4 keys).
func Fingerprint(armored []byte) (string, error) {
	body, err := dearmor(armored)
	if err != nil {
		return "", err
	}
	tag, packet, err := firstPacket(body)
	if err != nil {
		return "", err
	}
	if tag != 6 {
		return "", fmt.Errorf("first packet has tag %d, not a public key (6)", tag)
	}
	if len(packet) == 0 || packet[0] != 4 {
		return "", errors.New("only version 4 keys are supported")
	}
	h := sha1.New() //nolint:gosec
	h.Write([]byte{0x99, byte(len(packet) >> 8), byte(len(packet))})
	h.Write(packet)
	return strings.ToUpper(fmt.Sprintf("%x", h.Sum(nil))), nil
}

func dearmor(armored []byte) ([]byte, error) {
	sc := bufio.NewScanner(bytes.NewReader(armored))
	var b64 strings.Builder
	inBlock, inBody := false, false
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case strings.HasPrefix(line, "-----BEGIN PGP PUBLIC KEY BLOCK-----"):
			inBlock = true
		case strings.HasPrefix(line, "-----END PGP PUBLIC KEY BLOCK-----"):
			inBlock = false
		case !inBlock:
		case !inBody:
			// Armor headers end at the first empty line.
			if strings.TrimSpace(line) == "" {
				inBody = true
			}
		case strings.HasPrefix(line, "="):
			// CRC-24 checksum line.
		default:
			b64.WriteString(strings.TrimSpace(line))
		}
	}
	if b64.Len() == 0 {
		return nil, errors.New("no armored public key block")
	}
	return base64.StdEncoding.DecodeString(b64.String())
}

// firstPacket parses the header of the first packet (old and new formats).
func firstPacket(b []byte) (tag int, body []byte, err error) {
	if len(b) < 2 || b[0]&0x80 == 0 {
		return 0, nil, errors.New("not an OpenPGP packet")
	}
	var length, hdr int
	if b[0]&0x40 != 0 { // new format
		tag = int(b[0] & 0x3f)
		switch l := int(b[1]); {
		case l < 192:
			length, hdr = l, 2
		case l < 224:
			if len(b) < 3 {
				return 0, nil, errors.New("short packet")
			}
			length, hdr = (l-192)<<8+int(b[2])+192, 3
		case l == 255:
			if len(b) < 6 {
				return 0, nil, errors.New("short packet")
			}
			length, hdr = int(b[2])<<24|int(b[3])<<16|int(b[4])<<8|int(b[5]), 6
		default:
			return 0, nil, errors.New("partial body lengths are not valid for a key packet")
		}
	} else { // old format
		tag = int(b[0]>>2) & 0x0f
		switch b[0] & 0x03 {
		case 0:
			length, hdr = int(b[1]), 2
		case 1:
			if len(b) < 3 {
				return 0, nil, errors.New("short packet")
			}
			length, hdr = int(b[1])<<8|int(b[2]), 3
		case 2:
			if len(b) < 5 {
				return 0, nil, errors.New("short packet")
			}
			length, hdr = int(b[1])<<24|int(b[2])<<16|int(b[3])<<8|int(b[4]), 5
		default:
			return 0, nil, errors.New("indeterminate length")
		}
	}
	if hdr+length > len(b) {
		return 0, nil, errors.New("truncated packet")
	}
	return tag, b[hdr : hdr+length], nil
}
