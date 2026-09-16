package wire

import (
	"bufio"
	"bytes"
	"errors"
	"net/netip"
	"strings"
	"testing"
)

func TestStreamHeaderRoundTrip(t *testing.T) {
	tests := []StreamHeader{
		{Kind: KindHTTPS, Port: 443, Src: netip.MustParseAddrPort("203.0.113.9:40000"), Dst: netip.MustParseAddrPort("198.51.100.1:443"), Name: "app.example.com"},
		{Kind: KindHTTP, Port: 80, Src: netip.MustParseAddrPort("[2001:db8::7]:1"), Dst: netip.MustParseAddrPort("[2001:db8::1]:80"), Name: "x"},
		{Kind: KindTCP, Port: 25565, Src: netip.MustParseAddrPort("[::ffff:203.0.113.9]:5"), Dst: netip.AddrPort{}},
	}
	for _, h := range tests {
		b, err := AppendStreamHeader(nil, h)
		if err != nil {
			t.Fatal(err)
		}
		r := bytes.NewReader(append(b, "rest"...))
		got, err := ReadStreamHeader(r)
		if err != nil {
			t.Fatalf("%+v: %v", h, err)
		}
		want := h
		want.Src = netip.AddrPortFrom(h.Src.Addr().Unmap(), h.Src.Port())
		if got != want {
			t.Errorf("got %+v want %+v", got, want)
		}
		rest := make([]byte, 4)
		r.Read(rest)
		if string(rest) != "rest" {
			t.Errorf("read past header: %q", rest)
		}
	}
	if _, err := AppendStreamHeader(nil, StreamHeader{Kind: 9}); err == nil {
		t.Error("bad kind accepted")
	}
	if _, err := AppendStreamHeader(nil, StreamHeader{Kind: KindHTTP, Name: strings.Repeat("a", 256)}); err == nil {
		t.Error("long name accepted")
	}
	bad := [][]byte{
		[]byte("XX\x01\x01\x00\x50\x00\x00\x00"),
		[]byte("RT\x02\x01\x00\x50\x00\x00\x00"),
		[]byte("RT\x01\x07\x00\x50\x00\x00\x00"),
		[]byte("RT\x01\x01\x00\x50\x05"),
	}
	for _, b := range bad {
		if _, err := ReadStreamHeader(bytes.NewReader(b)); err == nil {
			t.Errorf("accepted %q", b)
		}
	}
	upper, _ := AppendStreamHeader(nil, StreamHeader{Kind: KindHTTP, Name: "App.Example.com"})
	if _, err := ReadStreamHeader(bytes.NewReader(upper)); !errors.Is(err, ErrBadHeader) {
		t.Errorf("uppercase name: %v", err)
	}
}

func TestMessages(t *testing.T) {
	var buf bytes.Buffer
	msgs := []*Message{
		{Hello: &Hello{Proto: 1, MinProto: 1, Version: "1.2.3", PublicIPs: []string{"198.51.100.1"}}},
		{Routes: &Routes{Generation: 7, Names: []string{"a.example.com", "*.b.example.com"}, TCP: []uint16{25565}}},
		{Ack: &RoutesAck{Generation: 7, Errors: []PortError{{Port: 25565, Error: "address in use"}}}},
		{Ping: &Ping{ID: 1, SentNs: 42}},
		{Unpair: &Unpair{}},
	}
	for _, m := range msgs {
		if err := WriteMessage(&buf, m); err != nil {
			t.Fatal(err)
		}
	}
	// A newer peer's unknown message type is ignored (all fields nil).
	buf.Write([]byte{0, 0, 0, 13})
	buf.WriteString(`{"future":{}}`)
	br := bufio.NewReader(&buf)
	for i, want := range msgs {
		got, err := ReadMessage(br)
		if err != nil {
			t.Fatal(err)
		}
		switch i {
		case 0:
			if got.Hello == nil || got.Hello.Version != "1.2.3" || got.Hello.PublicIPs[0] != "198.51.100.1" {
				t.Errorf("hello %+v", got.Hello)
			}
		case 1:
			if got.Routes == nil || got.Routes.Generation != 7 || len(got.Routes.Names) != 2 || got.Routes.TCP[0] != 25565 {
				t.Errorf("routes %+v", got.Routes)
			}
		case 2:
			if got.Ack == nil || got.Ack.Errors[0].Port != 25565 {
				t.Errorf("ack %+v", got.Ack)
			}
		case 3:
			if got.Ping == nil || got.Ping.SentNs != 42 {
				t.Errorf("ping %+v", got.Ping)
			}
		case 4:
			if got.Unpair == nil {
				t.Errorf("unpair %+v (want %+v)", got, want)
			}
		}
	}
	future, err := ReadMessage(br)
	if err != nil || *future != (Message{}) {
		t.Errorf("future message: %+v %v", future, err)
	}
	huge := bufio.NewReader(bytes.NewReader([]byte{0xff, 0xff, 0xff, 0xff}))
	if _, err := ReadMessage(huge); !errors.Is(err, ErrMessageTooLarge) {
		t.Errorf("huge: %v", err)
	}
}

func TestNegotiate(t *testing.T) {
	a := &Hello{Proto: 3, MinProto: 1}
	if p, err := Negotiate(a, &Hello{Proto: 2, MinProto: 2}); err != nil || p != 2 {
		t.Errorf("got %d %v", p, err)
	}
	if _, err := Negotiate(a, &Hello{Proto: 5, MinProto: 4}); !errors.Is(err, ErrIncompatible) {
		t.Errorf("incompatible: %v", err)
	}
	if _, err := Negotiate(a, nil); err == nil {
		t.Error("nil hello")
	}
	if p, err := Negotiate(LocalHello("x"), LocalHello("y")); err != nil || p != Proto {
		t.Errorf("local: %d %v", p, err)
	}
}

func TestNames(t *testing.T) {
	n := NewNames([]string{"App.Example.com.", "*.home.example.com", " ", "*.lan"})
	for name, want := range map[string]bool{
		"app.example.com": true, "APP.example.com.": true, "other.example.com": false,
		"grafana.home.example.com": true, "a.b.home.example.com": true, "home.example.com": false,
		"nas.lan": true, "lan": false, "": false, "example.com": false,
	} {
		if got := n.Match(name); got != want {
			t.Errorf("%q: %v", name, got)
		}
	}
	var none *Names
	if none.Match("x") {
		t.Error("nil matcher")
	}
}

func FuzzReadStreamHeader(f *testing.F) {
	b, _ := AppendStreamHeader(nil, StreamHeader{Kind: KindHTTPS, Port: 443, Src: netip.MustParseAddrPort("203.0.113.9:1"), Name: "a.example.com"})
	f.Add(b)
	f.Fuzz(func(t *testing.T, data []byte) {
		h, err := ReadStreamHeader(bytes.NewReader(data))
		if err != nil {
			return
		}
		back, err := AppendStreamHeader(nil, h)
		if err != nil {
			t.Fatalf("re-encode %+v: %v", h, err)
		}
		again, err := ReadStreamHeader(bytes.NewReader(back))
		if err != nil || again != h {
			t.Fatalf("round trip %+v -> %+v %v", h, again, err)
		}
	})
}
