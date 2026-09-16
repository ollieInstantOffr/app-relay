package sniff

import (
	"crypto/tls"
	"net"
	"testing"
	"time"
)

func captureHello(t testing.TB, serverName string) []byte {
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
		if _, done := ClientHelloSNI(data); done || err != nil {
			return data
		}
	}
}

func TestClientHelloSNI(t *testing.T) {
	hello := captureHello(t, "Redis.Home.Lan")
	if sni, done := ClientHelloSNI(hello); !done || sni != "redis.home.lan" {
		t.Fatalf("sni %q done %v", sni, done)
	}
	for i := 1; i < 10; i++ {
		if _, done := ClientHelloSNI(hello[:i]); done {
			t.Fatalf("partial hello (%d bytes) reported done", i)
		}
	}
	// Split across two records.
	body := hello[5:]
	cut := len(body) / 2
	split := append([]byte{0x16, hello[1], hello[2], byte(cut >> 8), byte(cut)}, body[:cut]...)
	split = append(split, 0x16, hello[1], hello[2], byte((len(body)-cut)>>8), byte(len(body)-cut))
	split = append(split, body[cut:]...)
	if sni, done := ClientHelloSNI(split); !done || sni != "redis.home.lan" {
		t.Fatalf("split: %q %v", sni, done)
	}
	if sni, done := ClientHelloSNI(captureHello(t, "")); !done || sni != "" {
		t.Fatalf("no sni: %q %v", sni, done)
	}
	if sni, done := ClientHelloSNI([]byte("SSH-2.0")); !done || sni != "" {
		t.Fatalf("non tls: %q %v", sni, done)
	}
}

func TestHTTPHost(t *testing.T) {
	tests := []struct {
		in   string
		host string
		done bool
	}{
		{"GET / HTTP/1.1\r\nHost: App.Example.com\r\nAccept: */*\r\n\r\n", "app.example.com", true},
		{"GET /x HTTP/1.1\r\nhost: app.example.com:8080\r\n\r\nbody", "app.example.com", true},
		{"GET / HTTP/1.1\r\nHost: [2001:db8::1]:80\r\n\r\n", "2001:db8::1", true},
		{"GET / HTTP/1.1\r\nHost: example.com.\r\n\r\n", "example.com", true},
		{"POST /upload HTTP/1.0\r\nContent-Length: 3\r\n\r\n", "", true}, // no Host
		{"GET / HTTP/1.1\r\nHost: a", "", false},                         // incomplete
		{"GE", "", false},
		{"PRI * HTTP/2.0\r\n\r\nSM", "", true}, // h2c prior knowledge
		{"\x16\x03\x01", "", true},             // TLS on :80
		{"SSH-2.0-OpenSSH\r\n", "", true},
		{"GET / HTTP/1.1\r\nbroken header\r\n\r\n", "", true},
		{"get / HTTP/1.1\r\nHost: x\r\n\r\n", "", true}, // lowercase method
	}
	for _, tt := range tests {
		host, done := HTTPHost([]byte(tt.in))
		if host != tt.host || done != tt.done {
			t.Errorf("%q: got %q %v, want %q %v", tt.in, host, done, tt.host, tt.done)
		}
	}
}

func FuzzClientHelloSNI(f *testing.F) {
	f.Add(captureHello(f, "app.example.com"))
	f.Add([]byte{0x16, 0x03, 0x01, 0x00, 0x05, 0x01, 0x00, 0x00, 0x01, 0x00})
	f.Fuzz(func(t *testing.T, data []byte) {
		ClientHelloSNI(data)
	})
}

func FuzzHTTPHost(f *testing.F) {
	f.Add([]byte("GET / HTTP/1.1\r\nHost: app.example.com\r\n\r\n"))
	f.Add([]byte("PRI * HTTP/2.0\r\n\r\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		host, done := HTTPHost(data)
		if !done && host != "" {
			t.Fatalf("host %q before done", host)
		}
	})
}
