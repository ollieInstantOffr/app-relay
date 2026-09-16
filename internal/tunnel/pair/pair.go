package pair

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

var (
	// ErrBadProof means the peer did not prove knowledge of the token for this
	// connection: wrong secret, wrong expiry, a replayed or reflected proof, or
	// a man in the middle.
	ErrBadProof = errors.New("pair: pairing proof rejected")
	// ErrExpired means the local token has expired.
	ErrExpired = errors.New("pair: pairing token expired")
)

// Wire format, all fixed length:
//
//	home → gateway: version(1) || expiry(8, BE) || proofHome(32)
//	gateway → home: statusFail(1)  |  statusOK(1) || proofGateway(32)
//	home → gateway: statusOK(1) (confirmed) or statusFail(1)
//
// transcript   = "relay-pair/1" || EKM(32) || SHA-256(gateway SPKI) || SHA-256(home SPKI) || expiry(8, BE)
// proofHome    = HMAC-SHA256(secret, "home->gateway" || transcript)
// proofGateway = HMAC-SHA256(secret, "gateway->home" || transcript)
const (
	exporterLabel = "EXPORTER-relay-pair-v1"
	labelHome     = "home->gateway"
	labelGateway  = "gateway->home"

	msgVersion byte = 1
	statusFail byte = 0
	statusOK   byte = 1

	homeMsgLen = 1 + 8 + sha256.Size
)

// session is what both sides derive from the completed TLS handshake.
type session struct {
	peerPin     string
	ekm         []byte
	local, peer [sha256.Size]byte
}

// Home pairs with a gateway over conn, where Home is the TLS client and conn
// negotiated ALPN (see ClientConfig with pin ""). On success it returns the
// gateway's fingerprint to pin; the token must then be discarded. The context
// bounds the whole exchange: its deadline is applied to conn and cancelling it
// closes conn.
func Home(ctx context.Context, conn *tls.Conn, tok Token) (gatewayPin string, err error) {
	return withConn(ctx, conn, func() (string, error) {
		if tok.Expired(time.Now()) {
			return "", ErrExpired
		}
		s, err := prepare(ctx, conn)
		if err != nil {
			return "", err
		}
		exp := tok.expiry()
		tr := transcript(s.ekm, s.peer, s.local, exp)

		msg := make([]byte, 0, homeMsgLen)
		msg = append(msg, msgVersion)
		msg = binary.BigEndian.AppendUint64(msg, exp)
		msg = append(msg, proof(tok.Secret, labelHome, tr)...)
		if _, err := conn.Write(msg); err != nil {
			return "", fmt.Errorf("pair: send proof: %w", err)
		}

		var reply [1 + sha256.Size]byte
		if _, err := io.ReadFull(conn, reply[:1]); err != nil {
			return "", fmt.Errorf("pair: read gateway reply: %w", err)
		}
		if reply[0] != statusOK {
			return "", fmt.Errorf("%w: gateway rejected the token", ErrBadProof)
		}
		if _, err := io.ReadFull(conn, reply[1:]); err != nil {
			return "", fmt.Errorf("pair: read gateway proof: %w", err)
		}
		if !hmac.Equal(reply[1:], proof(tok.Secret, labelGateway, tr)) {
			_, _ = conn.Write([]byte{statusFail})
			return "", fmt.Errorf("%w: gateway did not prove the token", ErrBadProof)
		}
		if _, err := conn.Write([]byte{statusOK}); err != nil {
			return "", fmt.Errorf("pair: send confirmation: %w", err)
		}
		return s.peerPin, nil
	})
}

