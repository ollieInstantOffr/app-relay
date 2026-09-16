package pair

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"
)

func ecdsaCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "ecdsa"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestServerConfigUnpairedOnlyAllowsPairingALPN(t *testing.T) {
	home, gw := mustIdentity(t, "home"), mustIdentity(t, "gateway")
	server := func() *tls.Config { return ServerConfig(gw, noPin, ALPN, tunnelALPN) }

	cerr, serr := connect(t, ClientConfig(home, gw.Fingerprint(), tunnelALPN), server())
	if serr == nil || cerr == nil {
		t.Fatalf("unpaired client reached %s: client %v, server %v", tunnelALPN, cerr, serr)
	}
	if cerr, serr := connect(t, ClientConfig(home, "", ALPN), server()); cerr != nil || serr != nil {
		t.Fatalf("pairing ALPN refused: client %v, server %v", cerr, serr)
	}

	// A client offering no ALPN at all is refused too.
	cfg := ClientConfig(home, gw.Fingerprint(), tunnelALPN)
	cfg.NextProtos = nil
	if _, serr := connect(t, cfg, server()); serr == nil {
		t.Fatal("server accepted a client without ALPN")
	}
}

func TestClientConfigPanicsWithoutPinOutsidePairing(t *testing.T) {
	id := mustIdentity(t, "home")
	for name, fn := range map[string]func(){
		"tunnel ALPN":         func() { ClientConfig(id, "", tunnelALPN) },
		"pairing plus tunnel": func() { ClientConfig(id, "", ALPN, tunnelALPN) },
		"no ALPN":             func() { ClientConfig(id, id.Fingerprint()) },
		"server nil pin":      func() { ServerConfig(id, nil, ALPN) },
		"server no ALPN":      func() { ServerConfig(id, noPin) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("did not panic")
				}
			}()
			fn()
		})
	}
}

func TestConfigsRequireEd25519AndTLS13(t *testing.T) {
	home, gw := mustIdentity(t, "home"), mustIdentity(t, "gateway")

	t.Run("server with ECDSA cert", func(t *testing.T) {
		srv := ServerConfig(gw, noPin, ALPN)
		srv.Certificates = []tls.Certificate{ecdsaCert(t)}
		if cerr, _ := connect(t, ClientConfig(home, "", ALPN), srv); cerr == nil {
			t.Fatal("client accepted an ECDSA server certificate")
		}
	})
	t.Run("client with ECDSA cert", func(t *testing.T) {
		cli := ClientConfig(home, "", ALPN)
		cert := ecdsaCert(t)
		cli.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &cert, nil }
		if _, serr := connect(t, cli, ServerConfig(gw, noPin, ALPN)); serr == nil {
			t.Fatal("server accepted an ECDSA client certificate")
		}
	})
	t.Run("client without cert", func(t *testing.T) {
		cli := ClientConfig(home, "", ALPN)
		cli.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &tls.Certificate{}, nil }
		if _, serr := connect(t, cli, ServerConfig(gw, noPin, ALPN)); serr == nil {
			t.Fatal("server accepted a client without a certificate")
		}
	})
	t.Run("TLS 1.2 client", func(t *testing.T) {
		cli := ClientConfig(home, "", ALPN)
		cli.MinVersion, cli.MaxVersion = tls.VersionTLS12, tls.VersionTLS12
		if _, serr := connect(t, cli, ServerConfig(gw, noPin, ALPN)); serr == nil {
			t.Fatal("server accepted TLS 1.2")
		}
	})
}

func TestPeerFingerprint(t *testing.T) {
	home, gw := mustIdentity(t, "home"), mustIdentity(t, "gateway")
	client, server, cerr, serr := handshake(t, ClientConfig(home, "", ALPN), ServerConfig(gw, noPin, ALPN))
	if cerr != nil || serr != nil {
		t.Fatal(cerr, serr)
	}
	if fp, err := PeerFingerprint(client.ConnectionState()); err != nil || fp != gw.Fingerprint() {
		t.Errorf("client sees %q, %v; want %q", fp, err, gw.Fingerprint())
	}
	if fp, err := PeerFingerprint(server.ConnectionState()); err != nil || fp != home.Fingerprint() {
		t.Errorf("server sees %q, %v; want %q", fp, err, home.Fingerprint())
	}
	if _, err := PeerFingerprint(tls.ConnectionState{}); err == nil {
		t.Error("no peer certificate accepted")
	}
}
