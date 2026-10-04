// Package dnsmsg is the small part of the DNS wire format (RFC 1035,
// RFC 3596) the Basalt resolver needs: read a query's question, read the
// address and alias records of an upstream answer, and build the answers
// the resolver sends back (a refusal, an empty answer, or a filtered
// answer holding only the records it accepted).
//
// It is deliberately narrow: names are decoded with compression pointers
// and loop protection, every length is bounds-checked, and anything it
// does not understand is an error, so the resolver refuses rather than
// passes on what it cannot check.
package dnsmsg

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
)

// Record types and classes used here.
const (
	TypeA     uint16 = 1
	TypeNS    uint16 = 2
	TypeCNAME uint16 = 5
	TypeSOA   uint16 = 6
	TypePTR   uint16 = 12
	TypeMX    uint16 = 15
	TypeTXT   uint16 = 16
	TypeAAAA  uint16 = 28
	TypeSRV   uint16 = 33
	TypeOPT   uint16 = 41
	TypeSVCB  uint16 = 64
	TypeHTTPS uint16 = 65
	TypeANY   uint16 = 255

	ClassINET uint16 = 1
)

// Response codes.
const (
	RcodeSuccess  = 0
	RcodeFormErr  = 1
	RcodeServFail = 2
	RcodeNXDomain = 3
	RcodeNotImp   = 4
	RcodeRefused  = 5
)

// Header flag bits.
const (
	flagQR = 1 << 15
	flagAA = 1 << 10
	flagTC = 1 << 9
	flagRD = 1 << 8
	flagRA = 1 << 7
)

var errShort = errors.New("dns message truncated")

// Header is the fixed 12-byte header.
type Header struct {
	ID      uint16
	Flags   uint16
	QDCount uint16
	ANCount uint16
	NSCount uint16
	ARCount uint16
}

// Rcode returns the response code of the header.
func (h Header) Rcode() int { return int(h.Flags & 0xf) }

// Truncated reports the TC bit.
func (h Header) Truncated() bool { return h.Flags&flagTC != 0 }

// Response reports the QR bit.
func (h Header) Response() bool { return h.Flags&flagQR != 0 }

// Question is a query's single question.
type Question struct {
	Name  string // lower case, no trailing dot
	Type  uint16
	Class uint16
}

// RR is a decoded resource record of the kinds the resolver keeps.
type RR struct {
	Name  string
	Type  uint16
	Class uint16
	TTL   uint32
	IP    net.IP // A and AAAA
	Alias string // CNAME target
}

// Query is a parsed client query.
type Query struct {
	Header   Header
	Question Question
	EDNS     bool   // the query carried an OPT record
	UDPSize  uint16 // its advertised UDP payload size
}

// Answer is a parsed upstream answer: the address and alias records of
// the answer section, in order.
type Answer struct {
	Header  Header
	Answers []RR
}

func readHeader(b []byte) (Header, error) {
	if len(b) < 12 {
		return Header{}, errShort
	}
	return Header{
		ID: binary.BigEndian.Uint16(b[0:]), Flags: binary.BigEndian.Uint16(b[2:]),
		QDCount: binary.BigEndian.Uint16(b[4:]), ANCount: binary.BigEndian.Uint16(b[6:]),
		NSCount: binary.BigEndian.Uint16(b[8:]), ARCount: binary.BigEndian.Uint16(b[10:]),
	}, nil
}

// readName decodes the name at off and returns it with the offset just
// after it in the original message (not after a pointer's target).
func readName(b []byte, off int) (string, int, error) {
	var labels []string
	end := -1
	jumps := 0
	total := 0
	for {
		if off >= len(b) {
			return "", 0, errShort
		}
		l := int(b[off])
		switch {
		case l == 0:
			if end < 0 {
				end = off + 1
			}
			name := strings.ToLower(strings.Join(labels, "."))
			return name, end, nil
		case l&0xc0 == 0xc0:
			if off+1 >= len(b) {
				return "", 0, errShort
			}
			if end < 0 {
				end = off + 2
			}
			jumps++
			if jumps > 32 {
				return "", 0, errors.New("dns name: too many compression pointers")
			}
			off = int(binary.BigEndian.Uint16(b[off:]) & 0x3fff)
		case l&0xc0 != 0:
			return "", 0, errors.New("dns name: unknown label type")
		default:
			if off+1+l > len(b) {
				return "", 0, errShort
			}
			label := string(b[off+1 : off+1+l])
			if strings.ContainsAny(label, ".\x00") {
				return "", 0, errors.New("dns name: label holds a dot or NUL")
			}
			total += l + 1
			if total > 255 {
				return "", 0, errors.New("dns name: longer than 255 bytes")
			}
			labels = append(labels, label)
			off += 1 + l
		}
	}
}

// appendName encodes name without compression.
func appendName(b []byte, name string) ([]byte, error) {
	name = strings.TrimSuffix(name, ".")
	if name != "" {
		for _, l := range strings.Split(name, ".") {
			if l == "" || len(l) > 63 {
				return nil, fmt.Errorf("dns name %q: bad label", name)
			}
			b = append(b, byte(len(l)))
			b = append(b, l...)
		}
	}
	return append(b, 0), nil
}

