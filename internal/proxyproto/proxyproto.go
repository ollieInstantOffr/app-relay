// Package proxyproto reads and writes PROXY protocol headers
// (https://www.haproxy.org/download/2.9/doc/proxy-protocol.txt). It is shared
// by Relay Balancer, Relay Edge and the tunnel engine.
package proxyproto

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

var v2Sig = []byte("\r\n\r\n\x00\r\nQUIT\n")

// ErrNotProxy is returned for data that is not a PROXY protocol header.
var ErrNotProxy = errors.New("received something which does not look like a PROXY protocol header")

// TLV types used by Relay (0xE0-0xEF is the range reserved for custom use).
const (
	// TLVRelayTunnel carries the id of the tunnel gateway a connection came through.
	TLVRelayTunnel byte = 0xE0
)

// TLV is one v2 type-length-value extension.
type TLV struct {
	Type  byte
	Value []byte
}

// Header is a parsed PROXY header. Local is true for LOCAL (v2), UNKNOWN (v1)
// and unsupported families: the connection's own addresses apply.
type Header struct {
	Src, Dst netip.AddrPort
	Local    bool
	TLVs     []TLV
}

// TLV returns the value of the first TLV of type t.
func (h Header) TLV(t byte) ([]byte, bool) {
	for _, v := range h.TLVs {
		if v.Type == t {
			return v.Value, true
		}
	}
	return nil, false
}

// Read reads a PROXY protocol v1 or v2 header from br. Bytes after the header
// stay readable from br.
func Read(br *bufio.Reader) (Header, error) {
	first, err := br.Peek(1)
	if err != nil {
		return Header{}, err
	}
	switch first[0] {
	case 'P':
		return readV1(br)
	case '\r':
		return readV2(br)
	}
	return Header{}, ErrNotProxy
}

func readV1(br *bufio.Reader) (Header, error) {
	var line []byte
	for len(line) < 108 {
		c, err := br.ReadByte()
		if err != nil {
			return Header{}, err
		}
		line = append(line, c)
		if c == '\n' {
			break
		}
	}
	if !bytes.HasSuffix(line, []byte("\r\n")) {
		return Header{}, ErrNotProxy
	}
	f := strings.Split(string(line[:len(line)-2]), " ")
	if len(f) < 2 || f[0] != "PROXY" {
		return Header{}, ErrNotProxy
	}
	switch f[1] {
	case "UNKNOWN":
		return Header{Local: true}, nil
	case "TCP4", "TCP6":
	default:
		return Header{}, ErrNotProxy
	}
	if len(f) != 6 {
		return Header{}, ErrNotProxy
	}
	src, err1 := netip.ParseAddr(f[2])
	dst, err2 := netip.ParseAddr(f[3])
	sp, err3 := strconv.ParseUint(f[4], 10, 16)
	dp, err4 := strconv.ParseUint(f[5], 10, 16)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || (f[1] == "TCP4") != src.Is4() || (f[1] == "TCP4") != dst.Is4() {
		return Header{}, ErrNotProxy
	}
	return Header{Src: netip.AddrPortFrom(src, uint16(sp)), Dst: netip.AddrPortFrom(dst, uint16(dp))}, nil
}

