package balancer

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestProxyProtocolParse(t *testing.T) {
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
		{"v2 tcp4", appendProxyV2(nil, src4, dst4), src4, dst4, false, false},
		{"v2 tcp6", appendProxyV2(nil, src6, dst6), src6, dst6, false, false},
		{"v2 local", []byte("\r\n\r\n\x00\r\nQUIT\n\x20\x00\x00\x00"), netip.AddrPort{}, netip.AddrPort{}, true, false},
		{"v2 bad version", []byte("\r\n\r\n\x00\r\nQUIT\n\x11\x11\x00\x0c"), netip.AddrPort{}, netip.AddrPort{}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, err := readProxyHeader(bufio.NewReader(bytes.NewReader(tt.in)))
			if tt.err {
				if err == nil {
					t.Fatalf("accepted: %+v", h)
				}
				return
			}
			if err != nil || h.local != tt.local || h.src != tt.src || h.dst != tt.dst {
				t.Fatalf("got %+v %v", h, err)
			}
		})
	}
	// Mixed families are sent as IPv6 (v4-mapped).
	h, err := readProxyHeader(bufio.NewReader(bytes.NewReader(appendProxyV2(nil, src4, dst6))))
	if err != nil || h.src != src4 || h.dst != dst6 {
		t.Fatalf("mixed: %+v %v", h, err)
	}
	// Remaining bytes stay readable.
	br := bufio.NewReader(bytes.NewReader([]byte("PROXY TCP4 192.0.2.10 198.51.100.1 51234 443\r\nGET")))
	readProxyHeader(br)
	if rest, _ := br.Peek(3); string(rest) != "GET" {
		t.Fatalf("rest %q", rest)
	}
}

func TestClientHelloSNI(t *testing.T) {
	capture := func(serverName string) []byte {
		c1, c2 := net.Pipe()
		defer c1.Close()
		defer c2.Close()
		go tls.Client(c1, &tls.Config{ServerName: serverName, InsecureSkipVerify: true}).Handshake()
		c2.SetReadDeadline(time.Now().Add(2 * time.Second))
		var data []byte
		buf := make([]byte, 4096)
		for {
			n, err := c2.Read(buf)
			data = append(data, buf[:n]...)
			if _, done := parseClientHelloSNI(data); done || err != nil {
				return data
			}
		}
	}
	hello := capture("Redis.Home.Lan")
	if sni, done := parseClientHelloSNI(hello); !done || sni != "redis.home.lan" {
		t.Fatalf("sni %q done %v", sni, done)
	}
	for i := 1; i < 10; i++ {
		if _, done := parseClientHelloSNI(hello[:i]); done {
			t.Fatalf("partial hello (%d bytes) reported done", i)
		}
	}
	// Split across two records.
	body := hello[5:]
	cut := len(body) / 2
	split := append([]byte{0x16, hello[1], hello[2], byte(cut >> 8), byte(cut)}, body[:cut]...)
	split = append(split, 0x16, hello[1], hello[2], byte((len(body)-cut)>>8), byte(len(body)-cut))
	split = append(split, body[cut:]...)
	if sni, done := parseClientHelloSNI(split); !done || sni != "redis.home.lan" {
		t.Fatalf("split: %q %v", sni, done)
	}
	if sni, done := parseClientHelloSNI(capture("")); !done || sni != "" {
		t.Fatalf("no sni: %q %v", sni, done)
	}
	if sni, done := parseClientHelloSNI([]byte("SSH-2.0")); !done || sni != "" {
		t.Fatalf("non tls: %q %v", sni, done)
	}
}

func TestLogPrefixParsesLikeHAProxy(t *testing.T) {
	buf := &syncBuffer{}
	l := newLogger(buf)
	l.sync(lvlAlert, "a")
	l.sync(lvlWarning, "b")
	l.sync(lvlNotice, "c\nd")
	l.close()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	want := []string{"[ALERT]    (", "[WARNING]  (", "[NOTICE]   ("}
	for i, w := range want {
		if !strings.HasPrefix(lines[i], w) {
			t.Errorf("line %q lacks prefix %q", lines[i], w)
		}
	}
	if !strings.HasSuffix(lines[2], ") : c d") {
		t.Errorf("newlines not flattened: %q", lines[2])
	}
}

func TestStickTable(t *testing.T) {
	now := time.Now()
	tb := newStickTable(2, time.Minute)
	a, b, c := netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("10.0.0.3")
	tb.put(a, "s1", now)
	tb.put(b, "s2", now)
	tb.get(a, now) // no LRU refresh on get: stick on src refreshes on store
	tb.put(a, "s1", now)
	tb.put(c, "s3", now) // evicts b (least recently stored)
	if _, ok := tb.get(b, now); ok || tb.len() != 2 {
		t.Fatalf("LRU eviction: len %d", tb.len())
	}
	if s, ok := tb.get(a, now.Add(59*time.Second)); !ok || s != "s1" {
		t.Fatal("entry expired early")
	}
	if _, ok := tb.get(a, now.Add(61*time.Second)); ok {
		t.Fatal("entry did not expire")
	}
}

func TestFreqCtr(t *testing.T) {
	var f freqCtr
	base := time.Unix(1000, 0)
	for range 10 {
		f.add(base)
	}
	if r := f.read(base.Add(900 * time.Millisecond)); r != 10 {
		t.Fatalf("same second: %d", r)
	}
	if r := f.read(base.Add(1500 * time.Millisecond)); r != 5 {
		t.Fatalf("half of previous second: %d", r)
	}
	if r := f.read(base.Add(3 * time.Second)); r != 0 {
		t.Fatalf("stale: %d", r)
	}
	var s swrate
	s.add(100)
	if s.avg() != 100 {
		t.Fatal(s.avg())
	}
	s.add(0)
	if s.avg() != 50 {
		t.Fatal(s.avg())
	}
}
