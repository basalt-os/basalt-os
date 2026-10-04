package dnsmsg

import (
	"encoding/binary"
	"net"
	"testing"
)

func TestQueryRoundTrip(t *testing.T) {
	b, err := NewQuery(0x1234, "API.Example.COM.", TypeA, true)
	if err != nil {
		t.Fatal(err)
	}
	q, err := ParseQuery(b)
	if err != nil {
		t.Fatal(err)
	}
	if q.Header.ID != 0x1234 || q.Question.Name != "api.example.com" || q.Question.Type != TypeA || !q.EDNS || q.UDPSize != ServerUDPSize {
		t.Fatalf("got %+v", q)
	}
}

func TestParseQueryRejects(t *testing.T) {
	good, _ := NewQuery(1, "a.example.com", TypeA, false)
	resp := append([]byte(nil), good...)
	resp[2] |= 0x80 // QR
	two := append([]byte(nil), good...)
	binary.BigEndian.PutUint16(two[4:], 2)
	loop := []byte{0, 1, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0xc0, 12, 0, 1, 0, 1}
	for name, b := range map[string][]byte{"short": good[:10], "response": resp, "two questions": two,
		"truncated name": good[:15], "pointer loop": loop} {
		if _, err := ParseQuery(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// answer builds an upstream-style answer with a compressed CNAME chain.
func answer(t *testing.T, id uint16, name string) []byte {
	t.Helper()
	q, _ := ParseQuery(mustQuery(t, id, name, TypeA))
	out, err := Build(q, []RR{
		{Name: name, Type: TypeCNAME, TTL: 300, Alias: "edge.cdn.example.net"},
		{Name: "edge.cdn.example.net", Type: TypeA, TTL: 60, IP: net.ParseIP("93.184.216.34")},
		{Name: "edge.cdn.example.net", Type: TypeA, TTL: 60, IP: net.ParseIP("10.0.0.5")},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mustQuery(t *testing.T, id uint16, name string, typ uint16) []byte {
	t.Helper()
	b, err := NewQuery(id, name, typ, false)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseAnswer(t *testing.T) {
	b := answer(t, 7, "www.example.com")
	a, err := ParseAnswer(b, Question{Name: "www.example.com", Type: TypeA, Class: ClassINET}, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Answers) != 3 || a.Answers[0].Alias != "edge.cdn.example.net" || !a.Answers[1].IP.Equal(net.ParseIP("93.184.216.34")) {
		t.Fatalf("answers %+v", a.Answers)
	}
	if _, err := ParseAnswer(b, Question{Name: "other.example.com", Type: TypeA, Class: ClassINET}, 7); err == nil {
		t.Fatal("answer for another question accepted")
	}
	if _, err := ParseAnswer(b, Question{Name: "www.example.com", Type: TypeA, Class: ClassINET}, 8); err == nil {
		t.Fatal("answer with another id accepted")
	}
}

func TestCompressedAnswer(t *testing.T) {
	// A hand-written answer using a compression pointer to the question name.
	q := mustQuery(t, 9, "x.example.org", TypeA)
	b := append([]byte(nil), q...)
	b[2] |= 0x80
	binary.BigEndian.PutUint16(b[6:], 1)
	b = append(b, 0xc0, 12) // name: pointer to offset 12
	b = binary.BigEndian.AppendUint16(b, TypeA)
	b = binary.BigEndian.AppendUint16(b, ClassINET)
	b = binary.BigEndian.AppendUint32(b, 30)
	b = binary.BigEndian.AppendUint16(b, 4)
	b = append(b, 1, 2, 3, 4)
	a, err := ParseAnswer(b, Question{Name: "x.example.org", Type: TypeA, Class: ClassINET}, 9)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Answers) != 1 || a.Answers[0].Name != "x.example.org" || a.Answers[0].IP.String() != "1.2.3.4" {
		t.Fatalf("%+v", a.Answers)
	}
}

func TestReplyAndTruncation(t *testing.T) {
	q, _ := ParseQuery(mustQuery(t, 3, "a.example.com", TypeA))
	r, err := Reply(q, RcodeRefused)
	if err != nil {
		t.Fatal(err)
	}
	h, _ := readHeader(r)
	if !h.Response() || h.Rcode() != RcodeRefused || h.ID != 3 {
		t.Fatalf("header %+v", h)
	}
	var rrs []RR
	for i := 0; i < 60; i++ {
		rrs = append(rrs, RR{Name: "a.example.com", Type: TypeA, TTL: 1, IP: net.IPv4(8, 8, 8, byte(i))})
	}
	out, err := Build(q, rrs, MaxUDP(q))
	if err != nil {
		t.Fatal(err)
	}
	h, _ = readHeader(out)
	if len(out) > 512 || !h.Truncated() || h.ANCount == 0 || int(h.ANCount) >= len(rrs) {
		t.Fatalf("len %d header %+v", len(out), h)
	}
}