// ParseQuery reads a client query: exactly one question, opcode QUERY,
// not a response. Extra records are only inspected for an OPT record.
func ParseQuery(b []byte) (Query, error) {
	var q Query
	h, err := readHeader(b)
	if err != nil {
		return q, err
	}
	q.Header = h
	if h.Response() {
		return q, errors.New("not a query")
	}
	if (h.Flags>>11)&0xf != 0 {
		return q, errors.New("opcode is not QUERY")
	}
	if h.QDCount != 1 {
		return q, fmt.Errorf("%d questions, want 1", h.QDCount)
	}
	name, off, err := readName(b, 12)
	if err != nil {
		return q, err
	}
	if off+4 > len(b) {
		return q, errShort
	}
	q.Question = Question{Name: name, Type: binary.BigEndian.Uint16(b[off:]), Class: binary.BigEndian.Uint16(b[off+2:])}
	off += 4
	// Skip answer and authority records (normally none), then look for OPT.
	for i := 0; i < int(h.ANCount)+int(h.NSCount)+int(h.ARCount); i++ {
		_, n, err := readName(b, off)
		if err != nil {
			return q, err
		}
		if n+10 > len(b) {
			return q, errShort
		}
		typ := binary.BigEndian.Uint16(b[n:])
		class := binary.BigEndian.Uint16(b[n+2:])
		rdlen := int(binary.BigEndian.Uint16(b[n+8:]))
		if n+10+rdlen > len(b) {
			return q, errShort
		}
		if typ == TypeOPT && i >= int(h.ANCount)+int(h.NSCount) {
			q.EDNS, q.UDPSize = true, class
		}
		off = n + 10 + rdlen
	}
	return q, nil
}

// ParseAnswer reads an upstream answer to q: the header must match the
// query's id and the question must be the same; A, AAAA and CNAME records
// of the answer section are returned, other records are skipped.
func ParseAnswer(b []byte, q Question, id uint16) (Answer, error) {
	var a Answer
	h, err := readHeader(b)
	if err != nil {
		return a, err
	}
	a.Header = h
	if !h.Response() || h.ID != id {
		return a, errors.New("answer does not match the query")
	}
	off := 12
	for i := 0; i < int(h.QDCount); i++ {
		name, n, err := readName(b, off)
		if err != nil {
			return a, err
		}
		if n+4 > len(b) {
			return a, errShort
		}
		if name != q.Name || binary.BigEndian.Uint16(b[n:]) != q.Type {
			return a, errors.New("answer is for another question")
		}
		off = n + 4
	}
	for i := 0; i < int(h.ANCount); i++ {
		name, n, err := readName(b, off)
		if err != nil {
			return a, err
		}
		if n+10 > len(b) {
			return a, errShort
		}
		rr := RR{Name: name, Type: binary.BigEndian.Uint16(b[n:]), Class: binary.BigEndian.Uint16(b[n+2:]),
			TTL: binary.BigEndian.Uint32(b[n+4:])}
		rdlen := int(binary.BigEndian.Uint16(b[n+8:]))
		rd := n + 10
		if rd+rdlen > len(b) {
			return a, errShort
		}
		off = rd + rdlen
		if rr.Class != ClassINET {
			continue
		}
		switch rr.Type {
		case TypeA:
			if rdlen != 4 {
				return a, errors.New("bad A record")
			}
			rr.IP = net.IP(append([]byte(nil), b[rd:rd+4]...))
		case TypeAAAA:
			if rdlen != 16 {
				return a, errors.New("bad AAAA record")
			}
			rr.IP = net.IP(append([]byte(nil), b[rd:rd+16]...))
		case TypeCNAME:
			alias, _, err := readName(b, rd)
			if err != nil {
				return a, err
			}
			rr.Alias = alias
		default:
			continue
		}
		a.Answers = append(a.Answers, rr)
	}
	return a, nil
}

// header writes a response header for q with rcode and counts.
func header(q Query, rcode int, an uint16, ar uint16) []byte {
	b := make([]byte, 12, 512)
	binary.BigEndian.PutUint16(b[0:], q.Header.ID)
	flags := uint16(flagQR|flagRA) | q.Header.Flags&flagRD | uint16(rcode&0xf)
	binary.BigEndian.PutUint16(b[2:], flags)
	binary.BigEndian.PutUint16(b[4:], 1)
	binary.BigEndian.PutUint16(b[6:], an)
	binary.BigEndian.PutUint16(b[10:], ar)
	return b
}

func appendQuestion(b []byte, q Question) ([]byte, error) {
	b, err := appendName(b, q.Name)
	if err != nil {
		return nil, err
	}
	return binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(b, q.Type), q.Class), nil
}

// appendOPT adds an EDNS OPT record advertising size (no options).
func appendOPT(b []byte, size uint16) []byte {
	b = append(b, 0) // root name
	b = binary.BigEndian.AppendUint16(b, TypeOPT)
	b = binary.BigEndian.AppendUint16(b, size)
	b = binary.BigEndian.AppendUint32(b, 0)
	return binary.BigEndian.AppendUint16(b, 0)
}

