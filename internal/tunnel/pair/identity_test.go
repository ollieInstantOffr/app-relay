package pair

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

func TestIdentityPEMRoundTrip(t *testing.T) {
	id := mustIdentity(t, "home")
	certPEM, keyPEM, err := id.MarshalPEM()
	if err != nil {
		t.Fatal(err)
	}
	got, err := LoadIdentity(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Pub.Equal(id.Pub) || got.Fingerprint() != id.Fingerprint() {
		t.Fatal("loaded identity differs")
	}
	if !bytes.Equal(got.Cert.Certificate[0], id.Cert.Certificate[0]) {
		t.Fatal("certificate differs")
	}
	if got.Cert.Leaf.NotAfter.Before(time.Now().AddDate(19, 0, 0)) {
		t.Errorf("certificate expires %v, want ~20 years", got.Cert.Leaf.NotAfter)
	}

	// The loaded identity works for a real pinned handshake.
	gw := mustIdentity(t, "gateway")
	if cerr, serr := connect(t,
		ClientConfig(got, gw.Fingerprint(), tunnelALPN),
		ServerConfig(gw, fixedPin(id.Fingerprint()), tunnelALPN)); cerr != nil || serr != nil {
		t.Fatalf("handshake with loaded identity: client %v, server %v", cerr, serr)
	}
}

func TestLoadIdentityRejects(t *testing.T) {
	a, b := mustIdentity(t, "a"), mustIdentity(t, "b")
	certA, keyA, _ := a.MarshalPEM()
	_, keyB, _ := b.MarshalPEM()

	ec := ecdsaCert(t)
	ecCert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ec.Certificate[0]})
	ecKeyDER, err := x509.MarshalPKCS8PrivateKey(ec.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	ecKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecKeyDER})

	for name, in := range map[string][2][]byte{
		"mismatched key":      {certA, keyB},
		"swapped":             {keyA, certA},
		"garbage":             {[]byte("nope"), keyA},
		"two certificates":    {append(append([]byte{}, certA...), certA...), keyA},
		"ECDSA cert and key":  {ecCert, ecKey},
		"ECDSA cert, ed25519": {ecCert, keyA},
		"ed25519 cert, ECDSA": {certA, ecKey},
		"empty":               {nil, nil},
	} {
		if _, err := LoadIdentity(in[0], in[1]); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestFingerprintFormat(t *testing.T) {
	id := mustIdentity(t, "x")
	der, err := x509.MarshalPKIXPublicKey(id.Pub)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	want := "sha256:" + base64.RawURLEncoding.EncodeToString(sum[:])
	fp := id.Fingerprint()
	if fp != want || Fingerprint(id.Pub) != want {
		t.Fatalf("fingerprint %q, want %q", fp, want)
	}
	if len(fp) != 50 {
		t.Errorf("len = %d, want 50", len(fp))
	}
	if !ValidFingerprint(fp) {
		t.Error("ValidFingerprint rejected a fingerprint")
	}
	for _, bad := range []string{"", fp[7:], "sha1:" + fp[7:], fp + "A", fp[:49], strings.Replace(fp, fp[10:11], "+", 1)} {
		if ValidFingerprint(bad) {
			t.Errorf("ValidFingerprint(%q) = true", bad)
		}
	}
}

func TestTokenRoundTrip(t *testing.T) {
	tok, err := NewToken(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s := tok.String()
	if !strings.HasPrefix(s, "rlypair1_") || len(s) != 63 {
		t.Fatalf("token %q", s)
	}
	got, err := ParseToken(s)
	if err != nil {
		t.Fatal(err)
	}
	if got.Secret != tok.Secret || !got.Expires.Equal(tok.Expires) || got.Expires != tok.Expires {
		t.Fatalf("round trip: got %+v, want %+v", got, tok)
	}
	if d := time.Until(tok.Expires); d > time.Hour || d < 59*time.Minute {
		t.Errorf("expires in %v", d)
	}
	other, _ := NewToken(time.Hour)
	if other.Secret == tok.Secret {
		t.Error("secrets repeat")
	}
	if _, err := NewToken(0); err == nil {
		t.Error("NewToken(0) accepted")
	}
}

func TestTokenExpired(t *testing.T) {
	tok := Token{Expires: time.Unix(1000, 0)}
	if tok.Expired(time.Unix(999, 0)) {
		t.Error("expired before expiry")
	}
	if !tok.Expired(time.Unix(1000, 0)) || !tok.Expired(time.Unix(1001, 0)) {
		t.Error("not expired at or after expiry")
	}
}

func TestParseTokenStrict(t *testing.T) {
	tok, _ := NewToken(time.Hour)
	s := tok.String()
	body := s[len("rlypair1_"):]

	// Flip a padding bit in the last character: decodes loosely, but is not canonical.
	last := strings.IndexByte("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_", body[len(body)-1])
	nonCanonical := s[:len(s)-1] + string("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"[last|1])

	raw := make([]byte, 40)
	zeroExpiry := "rlypair1_" + base64.RawURLEncoding.EncodeToString(raw)
	raw[32] = 0x80
	hugeExpiry := "rlypair1_" + base64.RawURLEncoding.EncodeToString(raw)

	for name, in := range map[string]string{
		"empty":            "",
		"prefix only":      "rlypair1_",
		"no prefix":        body,
		"wrong prefix":     "rlypair2_" + body,
		"upper prefix":     "RLYPAIR1_" + body,
		"short":            s[:len(s)-1],
		"long":             s + "A",
		"padded":           s + "=",
		"std alphabet":     "rlypair1_" + strings.NewReplacer("-", "+", "_", "/").Replace(body[:len(body)-1]) + "+",
		"leading space":    " " + s,
		"trailing newline": s + "\n",
		"non-canonical":    nonCanonical,
		"zero expiry":      zeroExpiry,
		"huge expiry":      hugeExpiry,
	} {
		if _, err := ParseToken(in); err == nil {
			t.Errorf("%s: %q accepted", name, in)
		}
	}
}
