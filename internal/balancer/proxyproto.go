package balancer

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net/netip"
	"strconv"
	"strings"
)

// PROXY protocol (https://www.haproxy.org/download/2.9/doc/proxy-protocol.txt).

var proxyV2Sig = []byte("\r\n\r\n\x00\r\nQUIT\n")

var errNotProxy = errors.New("received something which does not look like a PROXY protocol header")

// proxyHeader is a parsed PROXY header. local is true for LOCAL (v2),
// UNKNOWN (v1) and unsupported families: the connection's own addresses
// apply.
type proxyHeader struct {
	src, dst netip.AddrPort
	local    bool
}

// readProxyHeader reads a PROXY protocol v1 or v2 header from br.
func readProxyHeader(br *bufio.Reader) (proxyHeader, error) {
	first, err := br.Peek(1)
	if err != nil {
		return proxyHeader{}, err
	}
	switch first[0] {
	case 'P':
		return readProxyV1(br)
	case '\r':
		return readProxyV2(br)
	}
	return proxyHeader{}, errNotProxy
}

func readProxyV1(br *bufio.Reader) (proxyHeader, error) {
	var line []byte
	for len(line) < 108 {
		c, err := br.ReadByte()
		if err != nil {
			return proxyHeader{}, err
		}
		line = append(line, c)
		if c == '\n' {
			break
		}
	}
	if !bytes.HasSuffix(line, []byte("\r\n")) {
		return proxyHeader{}, errNotProxy
	}
	f := strings.Split(string(line[:len(line)-2]), " ")
	if len(f) < 2 || f[0] != "PROXY" {
		return proxyHeader{}, errNotProxy
	}
	switch f[1] {
	case "UNKNOWN":
		return proxyHeader{local: true}, nil
	case "TCP4", "TCP6":
	default:
		return proxyHeader{}, errNotProxy
	}
	if len(f) != 6 {
		return proxyHeader{}, errNotProxy
	}
	src, err1 := netip.ParseAddr(f[2])
	dst, err2 := netip.ParseAddr(f[3])
	sp, err3 := strconv.ParseUint(f[4], 10, 16)
	dp, err4 := strconv.ParseUint(f[5], 10, 16)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || (f[1] == "TCP4") != src.Is4() || (f[1] == "TCP4") != dst.Is4() {
		return proxyHeader{}, errNotProxy
	}
	return proxyHeader{src: netip.AddrPortFrom(src, uint16(sp)), dst: netip.AddrPortFrom(dst, uint16(dp))}, nil
}

func readProxyV2(br *bufio.Reader) (proxyHeader, error) {
	var hdr [16]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return proxyHeader{}, err
	}
	if !bytes.Equal(hdr[:12], proxyV2Sig) || hdr[12]>>4 != 2 {
		return proxyHeader{}, errNotProxy
	}
	cmd, fam := hdr[12]&0x0f, hdr[13]
	n := int(binary.BigEndian.Uint16(hdr[14:16]))
	if cmd > 1 {
		return proxyHeader{}, errNotProxy
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(br, payload); err != nil {
		return proxyHeader{}, err
	}
	if cmd == 0 {
		return proxyHeader{local: true}, nil
	}
	switch fam >> 4 {
	case 1: // AF_INET
		if n < 12 {
			return proxyHeader{}, errNotProxy
		}
		src := netip.AddrFrom4([4]byte(payload[0:4]))
		dst := netip.AddrFrom4([4]byte(payload[4:8]))
		return proxyHeader{
			src: netip.AddrPortFrom(src, binary.BigEndian.Uint16(payload[8:10])),
			dst: netip.AddrPortFrom(dst, binary.BigEndian.Uint16(payload[10:12])),
		}, nil
	case 2: // AF_INET6
		if n < 36 {
			return proxyHeader{}, errNotProxy
		}
		src := netip.AddrFrom16([16]byte(payload[0:16])).Unmap()
		dst := netip.AddrFrom16([16]byte(payload[16:32])).Unmap()
		return proxyHeader{
			src: netip.AddrPortFrom(src, binary.BigEndian.Uint16(payload[32:34])),
			dst: netip.AddrPortFrom(dst, binary.BigEndian.Uint16(payload[34:36])),
		}, nil
	}
	return proxyHeader{local: true}, nil
}

// appendProxyV2 appends a PROXY protocol v2 PROXY header for a TCP
// connection from src to dst (both families mixed are sent as IPv6). Invalid
// addresses produce a LOCAL header.
func appendProxyV2(b []byte, src, dst netip.AddrPort) []byte {
	b = append(b, proxyV2Sig...)
	s, d := src.Addr().Unmap(), dst.Addr().Unmap()
	switch {
	case !s.IsValid() || !d.IsValid():
		return append(b, 0x20, 0x00, 0x00, 0x00)
	case s.Is4() && d.Is4():
		b = append(b, 0x21, 0x11, 0x00, 12)
		s4, d4 := s.As4(), d.As4()
		b = append(b, s4[:]...)
		b = append(b, d4[:]...)
	default:
		b = append(b, 0x21, 0x21, 0x00, 36)
		s16, d16 := s.As16(), d.As16()
		b = append(b, s16[:]...)
		b = append(b, d16[:]...)
	}
	b = binary.BigEndian.AppendUint16(b, src.Port())
	return binary.BigEndian.AppendUint16(b, dst.Port())
}
