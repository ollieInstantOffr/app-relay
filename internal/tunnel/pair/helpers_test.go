package pair

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"
)

const tunnelALPN = "relay-tunnel-h2/1"

func noPin() string { return "" }

func fixedPin(p string) func() string { return func() string { return p } }

func mustIdentity(t *testing.T, cn string) Identity {
	t.Helper()
	id, err := NewIdentity(cn)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustToken(t *testing.T) Token {
	t.Helper()
	tok, err := NewToken(10 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

// handshake connects a TLS client and server over loopback TCP and runs both
// handshakes to completion.
func handshake(t *testing.T, clientCfg, serverCfg *tls.Config) (client, server *tls.Conn, clientErr, serverErr error) {
	t.Helper()
	ctx := testCtx(t)
	ln := listen(t)
	type result struct {
		conn *tls.Conn
		err  error
	}
	srv := make(chan result, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			srv <- result{err: err}
			return
		}
		tc := tls.Server(c, serverCfg)
		srv <- result{tc, tc.HandshakeContext(ctx)}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client = tls.Client(c, clientCfg)
	clientErr = client.HandshakeContext(ctx)
	if clientErr != nil {
		client.Close() // unblock the server if it is still waiting
	}
	r := <-srv
	server, serverErr = r.conn, r.err
	t.Cleanup(func() {
		client.Close()
		if server != nil {
			server.Close()
		}
	})
	return client, server, clientErr, serverErr
}

// connect handshakes and then exchanges a byte each way, which surfaces a
// server-side rejection of the client certificate on the client too.
func connect(t *testing.T, clientCfg, serverCfg *tls.Config) (clientErr, serverErr error) {
	t.Helper()
	client, server, clientErr, serverErr := handshake(t, clientCfg, serverCfg)
	if serverErr != nil {
		if clientErr == nil {
			_, clientErr = io.ReadFull(client, make([]byte, 1))
		}
		return clientErr, serverErr
	}
	if clientErr != nil {
		return clientErr, serverErr
	}
	done := make(chan error, 1)
	go func() {
		if _, err := server.Write([]byte{'s'}); err != nil {
			done <- err
			return
		}
		_, err := io.ReadFull(server, make([]byte, 1))
		done <- err
	}()
	if _, err := io.ReadFull(client, make([]byte, 1)); err != nil {
		clientErr = err
	} else {
		_, clientErr = client.Write([]byte{'c'})
	}
	if clientErr != nil {
		client.Close()
	}
	return clientErr, <-done
}

type pairResult struct {
	homePin, gatewayPin string
	homeErr, gatewayErr error
}

// runPair handshakes with the pairing configs and runs Home and Gateway
// concurrently, closing each connection when its side returns.
func runPair(t *testing.T, home, gw Identity, homeTok, gwTok Token) pairResult {
	t.Helper()
	client, server, cerr, serr := handshake(t, ClientConfig(home, "", ALPN), ServerConfig(gw, noPin, ALPN))
	if cerr != nil || serr != nil {
		t.Fatalf("pairing handshake: client %v, server %v", cerr, serr)
	}
	ctx := testCtx(t)
	var r pairResult
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.gatewayPin, r.gatewayErr = Gateway(ctx, server, gwTok)
		server.Close()
	}()
	r.homePin, r.homeErr = Home(ctx, client, homeTok)
	client.Close()
	<-done
	return r
}
