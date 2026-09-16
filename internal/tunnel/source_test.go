package tunnel

import (
	"net"
	"net/netip"
	"testing"
)

func TestPublicAddr(t *testing.T) {
	cases := []struct {
		addr   string
		public bool
	}{
		// public
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"203.0.113.7", true},
		{"100.63.255.255", true}, // just below CGNAT
		{"100.128.0.0", true},    // just above CGNAT
		{"172.32.0.1", true},     // just above 172.16/12
		{"11.0.0.1", true},
		{"::ffff:8.8.8.8", true},
		{"2001:4860:4860::8888", true},
		{"2a00:1450:4001::1", true},

		// unspecified / this network
		{"0.0.0.0", false},
		{"0.1.2.3", false},
		{"::", false},
		{"::ffff:0.0.0.0", false},
		// loopback
		{"127.0.0.1", false},
		{"127.255.255.254", false},
		{"::1", false},
		{"::ffff:127.0.0.1", false},
		// RFC 1918
		{"10.0.0.1", false},
		{"10.255.255.255", false},
		{"172.16.0.1", false},
		{"172.31.255.255", false},
		{"192.168.0.1", false},
		{"192.168.255.255", false},
		{"::ffff:10.1.2.3", false},
		{"::ffff:192.168.1.1", false},
		// CGNAT
		{"100.64.0.0", false},
		{"100.100.100.100", false},
		{"100.127.255.255", false},
		{"::ffff:100.64.0.1", false},
		// link-local
		{"169.254.0.1", false},
		{"169.254.169.254", false},
		{"fe80::1", false},
		{"fe80::1%eth0", false},
		{"febf::1", false},
		// unique local
		{"fc00::1", false},
		{"fd12:3456:789a::1", false},
		// multicast
		{"224.0.0.1", false},
		{"239.255.255.250", false},
		{"ff02::1", false},
		{"ff05::1:3", false},
		{"ff01::1", false},
		// reserved / broadcast
		{"240.0.0.1", false},
		{"255.255.255.255", false},
	}
	for _, c := range cases {
		a := netip.MustParseAddr(c.addr)
		if got := publicAddr(a); got != c.public {
			t.Errorf("publicAddr(%s) = %v, want %v", c.addr, got, c.public)
		}
	}
	if publicAddr(netip.Addr{}) {
		t.Error("invalid address is public")
	}
}

