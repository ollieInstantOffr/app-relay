package lbcheck

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

// serve runs handle for every connection on a loopback listener.
func serve(t *testing.T, handle func(net.Conn)) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				handle(c)
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func closedPort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return p
}

func check(t *testing.T, tg Target) Result {
	t.Helper()
	if tg.Address == "" {
		tg.Address = "127.0.0.1"
	}
	if tg.Timeout == 0 {
		tg.Timeout = 2 * time.Second
	}
	return Run(context.Background(), tg)
}

func expect(t *testing.T, r Result, status string, ok bool, info string) {
	t.Helper()
	if r.Status != status || r.OK != ok || !strings.Contains(r.Info, info) {
		t.Fatalf("got %+v, want %s ok=%v info~%q", r, status, ok, info)
	}
	if Description(r.Status) == "" {
		t.Fatalf("no description for %s", r.Status)
	}
}

func TestTCP(t *testing.T) {
	port := serve(t, func(c net.Conn) {})
	expect(t, check(t, Target{Port: port}), L4OK, true, "connected")
	expect(t, check(t, Target{Port: closedPort(t)}), L4CON, false, "connection refused")
	r := check(t, Target{Port: 1, Timeout: 50 * time.Millisecond, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, &net.OpError{Op: "dial", Err: ctx.Err()}
	}})
	expect(t, r, L4TOUT, false, "timeout")
	if r.Description() != "Layer4 timeout" {
		t.Fatal(r.Description())
	}
	expect(t, check(t, Target{Address: "nonexistent.invalid", Port: 80}), L4CON, false, "")
}

func TestHTTP(t *testing.T) {
	lines := make(chan string, 8)
	respond := func(status string) func(net.Conn) {
		return func(c net.Conn) {
			br := bufio.NewReader(c)
			var req []string
			for {
				l, err := br.ReadString('\n')
				if err != nil || l == "\r\n" {
					break
				}
				req = append(req, strings.TrimSpace(l))
			}
			lines <- strings.Join(req, "|")
			io.WriteString(c, "HTTP/1.1 "+status+"\r\nContent-Length: 0\r\n\r\n")
		}
	}
	ok := serve(t, respond("200 OK"))
	expect(t, check(t, Target{Type: TypeHTTP, Port: ok}), L7OK, true, "200 OK")
	if l := <-lines; l != "GET / HTTP/1.0" {
		t.Errorf("HTTP/1.0 request without Host: %q", l)
	}
	r := check(t, Target{Type: TypeHTTP, Port: ok, Method: "head", Path: "/health", Host: "app.lan", UserAgent: "UA"})
	expect(t, r, L7OK, true, "")
	if r.Code != 200 {
		t.Errorf("code %d", r.Code)
	}
	if l := <-lines; l != "HEAD /health HTTP/1.1|Host: app.lan|Connection: close|User-Agent: UA" {
		t.Errorf("HTTP/1.1 request: %q", l)
	}

	down := serve(t, respond("503 Service Unavailable"))
	r = check(t, Target{Type: TypeHTTP, Port: down})
	expect(t, r, L7STS, false, "503 Service Unavailable (expected 2xx)")
	if r.Code != 503 {
		t.Errorf("code %d", r.Code)
	}
	<-lines
	notFound := serve(t, respond("404 Not Found"))
	expect(t, check(t, Target{Type: TypeHTTP, Port: notFound, Expect: "200-299,404"}), L7OK, true, "404")
	<-lines

	silent := serve(t, func(c net.Conn) { io.Copy(io.Discard, c) })
	expect(t, check(t, Target{Type: TypeHTTP, Port: silent, Timeout: 100 * time.Millisecond}), L7TOUT, false, "timeout")
	garbage := serve(t, func(c net.Conn) { io.WriteString(c, "SSH-2.0-OpenSSH\r\n") })
	expect(t, check(t, Target{Type: TypeHTTP, Port: garbage}), L7RSP, false, "")
	closes := serve(t, func(c net.Conn) {})
	expect(t, check(t, Target{Type: TypeHTTP, Port: closes}), L7RSP, false, "")
}

func TestRedis(t *testing.T) {
	reply := func(s string) int {
		return serve(t, func(c net.Conn) {
			line, _ := bufio.NewReader(c).ReadString('\n')
			if line == "PING\r\n" {
				io.WriteString(c, s)
			}
		})
	}
	expect(t, check(t, Target{Type: TypeRedis, Port: reply("+PONG\r\n")}), L7OK, true, "PONG")
	expect(t, check(t, Target{Type: TypeRedis, Port: reply("-NOAUTH Authentication required.\r\n")}), L7STS, false, "NOAUTH")
}

