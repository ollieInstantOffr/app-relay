package edge

import (
	"crypto/tls"
	"fmt"
)

// tlsProfile mirrors the Mozilla profiles of the nginx renderer. Go cannot
// offer DHE suites or configure TLS 1.3 suites (all of them are secure), so
// the lists are the nginx lists minus what Go does not implement. Go's
// default key exchange preferences (X25519MLKEM768, X25519, P-256, P-384)
// are kept: they are a superset of nginx's ssl_ecdh_curve with post-quantum
// hybrids on top.
type tlsProfile struct {
	name       string
	minVersion uint16
	ciphers    []uint16 // nil = Go defaults (TLS 1.3 only profiles)
}

var tlsProfiles = map[string]*tlsProfile{
	"modern": {name: "modern", minVersion: tls.VersionTLS13},
	"intermediate": {name: "intermediate", minVersion: tls.VersionTLS12, ciphers: []uint16{
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
		tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
	}},
	"old": {name: "old", minVersion: tls.VersionTLS10, ciphers: []uint16{
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
		tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
		tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
		tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
		tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_RSA_WITH_AES_128_CBC_SHA256,
		tls.TLS_RSA_WITH_AES_128_CBC_SHA,
		tls.TLS_RSA_WITH_AES_256_CBC_SHA,
		tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,
	}},
}

// profileFor resolves a host profile, falling back to the global one and
// then to intermediate (the renderer's cipherProfile rule).
func profileFor(names ...string) (*tlsProfile, error) {
	for _, n := range names {
		if n == "" {
			continue
		}
		p, ok := tlsProfiles[n]
		if !ok {
			return nil, fmt.Errorf("unknown TLS profile %q", n)
		}
		return p, nil
	}
	return tlsProfiles["intermediate"], nil
}

var (
	alpnH2   = []string{"h2", "http/1.1"}
	alpnHTTP = []string{"http/1.1"}
)

// handshakeConfig returns the per-server TLS configuration returned from
// GetConfigForClient. Session ticket keys are inherited from the listener's
// base config, so resumption works across servers.
func handshakeConfig(p *tlsProfile, http2 bool, cert func() *tls.Certificate) *tls.Config {
	c := &tls.Config{
		MinVersion:   p.minVersion,
		CipherSuites: p.ciphers,
		NextProtos:   alpnHTTP,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return cert(), nil
		},
	}
	if http2 {
		c.NextProtos = alpnH2
	}
	return c
}

func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS13:
		return "TLSv1.3"
	case tls.VersionTLS12:
		return "TLSv1.2"
	case tls.VersionTLS11:
		return "TLSv1.1"
	case tls.VersionTLS10:
		return "TLSv1"
	}
	return ""
}