func readV2(br *bufio.Reader) (Header, error) {
	var hdr [16]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return Header{}, err
	}
	if !bytes.Equal(hdr[:12], v2Sig) || hdr[12]>>4 != 2 {
		return Header{}, ErrNotProxy
	}
	cmd, fam := hdr[12]&0x0f, hdr[13]
	n := int(binary.BigEndian.Uint16(hdr[14:16]))
	if cmd > 1 {
		return Header{}, ErrNotProxy
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(br, payload); err != nil {
		return Header{}, err
	}
	var h Header
	var rest []byte
	switch {
	case cmd == 0:
		h.Local = true
		rest = payload
		if fam>>4 == 1 && n >= 12 {
			rest = payload[12:]
		} else if fam>>4 == 2 && n >= 36 {
			rest = payload[36:]
		}
	case fam>>4 == 1: // AF_INET
		if n < 12 {
			return Header{}, ErrNotProxy
		}
		h.Src = netip.AddrPortFrom(netip.AddrFrom4([4]byte(payload[0:4])), binary.BigEndian.Uint16(payload[8:10]))
		h.Dst = netip.AddrPortFrom(netip.AddrFrom4([4]byte(payload[4:8])), binary.BigEndian.Uint16(payload[10:12]))
		rest = payload[12:]
	case fam>>4 == 2: // AF_INET6
		if n < 36 {
			return Header{}, ErrNotProxy
		}
		h.Src = netip.AddrPortFrom(netip.AddrFrom16([16]byte(payload[0:16])).Unmap(), binary.BigEndian.Uint16(payload[32:34]))
		h.Dst = netip.AddrPortFrom(netip.AddrFrom16([16]byte(payload[16:32])).Unmap(), binary.BigEndian.Uint16(payload[34:36]))
		rest = payload[36:]
	default:
		return Header{Local: true}, nil
	}
	h.TLVs = parseTLVs(rest)
	return h, nil
}

// parseTLVs parses v2 extensions; a truncated trailing TLV is ignored.
func parseTLVs(b []byte) []TLV {
	var out []TLV
	for len(b) >= 3 {
		n := int(binary.BigEndian.Uint16(b[1:3]))
		if len(b) < 3+n {
			break
		}
		out = append(out, TLV{Type: b[0], Value: b[3 : 3+n : 3+n]})
		b = b[3+n:]
	}
	return out
}

// AppendV2 appends a PROXY protocol v2 PROXY header for a TCP connection from
// src to dst (mixed families are sent as IPv6), followed by tlvs. Invalid
// addresses produce a LOCAL header.
func AppendV2(b []byte, src, dst netip.AddrPort, tlvs ...TLV) []byte {
	b = append(b, v2Sig...)
	s, d := src.Addr().Unmap(), dst.Addr().Unmap()
	var addrLen int
	switch {
	case !s.IsValid() || !d.IsValid():
		b = append(b, 0x20, 0x00)
	case s.Is4() && d.Is4():
		b = append(b, 0x21, 0x11)
		addrLen = 12
	default:
		b = append(b, 0x21, 0x21)
		addrLen = 36
	}
	n := addrLen
	for _, t := range tlvs {
		n += 3 + len(t.Value)
	}
	b = binary.BigEndian.AppendUint16(b, uint16(n))
	switch addrLen {
	case 12:
		s4, d4 := s.As4(), d.As4()
		b = append(b, s4[:]...)
		b = append(b, d4[:]...)
	case 36:
		s16, d16 := s.As16(), d.As16()
		b = append(b, s16[:]...)
		b = append(b, d16[:]...)
	}
	if addrLen > 0 {
		b = binary.BigEndian.AppendUint16(b, src.Port())
		b = binary.BigEndian.AppendUint16(b, dst.Port())
	}
	for _, t := range tlvs {
		b = append(b, t.Type)
		b = binary.BigEndian.AppendUint16(b, uint16(len(t.Value)))
		b = append(b, t.Value...)
	}
	return b
}

// AppendV2Local appends a PROXY protocol v2 LOCAL header (health checks).
func AppendV2Local(b []byte) []byte {
	b = append(b, v2Sig...)
	return append(b, 0x20, 0x00, 0x00, 0x00)
}

// V1 returns the PROXY protocol v1 header nginx sends with `proxy_protocol
// on` for a connection from src to dst.
func V1(src, dst net.Addr) []byte {
	sa, sOK := src.(*net.TCPAddr)
	da, dOK := dst.(*net.TCPAddr)
	if !sOK || !dOK {
		return []byte("PROXY UNKNOWN\r\n")
	}
	family := "TCP4"
	sip, dip := sa.IP.To4(), da.IP.To4()
	if sip == nil || dip == nil {
		family, sip, dip = "TCP6", sa.IP.To16(), da.IP.To16()
	}
	return fmt.Appendf(nil, "PROXY %s %s %s %d %d\r\n", family, sip, dip, sa.Port, da.Port)
}