// Gateway pairs with a home over conn, where Gateway is the TLS server (see
// ServerConfig with pin returning ""). It commits only after the home has
// verified the gateway's proof, and reveals nothing about why a proof failed.
// On success it returns the home's fingerprint to pin; the token must then be
// discarded. Callers should count any error against the peer with Limiter.Failed.
func Gateway(ctx context.Context, conn *tls.Conn, tok Token) (homePin string, err error) {
	return withConn(ctx, conn, func() (string, error) {
		s, err := prepare(ctx, conn)
		if err != nil {
			return "", err
		}
		var msg [homeMsgLen]byte
		if _, err := io.ReadFull(conn, msg[:]); err != nil {
			return "", fmt.Errorf("pair: read home proof: %w", err)
		}
		fail := func(err error) (string, error) {
			_, _ = conn.Write([]byte{statusFail})
			return "", err
		}
		if tok.Expired(time.Now()) {
			return fail(ErrExpired)
		}
		exp := tok.expiry()
		tr := transcript(s.ekm, s.local, s.peer, exp)
		ok := hmac.Equal(msg[9:], proof(tok.Secret, labelHome, tr))
		ok = ok && msg[0] == msgVersion && binary.BigEndian.Uint64(msg[1:9]) == exp
		if !ok {
			return fail(ErrBadProof)
		}

		reply := append([]byte{statusOK}, proof(tok.Secret, labelGateway, tr)...)
		if _, err := conn.Write(reply); err != nil {
			return "", fmt.Errorf("pair: send proof: %w", err)
		}
		var ack [1]byte
		if _, err := io.ReadFull(conn, ack[:]); err != nil {
			return "", fmt.Errorf("pair: read home confirmation: %w", err)
		}
		if ack[0] != statusOK {
			return "", fmt.Errorf("%w: home rejected the gateway", ErrBadProof)
		}
		return s.peerPin, nil
	})
}

// prepare completes the handshake and checks the pairing preconditions.
func prepare(ctx context.Context, conn *tls.Conn) (session, error) {
	if err := conn.HandshakeContext(ctx); err != nil {
		return session{}, fmt.Errorf("pair: handshake: %w", err)
	}
	cs := conn.ConnectionState()
	switch {
	case cs.Version != tls.VersionTLS13:
		return session{}, errors.New("pair: TLS 1.3 required")
	case cs.NegotiatedProtocol != ALPN:
		return session{}, fmt.Errorf("pair: negotiated ALPN %q, want %q", cs.NegotiatedProtocol, ALPN)
	case cs.DidResume:
		return session{}, errors.New("pair: resumed sessions cannot pair")
	case len(cs.LocalCertificate) != 1:
		return session{}, errors.New("pair: local side presented no single certificate")
	}
	peerPin, err := PeerFingerprint(cs)
	if err != nil {
		return session{}, err
	}
	peerKey, err := leafKey(cs.PeerCertificates[0])
	if err != nil {
		return session{}, err
	}
	localKey, err := certKey(cs.LocalCertificate[0])
	if err != nil {
		return session{}, fmt.Errorf("pair: local certificate: %w", err)
	}
	if localKey.Equal(peerKey) {
		return session{}, errors.New("pair: peer presented our own key")
	}
	ekm, err := cs.ExportKeyingMaterial(exporterLabel, nil, 32)
	if err != nil {
		return session{}, fmt.Errorf("pair: export keying material: %w", err)
	}
	return session{peerPin: peerPin, ekm: ekm, local: spkiHash(localKey), peer: spkiHash(peerKey)}, nil
}

func transcript(ekm []byte, gatewaySPKI, homeSPKI [sha256.Size]byte, expiry uint64) []byte {
	b := make([]byte, 0, len(ALPN)+len(ekm)+2*sha256.Size+8)
	b = append(b, ALPN...)
	b = append(b, ekm...)
	b = append(b, gatewaySPKI[:]...)
	b = append(b, homeSPKI[:]...)
	return binary.BigEndian.AppendUint64(b, expiry)
}

func proof(secret [32]byte, label string, transcript []byte) []byte {
	m := hmac.New(sha256.New, secret[:])
	m.Write([]byte(label))
	m.Write(transcript)
	return m.Sum(nil)
}

// withConn applies ctx to conn for the duration of fn.
func withConn(ctx context.Context, conn *tls.Conn, fn func() (string, error)) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	d, hasDeadline := ctx.Deadline()
	if hasDeadline {
		if err := conn.SetDeadline(d); err != nil {
			return "", err
		}
		defer conn.SetDeadline(time.Time{})
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	pin, err := fn()
	if err == nil {
		return pin, nil
	}
	if hasDeadline && errors.Is(err, os.ErrDeadlineExceeded) {
		<-ctx.Done() // the conn deadline is the context's, so this is imminent
	}
	if ctx.Err() != nil {
		return "", fmt.Errorf("pair: %w", ctx.Err())
	}
	return "", err
}
