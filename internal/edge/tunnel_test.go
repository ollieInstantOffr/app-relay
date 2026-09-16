package edge

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/proxyproto"
)

// tunnelSocketDir returns a short directory for unix sockets (path length limit).
func tunnelSocketDir(t *testing.T) string {
	dir, err := os.MkdirTemp("/tmp", "edgetun")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// tunnelClient returns an HTTP client that reaches socket through a PROXY v2
// header claiming src and gateway gw.
func tunnelClient(socket, src, gw string, tlsOn bool) *http.Client {
	dial := func(ctx context.Context) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
		if err != nil {
			return nil, err
		}
		hdr := proxyproto.AppendV2(nil, netip.MustParseAddrPort(src), netip.MustParseAddrPort("198.51.100.1:443"),
			proxyproto.TLV{Type: proxyproto.TLVRelayTunnel, Value: []byte(gw)})
		if _, err := c.Write(hdr); err != nil {
			c.Close()
			return nil, err
		}
		return c, nil
	}
	tr := &http.Transport{DisableKeepAlives: true}
	if tlsOn {
		tr.DialTLSContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
			c, err := dial(ctx)
			if err != nil {
				return nil, err
			}
			host, _, _ := net.SplitHostPort(addr)
			tc := tls.Client(c, &tls.Config{ServerName: host, InsecureSkipVerify: true})
			if err := tc.HandshakeContext(ctx); err != nil {
				c.Close()
				return nil, err
			}
			return tc, nil
		}
	} else {
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) }
	}
	return &http.Client{Transport: tr, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func TestTunnelIngress(t *testing.T) {
	cfg, dir := newConfig(t)
	socks := tunnelSocketDir(t)
	cfg.HTTPSPort = freePort(t, "tcp")
	cfg.HTTP3 = true
	cfg.Tunnel = &TunnelIngress{HTTPSocket: filepath.Join(socks, "edge-http.sock"), HTTPSSocket: filepath.Join(socks, "edge-https.sock")}
	cfg.AccessLists = map[string]AccessList{"lan": {Name: "lan", Rules: []IPRule{{Allow: true, CIDR: "192.168.0.0/16"}, {Allow: false, CIDR: "all"}}}}
	up := newUpstream(t, nil)

	pub := proxyHost("pub", []string{"pub.test"}, up.ref())
	pub.Tunnel = true
	pub.Cert = writeCert(t, dir, "pub", "pub.test")
	pub.HTTP3 = true
	lan := proxyHost("lan", []string{"lan.test"}, up.ref())
	lan.Tunnel = true
	lan.Locations[0].AccessListID = "lan"
	secret := proxyHost("secret", []string{"secret.test"}, up.ref())
	secret.Cert = writeCert(t, dir, "secret", "secret.test")
	redirect := proxyHost("redir", []string{"redir.test"}, up.ref())
	redirect.Tunnel = true
	redirect.Cert = writeCert(t, dir, "redir", "redir.test")
	redirect.ForceHTTPS = true
	cfg.Hosts = []Host{pub, lan, secret, redirect}
	e := startEnv(t, cfg, dir)

	get := func(c *http.Client, url string) (*http.Response, string, error) {
		res, err := c.Get(url)
		if err != nil {
			return nil, "", err
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res, string(b), nil
	}

	// Published HTTPS host: client address from the header, public port, no Alt-Svc.
	httpsC := tunnelClient(cfg.Tunnel.HTTPSSocket, "203.0.113.9:40000", "gw1", true)
	res, body, err := get(httpsC, "https://pub.test/x")
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("pub: %d %s", res.StatusCode, body)
	}
	if v, _ := field(body, "x-real-ip"); v != "203.0.113.9" {
		t.Errorf("X-Real-IP %q", v)
	}
	if v, _ := field(body, "x-forwarded-port"); v != "443" {
		t.Errorf("X-Forwarded-Port %q", v)
	}
	if res.Header.Get("Alt-Svc") != "" {
		t.Errorf("Alt-Svc through tunnel: %q", res.Header.Get("Alt-Svc"))
	}
	// The same host directly still announces HTTP/3.
	if d := e.get("https://pub.test/x", "Host", "pub.test"); d.Header.Get("Alt-Svc") == "" {
		t.Error("Alt-Svc missing on the direct listener")
	}

	// A name that is not published fails the TLS handshake.
	if _, _, err := get(httpsC, "https://secret.test/"); err == nil {
		t.Error("unpublished SNI accepted through the tunnel")
	}
	// Published SNI but the Host header of an unpublished host: refused.
	req, _ := http.NewRequest("GET", "https://pub.test/", nil)
	req.Host = "secret.test"
	if res, err := httpsC.Do(req); err == nil {
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if strings.Contains(string(b), "uri=") {
			t.Errorf("unpublished Host served through the tunnel: %d %q", res.StatusCode, b)
		}
	}

	// Access lists see the client address from the header.
	httpC := tunnelClient(cfg.Tunnel.HTTPSocket, "203.0.113.9:40000", "gw1", false)
	if res, _, err := get(httpC, "http://lan.test/"); err != nil || res.StatusCode != 403 {
		t.Errorf("lan from internet: %v %v", res, err)
	}
	lanC := tunnelClient(cfg.Tunnel.HTTPSocket, "192.168.1.5:40000", "gw1", false)
	if res, _, err := get(lanC, "http://lan.test/"); err != nil || res.StatusCode != 200 {
		t.Errorf("lan from lan: %v %v", res, err)
	}
	// Unpublished host over HTTP: the connection is closed (444).
	if res, _, err := get(httpC, "http://secret.test/"); err == nil {
		t.Errorf("unpublished host over HTTP: %d", res.StatusCode)
	}
	// ForceHTTPS redirects to the gateway's port 443 even with a custom HTTPS port.
	if res, _, err := get(httpC, "http://redir.test/a?b"); err != nil || res.Header.Get("Location") != "https://redir.test/a?b" {
		t.Errorf("redirect through tunnel: %v %v", res, err)
	}

	// Access log records the gateway and the real client.
	waitFor(t, 3*time.Second, func() bool {
		data, _ := os.ReadFile(filepath.Join(cfg.LogDir, "access.log"))
		return strings.Contains(string(data), `"remote_addr":"192.168.1.5"`)
	})
	data, _ := os.ReadFile(filepath.Join(cfg.LogDir, "access.log"))
	if !strings.Contains(string(data), `"remote_addr":"203.0.113.9"`) || !strings.Contains(string(data), `"tunnel":"gw1"`) {
		t.Errorf("access log:\n%s", data)
	}

	// Missing PROXY header: the connection is dropped.
	c, err := net.Dial("unix", cfg.Tunnel.HTTPSocket)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: lan.test\r\n\r\n")
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if b, _ := io.ReadAll(c); len(b) != 0 {
		t.Errorf("request without PROXY header answered: %q", b)
	}
	c.Close()

	// Reload keeps the sockets; removing the ingress closes them.
	if err := e.reload(); err != nil {
		t.Fatal(err)
	}
	if res, _, err := get(lanC, "http://lan.test/"); err != nil || res.StatusCode != 200 {
		t.Errorf("after reload: %v %v", res, err)
	}
	e.cfg.Tunnel = nil
	if err := e.reload(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		_, err := net.Dial("unix", filepath.Join(socks, "edge-http.sock"))
		return err != nil
	})
	if c, err := net.Dial("unix", filepath.Join(socks, "edge-http.sock")); err == nil {
		c.Close()
		t.Error("tunnel socket still accepting after removal")
	}
}

