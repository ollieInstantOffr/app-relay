package gateway

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// portRange is an inclusive range of TCP ports.
type portRange struct{ lo, hi uint16 }

// parsePortRanges parses "1024-65535" or a comma-separated list of ports and
// ranges ("8000-8100,9000").
func parsePortRanges(s string) ([]portRange, error) {
	var out []portRange
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := parsePort(lo)
		if err != nil {
			return nil, fmt.Errorf("invalid port range %q", part)
		}
		b := a
		if isRange {
			if b, err = parsePort(hi); err != nil || b < a {
				return nil, fmt.Errorf("invalid port range %q", part)
			}
		}
		out = append(out, portRange{a, b})
	}
	return out, nil
}

func parsePort(s string) (uint16, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 16)
	if err != nil || n == 0 {
		return 0, errors.New("invalid port")
	}
	return uint16(n), nil
}

// portAllowed reports whether a published TCP port may be bound: it must be
// in an allowed range and not one of the ports the gateway itself serves.
func (s *Server) portAllowed(p uint16) bool {
	if s.reserved[p] {
		return false
	}
	for _, r := range s.allow {
		if p >= r.lo && p <= r.hi {
			return true
		}
	}
	return false
}

// addrPort returns a TCP or UDP address with IPv4-mapped addresses unmapped.
func addrPort(a net.Addr) netip.AddrPort {
	var ap netip.AddrPort
	switch a := a.(type) {
	case *net.TCPAddr:
		ap = a.AddrPort()
	case *net.UDPAddr:
		ap = a.AddrPort()
	default:
		if a != nil {
			ap, _ = netip.ParseAddrPort(a.String())
		}
	}
	return netip.AddrPortFrom(ap.Addr().Unmap().WithZone(""), ap.Port())
}

// addrPortNum returns the port of a listen address like ":443".
func addrPortNum(addr string) uint16 {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseUint(p, 10, 16)
	return uint16(n)
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// publicAddr reports whether a is a usable public address.
func publicAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsValid() && a.IsGlobalUnicast() && !a.IsPrivate() && !a.IsLoopback() && !cgnat.Contains(a)
}

// interfacePublicIPs lists the public addresses of local interfaces.
func interfacePublicIPs() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		pfx, err := netip.ParsePrefix(a.String())
		if err != nil {
			continue
		}
		if ip := pfx.Addr().Unmap(); publicAddr(ip) {
			out = append(out, ip.String())
		}
	}
	return out
}

// bindReason shortens a listen error to its cause ("address already in use").
func bindReason(err error) string {
	var se *os.SyscallError
	if errors.As(err, &se) {
		return se.Err.Error()
	}
	return err.Error()
}