func TestSanitizeSource(t *testing.T) {
	remote := netip.MustParseAddr("198.51.100.200")
	cases := []struct {
		src  netip.AddrPort
		want string
	}{
		{netip.MustParseAddrPort("203.0.113.7:5555"), "203.0.113.7:5555"},
		{netip.MustParseAddrPort("[::ffff:203.0.113.7]:5555"), "203.0.113.7:5555"}, // unmapped
		{netip.MustParseAddrPort("[2001:4860::1]:443"), "[2001:4860::1]:443"},
		{netip.MustParseAddrPort("192.168.1.10:5555"), "198.51.100.200:0"},
		{netip.MustParseAddrPort("[::ffff:192.168.1.10]:5555"), "198.51.100.200:0"},
		{netip.MustParseAddrPort("127.0.0.1:80"), "198.51.100.200:0"},
		{netip.MustParseAddrPort("[::1]:80"), "198.51.100.200:0"},
		{netip.MustParseAddrPort("100.64.1.1:80"), "198.51.100.200:0"},
		{netip.MustParseAddrPort("[fd00::5]:80"), "198.51.100.200:0"},
		{netip.MustParseAddrPort("[fe80::5%en0]:80"), "198.51.100.200:0"},
		{netip.MustParseAddrPort("0.0.0.0:0"), "198.51.100.200:0"},
		{netip.MustParseAddrPort("224.0.0.251:5353"), "198.51.100.200:0"},
		{netip.AddrPort{}, "198.51.100.200:0"},
	}
	for _, c := range cases {
		got, ok := sanitizeSource(c.src, remote)
		if !ok || got.String() != c.want {
			t.Errorf("sanitizeSource(%s) = %s, %v, want %s", c.src, got, ok, c.want)
		}
	}
	// The session's own address is used even when it is not public (a
	// gateway on the LAN), but a missing one rejects the stream: an invalid
	// address would produce a LOCAL header.
	if got, ok := sanitizeSource(netip.MustParseAddrPort("10.0.0.1:1"), netip.MustParseAddr("::ffff:192.168.1.2")); !ok || got.String() != "192.168.1.2:0" {
		t.Errorf("LAN gateway: %s, %v", got, ok)
	}
	// A gateway reached over loopback never makes clients look local.
	if got, ok := sanitizeSource(netip.MustParseAddrPort("10.0.0.1:1"), netip.MustParseAddr("127.0.0.1")); !ok || got.String() != "0.0.0.0:0" {
		t.Errorf("loopback gateway: %s, %v", got, ok)
	}
	if got, ok := sanitizeSource(netip.MustParseAddrPort("[fd00::1]:1"), netip.MustParseAddr("::1")); !ok || got.String() != "[::]:0" {
		t.Errorf("loopback v6 gateway: %s, %v", got, ok)
	}
	if _, ok := sanitizeSource(netip.MustParseAddrPort("10.0.0.1:1"), netip.Addr{}); ok {
		t.Error("no usable source accepted")
	}
	if got, ok := sanitizeSource(netip.MustParseAddrPort("8.8.8.8:1"), netip.Addr{}); !ok || got.String() != "8.8.8.8:1" {
		t.Errorf("public source without remote: %s, %v", got, ok)
	}

	// Destinations only need to be valid, in the source's family.
	for _, c := range []struct {
		dst, src, remote string
		port             uint16
		want             string
	}{
		{"10.0.0.5:443", "203.0.113.1:1", "198.51.100.200", 443, "10.0.0.5:443"},
		{"[::ffff:198.51.100.1]:443", "203.0.113.1:1", "198.51.100.200", 443, "198.51.100.1:443"},
		{"[2001:db8::1]:443", "[2001:4860::1]:1", "198.51.100.200", 443, "[2001:db8::1]:443"},
		{"", "203.0.113.1:1", "198.51.100.200", 8443, "198.51.100.200:8443"},
		{"[2001:db8::1]:443", "203.0.113.1:1", "198.51.100.200", 443, "198.51.100.200:443"},
		{"198.51.100.1:443", "[2001:4860::1]:1", "198.51.100.200", 443, "[::]:443"},
		{"", "203.0.113.1:1", "", 80, "0.0.0.0:80"},
	} {
		var dst netip.AddrPort
		if c.dst != "" {
			dst = netip.MustParseAddrPort(c.dst)
		}
		var remote netip.Addr
		if c.remote != "" {
			remote = netip.MustParseAddr(c.remote)
		}
		if got := sanitizeDest(dst, netip.MustParseAddrPort(c.src), remote, c.port); got.String() != c.want {
			t.Errorf("sanitizeDest(%s, %s, %s) = %s, want %s", c.dst, c.src, c.remote, got, c.want)
		}
	}
}

func TestAddrIP(t *testing.T) {
	cases := []struct {
		addr net.Addr
		want string
	}{
		{&net.UDPAddr{IP: net.ParseIP("203.0.113.1"), Port: 443}, "203.0.113.1"},
		{&net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 443}, "2001:db8::1"},
		{&net.TCPAddr{IP: net.IPv4(198, 51, 100, 1).To16(), Port: 443}, "198.51.100.1"},
		{(*net.TCPAddr)(nil), "invalid IP"},
		{nil, "invalid IP"},
		{&net.UnixAddr{Name: "/x", Net: "unix"}, "invalid IP"},
	}
	for _, c := range cases {
		if got := addrIP(c.addr).String(); got != c.want {
			t.Errorf("addrIP(%v) = %s, want %s", c.addr, got, c.want)
		}
	}
}