func TestTunnelStream(t *testing.T) {
	cfg, dir := newConfig(t)
	socks := tunnelSocketDir(t)
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				h, err := proxyproto.Read(br)
				if err != nil {
					return
				}
				line, _ := br.ReadString('\n')
				fmt.Fprintf(c, "%s %s", h.Src.Addr(), line)
			}()
		}
	}()
	up := echo.Addr().(*net.TCPAddr)
	listen := freePort(t, "tcp")
	sock := filepath.Join(socks, "edge-stream.sock")
	cfg.Streams = []Stream{{ID: "s1", Name: "echo", TCP: true, ListenAddr: "127.0.0.1", ListenLo: listen, ListenHi: listen,
		ForwardHost: "127.0.0.1", ForwardLo: up.Port, ForwardHi: up.Port, ProxyProtocol: true,
		TunnelSockets: []TunnelSocket{{Port: listen, Socket: sock}}}}
	startEnv(t, cfg, dir)

	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write(proxyproto.AppendV2(nil, netip.MustParseAddrPort("203.0.113.7:1234"), netip.MustParseAddrPort("198.51.100.1:25565"),
		proxyproto.TLV{Type: proxyproto.TLVRelayTunnel, Value: []byte("gw1")}))
	io.WriteString(c, "hello\n")
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || line != "203.0.113.7 hello\n" {
		t.Fatalf("stream through tunnel: %q %v", line, err)
	}
	c.Close()
	waitFor(t, 3*time.Second, func() bool {
		data, _ := os.ReadFile(filepath.Join(cfg.LogDir, "stream-access.log"))
		return strings.Contains(string(data), `"tunnel":"gw1"`)
	})
	data, _ := os.ReadFile(filepath.Join(cfg.LogDir, "stream-access.log"))
	if !strings.Contains(string(data), `"remote_addr":"203.0.113.7"`) || !strings.Contains(string(data), `"tunnel":"gw1"`) {
		t.Errorf("stream log:\n%s", data)
	}

	// A tunnel socket outside the listen ports is rejected.
	cfg2, dir2 := newConfig(t)
	cfg2.Streams = []Stream{{ID: "s", TCP: true, ListenLo: 1000, ListenHi: 1000, ForwardHost: "127.0.0.1", ForwardLo: 1, ForwardHi: 1,
		TunnelSockets: []TunnelSocket{{Port: 2000, Socket: filepath.Join(socks, "x.sock")}}}}
	if _, err := compile(cfg2, dir2, compileEnv{}); err == nil {
		t.Error("tunnel socket outside listen ports accepted")
	}
}
