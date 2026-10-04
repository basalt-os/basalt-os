// Package nflog receives the packets nftables logs with "log group N"
// (NFLOG, netfilter's netlink logging), so the resolver can turn every
// dropped connection of a session into an audit record. Standard library
// only: a NETLINK_NETFILTER socket, the bind command for one group and a
// small parser for the packet messages and their IP headers.
package nflog

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
)

// Netlink and nfnetlink_log constants (linux/netfilter/nfnetlink_log.h).
const (
	netlinkNetfilter = 12
	nfnlSubsysULOG   = 4
	msgPacket        = 0
	msgConfig        = 1

	attrCfgCmd  = 1
	attrCfgMode = 2

	cmdBind = 1

	copyPacket = 2

	attrPayload = 9
	attrPrefix  = 10
)

// Packet is one logged packet.
type Packet struct {
	Prefix string
	Family int // 4 or 6
	Proto  string
	Src    net.IP
	Dst    net.IP
	SPort  int
	DPort  int
}

// Conn is a socket bound to one NFLOG group.
type Conn struct {
	fd    int
	group uint16
	seq   uint32
}

// Open binds to group and asks for the first 128 bytes of each packet.
func Open(group uint16) (*Conn, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, netlinkNetfilter)
	if err != nil {
		return nil, fmt.Errorf("nflog socket: %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("nflog bind: %w", err)
	}
	_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 1<<20)
	c := &Conn{fd: fd, group: group}
	if err := c.config(attr(attrCfgCmd, []byte{cmdBind, 0, 0, 0}[:1])); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("nflog bind group %d: %w", group, err)
	}
	mode := make([]byte, 6)
	binary.BigEndian.PutUint32(mode, 128)
	mode[4] = copyPacket
	if err := c.config(attr(attrCfgMode, mode)); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("nflog copy mode: %w", err)
	}
	return c, nil
}

// Close closes the socket; the kernel unbinds the group with it. (An
// explicit unbind would wait for an acknowledgement that the reading
// goroutine may consume.)
func (c *Conn) Close() error {
	return syscall.Close(c.fd)
}

// attr encodes one netlink attribute, padded to 4 bytes.
func attr(typ uint16, val []byte) []byte {
	l := 4 + len(val)
	b := make([]byte, (l+3)&^3)
	binary.LittleEndian.PutUint16(b[0:], uint16(l))
	binary.LittleEndian.PutUint16(b[2:], typ)
	copy(b[4:], val)
	return b
}

// config sends a config message for the group and waits for its ack.
func (c *Conn) config(attrs []byte) error {
	c.seq++
	body := make([]byte, 4)
	body[0] = syscall.AF_UNSPEC
	body[1] = 0 // NFNETLINK_V0
	binary.BigEndian.PutUint16(body[2:], c.group)
	body = append(body, attrs...)
	msg := make([]byte, 16, 16+len(body))
	binary.LittleEndian.PutUint32(msg[0:], uint32(16+len(body)))
	binary.LittleEndian.PutUint16(msg[4:], nfnlSubsysULOG<<8|msgConfig)
	binary.LittleEndian.PutUint16(msg[6:], syscall.NLM_F_REQUEST|syscall.NLM_F_ACK)
	binary.LittleEndian.PutUint32(msg[8:], c.seq)
	msg = append(msg, body...)
	if err := syscall.Sendto(c.fd, msg, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return err
	}
	buf := make([]byte, 8192)
	for {
		n, _, err := syscall.Recvfrom(c.fd, buf, 0)
		if err != nil {
			return err
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if m.Header.Type == syscall.NLMSG_ERROR && m.Header.Seq == c.seq {
				if len(m.Data) < 4 {
					return errors.New("short netlink ack")
				}
				if e := int32(binary.LittleEndian.Uint32(m.Data)); e != 0 {
					return syscall.Errno(-e)
				}
				return nil
			}
		}
	}
}

// Read blocks for the next batch of logged packets. A receive-buffer
// overrun (ENOBUFS) is reported as ErrOverrun; the caller keeps reading.
func (c *Conn) Read() ([]Packet, error) {
	buf := make([]byte, 65536)
	n, _, err := syscall.Recvfrom(c.fd, buf, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOBUFS) {
			return nil, ErrOverrun
		}
		return nil, os.NewSyscallError("recvfrom", err)
	}
	msgs, err := syscall.ParseNetlinkMessage(buf[:n])
	if err != nil {
		return nil, err
	}
	var out []Packet
	for _, m := range msgs {
		if m.Header.Type != nfnlSubsysULOG<<8|msgPacket {
			continue
		}
		if p, ok := ParsePacketMessage(m.Data); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// ErrOverrun means packets were lost before they could be read.
var ErrOverrun = errors.New("nflog: receive buffer overrun, some packets were not logged")

// ParsePacketMessage parses the body of an NFULNL_MSG_PACKET message
// (nfgenmsg header, then attributes).
func ParsePacketMessage(b []byte) (Packet, bool) {
	var p Packet
	if len(b) < 4 {
		return p, false
	}
	b = b[4:]
	var payload []byte
	for len(b) >= 4 {
		l := int(binary.LittleEndian.Uint16(b[0:]))
		typ := binary.LittleEndian.Uint16(b[2:]) & 0x3fff
		if l < 4 || l > len(b) {
			return p, false
		}
		v := b[4:l]
		switch typ {
		case attrPrefix:
			p.Prefix = string(trimNUL(v))
		case attrPayload:
			payload = v
		}
		adv := (l + 3) &^ 3
		if adv > len(b) {
			break
		}
		b = b[adv:]
	}
	if payload == nil {
		return p, false
	}
	return p, ParseIP(payload, &p)
}

func trimNUL(b []byte) []byte {
	for i, c := range b {
		if c == 0 {
			return b[:i]
		}
	}
	return b
}

// ParseIP fills addresses, protocol and ports from an IPv4 or IPv6 packet.
func ParseIP(b []byte, p *Packet) bool {
	if len(b) < 1 {
		return false
	}
	var proto byte
	var l4 []byte
	switch b[0] >> 4 {
	case 4:
		if len(b) < 20 {
			return false
		}
		ihl := int(b[0]&0xf) * 4
		if ihl < 20 || len(b) < ihl {
			return false
		}
		p.Family, proto = 4, b[9]
		p.Src, p.Dst = net.IP(append([]byte(nil), b[12:16]...)), net.IP(append([]byte(nil), b[16:20]...))
		l4 = b[ihl:]
	case 6:
		if len(b) < 40 {
			return false
		}
		p.Family, proto = 6, b[6]
		p.Src, p.Dst = net.IP(append([]byte(nil), b[8:24]...)), net.IP(append([]byte(nil), b[24:40]...))
		l4 = b[40:]
	default:
		return false
	}
	switch proto {
	case 6:
		p.Proto = "tcp"
	case 17:
		p.Proto = "udp"
	case 1, 58:
		p.Proto = "icmp"
		return true
	default:
		p.Proto = fmt.Sprintf("proto-%d", proto)
		return true
	}
	if len(l4) >= 4 {
		p.SPort = int(binary.BigEndian.Uint16(l4[0:]))
		p.DPort = int(binary.BigEndian.Uint16(l4[2:]))
	}
	return true
}