func TestPgSQL(t *testing.T) {
	startup := make(chan []byte, 2)
	pg := func(reply []byte) int {
		return serve(t, func(c net.Conn) {
			var n [4]byte
			io.ReadFull(c, n[:])
			body := make([]byte, binary.BigEndian.Uint32(n[:])-4)
			io.ReadFull(c, body)
			startup <- body
			c.Write(reply)
		})
	}
	auth := []byte{'R', 0, 0, 0, 12, 0, 0, 0, 5, 1, 2, 3, 4}
	expect(t, check(t, Target{Type: TypePgSQL, Port: pg(auth)}), L7OK, true, "PostgreSQL server is ok")
	if b := <-startup; !bytes.Equal(b, []byte("\x00\x03\x00\x00user\x00relay\x00\x00")) {
		t.Errorf("startup message %q", b)
	}
	msg := "SFATAL\x00C28000\x00Mno pg_hba.conf entry\x00\x00"
	errResp := append([]byte{'E'}, binary.BigEndian.AppendUint32(nil, uint32(len(msg)+4))...)
	errResp = append(errResp, msg...)
	expect(t, check(t, Target{Type: TypePgSQL, Port: pg(errResp), User: "bob"}), L7RSP, false, "no pg_hba.conf entry")
	if b := <-startup; !bytes.Contains(b, []byte("user\x00bob\x00")) {
		t.Errorf("startup message %q", b)
	}
}

func TestMySQL(t *testing.T) {
	packet := func(payload []byte) []byte {
		n := len(payload)
		return append([]byte{byte(n), byte(n >> 8), byte(n >> 16), 0}, payload...)
	}
	greet := serve(t, func(c net.Conn) {
		c.Write(packet(append([]byte{0x0a}, "8.0.36\x00rest"...)))
	})
	expect(t, check(t, Target{Type: TypeMySQL, Port: greet}), L7OK, true, "8.0.36")
	denied := serve(t, func(c net.Conn) {
		c.Write(packet(append([]byte{0xff, 0x6a, 0x04}, "Host '10.0.0.9' is not allowed"...)))
	})
	expect(t, check(t, Target{Type: TypeMySQL, Port: denied}), L7RSP, false, "MySQL error 1130: Host '10.0.0.9' is not allowed")
}

func TestSendProxyAndTLS(t *testing.T) {
	cert, pool := selfSigned(t)
	hdr := make(chan []byte, 4)
	port := serve(t, func(c net.Conn) {
		b := make([]byte, len(ProxyV2Local))
		io.ReadFull(c, b)
		hdr <- b
		tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}})
		tc.Handshake()
		io.Copy(io.Discard, tc)
	})
	expect(t, check(t, Target{Port: port, SendProxy: true, TLS: true}), L6OK, true, "TLS handshake ok")
	if b := <-hdr; !bytes.Equal(b, ProxyV2Local) {
		t.Errorf("PROXY header %q", b)
	}
	expect(t, check(t, Target{Port: port, SendProxy: true, TLS: true, TLSVerify: true, RootCAs: x509.NewCertPool()}), L6RSP, false, "TLS handshake failed")
	<-hdr
	expect(t, check(t, Target{Port: port, SendProxy: true, TLS: true, TLSVerify: true, RootCAs: pool}), L6OK, true, "")
	<-hdr
}

func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "some-other-name"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA: true, BasicConstraintsValid: true, DNSNames: []string{"some-other-name"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(c)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func TestStatusMatches(t *testing.T) {
	cases := []struct {
		expect string
		code   int
		ok     bool
	}{
		{"", 200, true}, {"", 301, false}, {"200", 200, true}, {"200", 204, false}, {"2xx", 204, true},
		{"3xx", 302, true}, {"200-399", 301, true}, {"200,204", 204, true}, {"200,204", 201, false}, {"200-299,404", 404, true},
	}
	for _, c := range cases {
		if StatusMatches(c.expect, c.code) != c.ok {
			t.Errorf("%q %d", c.expect, c.code)
		}
	}
	for v, ok := range map[string]bool{"": true, "200": true, "2XX": true, "200-399": true, "200,204": true, "200-299,404": true, "20x": false, "600": false, "2xx,3xx": false} {
		if ValidExpect(v) != ok {
			t.Errorf("ValidExpect(%q)", v)
		}
	}
	if d, err := ParseDuration("2s"); err != nil || d != 2*time.Second {
		t.Error("ParseDuration")
	}
}
