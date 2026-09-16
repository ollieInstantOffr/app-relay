package tunnel

import (
	"net"
	"net/netip"
)

// The gateway is not trusted with client addresses that would grant LAN
// privileges in the proxy engine (allow lists, real_ip, rate-limit
// exemptions): only public global unicast sources are passed on.

var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),     // "this network"
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT shared address space
	netip.MustParsePrefix("240.0.0.0/4"),   // reserved, incl. broadcast
	netip.MustParsePrefix("fc00::/7"),      // unique local
}

// publicAddr reports whether a is a public global unicast address.
func publicAddr(a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	if !a.IsValid() || a.IsUnspecified() || a.IsLoopback() || a.IsPrivate() ||
		a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() ||
		a.IsMulticast() || !a.IsGlobalUnicast() {
		return false
	}
	for _, p := range nonPublicPrefixes {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// sanitizeSource returns the client address to put in the PROXY header: src
// when it is public, otherwise the gateway session's remote address with
// port 0. A gateway reached over loopback is replaced by the unspecified
// address, so tunnelled clients never look like local connections. ok is
// false when nothing is usable (an invalid address would make a LOCAL header,
// which proxies treat as a local connection).
func sanitizeSource(src netip.AddrPort, remote netip.Addr) (netip.AddrPort, bool) {
	if a := src.Addr().Unmap(); publicAddr(a) {
		return netip.AddrPortFrom(a, src.Port()), true
	}
	remote = remote.Unmap().WithZone("")
	switch {
	case !remote.IsValid():
		return netip.AddrPort{}, false
	case remote.IsLoopback() && remote.Is4():
		return netip.AddrPortFrom(netip.IPv4Unspecified(), 0), true
	case remote.IsLoopback():
		return netip.AddrPortFrom(netip.IPv6Unspecified(), 0), true
	}
	return netip.AddrPortFrom(remote, 0), true
}

// sanitizeDest returns the destination for the PROXY header, in the family
// of src (a mixed header would carry the client as a v4-mapped IPv6
// address): dst when usable, otherwise the gateway's address, otherwise the
// unspecified address, with the public port.
func sanitizeDest(dst netip.AddrPort, src netip.AddrPort, remote netip.Addr, port uint16) netip.AddrPort {
	is4 := src.Addr().Unmap().Is4()
	if a := dst.Addr().Unmap().WithZone(""); a.IsValid() && a.Is4() == is4 {
		return netip.AddrPortFrom(a, dst.Port())
	}
	if remote = remote.Unmap().WithZone(""); remote.IsValid() && remote.Is4() == is4 {
		return netip.AddrPortFrom(remote, port)
	}
	if is4 {
		return netip.AddrPortFrom(netip.IPv4Unspecified(), port)
	}
	return netip.AddrPortFrom(netip.IPv6Unspecified(), port)
}

// addrIP extracts the IP of a session's remote address.
func addrIP(a net.Addr) netip.Addr {
	switch v := a.(type) {
	case nil:
		return netip.Addr{}
	case *net.UDPAddr:
		if v == nil {
			return netip.Addr{}
		}
		ip, _ := netip.AddrFromSlice(v.IP)
		return ip.Unmap()
	case *net.TCPAddr:
		if v == nil {
			return netip.Addr{}
		}
		ip, _ := netip.AddrFromSlice(v.IP)
		return ip.Unmap()
	}
	if ap, err := netip.ParseAddrPort(a.String()); err == nil {
		return ap.Addr().Unmap()
	}
	if ip, err := netip.ParseAddr(a.String()); err == nil {
		return ip.Unmap()
	}
	return netip.Addr{}
}
