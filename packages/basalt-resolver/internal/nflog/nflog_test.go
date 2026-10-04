package nflog

import (
	"encoding/binary"
	"testing"
)

func ipv4TCP(dst [4]byte, dport uint16) []byte {
	b := make([]byte, 40)
	b[0] = 0x45
	b[9] = 6
	copy(b[12:], []byte{10, 86, 0, 10})
	copy(b[16:], dst[:])
	binary.BigEndian.PutUint16(b[20:], 40000)
	binary.BigEndian.PutUint16(b[22:], dport)
	return b
}

func TestParsePacketMessage(t *testing.T) {
	body := []byte{2, 0, 0, 47} // nfgenmsg: AF_INET, version, res_id
	body = append(body, attr(attrPrefix, []byte("basalt-egress s_abc drop\x00"))...)
	body = append(body, attr(attrPayload, ipv4TCP([4]byte{1, 1, 1, 1}, 443))...)
	p, ok := ParsePacketMessage(body)
	if !ok {
		t.Fatal("not parsed")
	}
	if p.Prefix != "basalt-egress s_abc drop" || p.Proto != "tcp" || p.Dst.String() != "1.1.1.1" || p.DPort != 443 || p.Family != 4 {
		t.Fatalf("%+v", p)
	}
}

func TestParseIPv6UDP(t *testing.T) {
	b := make([]byte, 48)
	b[0] = 0x60
	b[6] = 17
	b[24], b[25], b[39] = 0x20, 0x01, 1
	binary.BigEndian.PutUint16(b[42:], 53)
	var p Packet
	if !ParseIP(b, &p) || p.Proto != "udp" || p.DPort != 53 || p.Dst.String() != "2001::1" {
		t.Fatalf("%+v", p)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	var p Packet
	for _, b := range [][]byte{nil, {0x45}, {0x46, 0, 0}, make([]byte, 30)} {
		if ParseIP(b, &p) && b != nil && len(b) < 20 {
			t.Errorf("accepted %v", b)
		}
	}
	if _, ok := ParsePacketMessage([]byte{2, 0, 0, 47, 3, 0}); ok {
		t.Error("accepted a broken attribute")
	}
}
