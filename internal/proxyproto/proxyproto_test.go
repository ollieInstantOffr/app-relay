package proxyproto

import (
	"bufio"
	"bytes"
	"net"
	"net/netip"
	"testing"
)

func TestParse(t *testing.T) {
	src4 := netip.MustParseAddrPort("192.0.2.10:51234")
	dst4 := netip.MustParseAddrPort("198.51.100.1:443")
	src6 := netip.MustParseAddrPort("[2001:db8::1]:40000")
	dst6 := netip.MustParseAddrPort("[2001:db8::2]:8443")
	tests := []struct {
		name     string
		in       []byte
		src, dst netip.AddrPort
		local    bool
		err      bool
	}{
		{"v1 tcp4", []byte("PROXY TCP4 192.0.2.10 198.51.100.1 51234 443\r\nGET"), src4, dst4, false, false},
		{"v1 tcp6", []byte("PROXY TCP6 2001:db8::1 2001:db8::2 40000 8443\r\n"), src6, dst6, false, false},
		{"v1 unknown", []byte("PROXY UNKNOWN\r\n"), netip.AddrPort{}, netip.AddrPort{}, true, false},
		{"v1 family mismatch", []byte("PROXY TCP4 2001:db8::1 198.51.100.1 1 2\r\n"), netip.AddrPort{}, netip.AddrPort{}, false, true},
		{"v1 garbage", []byte("GET / HTTP/1.1\r\n"), netip.AddrPort{}, netip.AddrPort{}, false, true},
		{"v2 tcp4", AppendV2(nil, src4, dst4), src4, dst4, false, false},
		{"v2 tcp6", AppendV2(nil, src6, dst6), src6, dst6, false, false},
		{"v2 local", []byte("\r\n\r\n\x00\r\nQUIT\n\x20\x00\x00\x00"), netip.AddrPort{}, netip.AddrPort{}, true, false},
		{"v2 local helper", AppendV2Local(nil), netip.AddrPort{}, netip.AddrPort{}, true, false},
		{"v2 bad version", []byte("\r\n\r\n\x00\r\nQUIT\n\x11\x11\x00\x0c"), netip.AddrPort{}, netip.AddrPort{}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, err := Read(bufio.NewReader(bytes.NewReader(tt.in)))
			if tt.err {
				if err == nil {
					t.Fatalf("accepted: %+v", h)
				}
				return
			}
			if err != nil || h.Local != tt.local || h.Src != tt.src || h.Dst != tt.dst {
				t.Fatalf("got %+v %v", h, err)
			}
		})
	}
	// Mixed families are sent as IPv6 (v4-mapped).
	h, err := Read(bufio.NewReader(bytes.NewReader(AppendV2(nil, src4, dst6))))
	if err != nil || h.Src != src4 || h.Dst != dst6 {
		t.Fatalf("mixed: %+v %v", h, err)
	}
	// Remaining bytes stay readable.
	br := bufio.NewReader(bytes.NewReader([]byte("PROXY TCP4 192.0.2.10 198.51.100.1 51234 443\r\nGET")))
	Read(br)
	if rest, _ := br.Peek(3); string(rest) != "GET" {
		t.Fatalf("rest %q", rest)
	}
}

func TestTLVs(t *testing.T) {
	src := netip.MustParseAddrPort("203.0.113.9:40000")
	dst := netip.MustParseAddrPort("198.51.100.1:443")
	in := AppendV2(nil, src, dst, TLV{Type: TLVRelayTunnel, Value: []byte("gw-1")}, TLV{Type: 0xE1, Value: nil})
	in = append(in, "GET"...)
	br := bufio.NewReader(bytes.NewReader(in))
	h, err := Read(br)
	if err != nil || h.Src != src || h.Dst != dst || len(h.TLVs) != 2 {
		t.Fatalf("got %+v %v", h, err)
	}
	if v, ok := h.TLV(TLVRelayTunnel); !ok || string(v) != "gw-1" {
		t.Fatalf("tlv %q %v", v, ok)
	}
	if _, ok := h.TLV(0x01); ok {
		t.Fatal("unexpected tlv")
	}
	if rest, _ := br.Peek(3); string(rest) != "GET" {
		t.Fatalf("rest %q", rest)
	}
	// IPv6 with TLVs.
	src6 := netip.MustParseAddrPort("[2001:db8::7]:4000")
	h, err = Read(bufio.NewReader(bytes.NewReader(AppendV2(nil, src6, dst, TLV{Type: TLVRelayTunnel, Value: []byte("x")}))))
	if err != nil || h.Src != src6 {
		t.Fatalf("v6: %+v %v", h, err)
	}
	if v, _ := h.TLV(TLVRelayTunnel); string(v) != "x" {
		t.Fatalf("v6 tlv %q", v)
	}
}

func TestV1(t *testing.T) {
	src := &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 1000}
	dst := &net.TCPAddr{IP: net.ParseIP("192.0.2.2"), Port: 25565}
	if got := string(V1(src, dst)); got != "PROXY TCP4 192.0.2.1 192.0.2.2 1000 25565\r\n" {
		t.Fatalf("v4 %q", got)
	}
	src6 := &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 1}
	if got := string(V1(src6, dst)); got != "PROXY TCP6 2001:db8::1 192.0.2.2 1 25565\r\n" {
		t.Fatalf("v6 %q", got)
	}
	if got := string(V1(&net.UnixAddr{}, dst)); got != "PROXY UNKNOWN\r\n" {
		t.Fatalf("unknown %q", got)
	}
}

func FuzzRead(f *testing.F) {
	f.Add([]byte("PROXY TCP4 192.0.2.10 198.51.100.1 51234 443\r\n"))
	f.Add(AppendV2(nil, netip.MustParseAddrPort("[2001:db8::1]:1"), netip.MustParseAddrPort("[2001:db8::2]:2"), TLV{Type: TLVRelayTunnel, Value: []byte("gw")}))
	f.Add(AppendV2Local(nil))
	f.Fuzz(func(t *testing.T, data []byte) {
		h, err := Read(bufio.NewReader(bytes.NewReader(data)))
		if err != nil {
			return
		}
		if h.Local {
			return
		}
		// A parsed PROXY header must survive a round trip.
		back, err := Read(bufio.NewReader(bytes.NewReader(AppendV2(nil, h.Src, h.Dst, h.TLVs...))))
		if err != nil || back.Src != h.Src || back.Dst != h.Dst || len(back.TLVs) != len(h.TLVs) {
			t.Fatalf("round trip: %+v -> %+v %v", h, back, err)
		}
	})
}