// ServerUDPSize is the EDNS payload size the resolver advertises.
const ServerUDPSize = 1232

// Reply builds a response to q with rcode and no records (REFUSED,
// NXDOMAIN, SERVFAIL, or NOERROR with no data).
func Reply(q Query, rcode int) ([]byte, error) {
	var ar uint16
	if q.EDNS {
		ar = 1
	}
	b, err := appendQuestion(header(q, rcode, 0, ar), q.Question)
	if err != nil {
		return nil, err
	}
	if q.EDNS {
		b = appendOPT(b, ServerUDPSize)
	}
	return b, nil
}

// Build returns a NOERROR response to q holding rrs (A, AAAA, CNAME). When
// maxSize > 0 and the message would be larger, it is cut to the records
// that fit and the TC bit is set, so the client retries over TCP.
func Build(q Query, rrs []RR, maxSize int) ([]byte, error) {
	var ar uint16
	if q.EDNS {
		ar = 1
	}
	body := []byte{}
	var count uint16
	truncated := false
	base, err := appendQuestion(header(q, RcodeSuccess, 0, ar), q.Question)
	if err != nil {
		return nil, err
	}
	optLen := 0
	if q.EDNS {
		optLen = 11
	}
	for _, rr := range rrs {
		rec, err := appendName(nil, rr.Name)
		if err != nil {
			return nil, err
		}
		rec = binary.BigEndian.AppendUint16(rec, rr.Type)
		rec = binary.BigEndian.AppendUint16(rec, ClassINET)
		rec = binary.BigEndian.AppendUint32(rec, rr.TTL)
		switch rr.Type {
		case TypeA:
			ip4 := rr.IP.To4()
			if ip4 == nil {
				return nil, errors.New("A record without an IPv4 address")
			}
			rec = binary.BigEndian.AppendUint16(rec, 4)
			rec = append(rec, ip4...)
		case TypeAAAA:
			ip6 := rr.IP.To16()
			if ip6 == nil || rr.IP.To4() != nil {
				return nil, errors.New("AAAA record without an IPv6 address")
			}
			rec = binary.BigEndian.AppendUint16(rec, 16)
			rec = append(rec, ip6...)
		case TypeCNAME:
			rd, err := appendName(nil, rr.Alias)
			if err != nil {
				return nil, err
			}
			rec = binary.BigEndian.AppendUint16(rec, uint16(len(rd)))
			rec = append(rec, rd...)
		default:
			return nil, fmt.Errorf("record type %d cannot be built", rr.Type)
		}
		if maxSize > 0 && len(base)+len(body)+len(rec)+optLen > maxSize {
			truncated = true
			break
		}
		body = append(body, rec...)
		count++
	}
	binary.BigEndian.PutUint16(base[6:], count)
	if truncated {
		binary.BigEndian.PutUint16(base[2:], binary.BigEndian.Uint16(base[2:])|flagTC)
	}
	out := append(base, body...)
	if q.EDNS {
		out = appendOPT(out, ServerUDPSize)
	}
	return out, nil
}

// MaxUDP is the largest UDP response q accepts.
func MaxUDP(q Query) int {
	if q.EDNS && q.UDPSize > 512 {
		return min(int(q.UDPSize), ServerUDPSize)
	}
	return 512
}

// NewQuery encodes a query (tests and the upstream TCP retry use it).
func NewQuery(id uint16, name string, qtype uint16, edns bool) ([]byte, error) {
	b := make([]byte, 12)
	binary.BigEndian.PutUint16(b[0:], id)
	binary.BigEndian.PutUint16(b[2:], flagRD)
	binary.BigEndian.PutUint16(b[4:], 1)
	if edns {
		binary.BigEndian.PutUint16(b[10:], 1)
	}
	b, err := appendQuestion(b, Question{Name: name, Type: qtype, Class: ClassINET})
	if err != nil {
		return nil, err
	}
	if edns {
		b = appendOPT(b, ServerUDPSize)
	}
	return b, nil
}

// SetID rewrites the id of a message in place.
func SetID(b []byte, id uint16) {
	if len(b) >= 2 {
		binary.BigEndian.PutUint16(b, id)
	}
}

// TypeName is a short name of a record type for logs.
func TypeName(t uint16) string {
	switch t {
	case TypeA:
		return "A"
	case TypeAAAA:
		return "AAAA"
	case TypeCNAME:
		return "CNAME"
	case TypeMX:
		return "MX"
	case TypeTXT:
		return "TXT"
	case TypeNS:
		return "NS"
	case TypePTR:
		return "PTR"
	case TypeSOA:
		return "SOA"
	case TypeSRV:
		return "SRV"
	case TypeSVCB:
		return "SVCB"
	case TypeHTTPS:
		return "HTTPS"
	case TypeANY:
		return "ANY"
	}
	return fmt.Sprintf("TYPE%d", t)
}
