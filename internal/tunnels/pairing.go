package tunnels

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/tunnel/pair"
)

// PairTokenTTL is how long a pairing token works.
const PairTokenTTL = 24 * time.Hour

// Pairing is what the user runs on the gateway host.
type Pairing struct {
	Token           string    `json:"token"`
	ExpiresAt       time.Time `json:"expiresAt"`
	HomeFingerprint string    `json:"homeFingerprint"`
	// Install is the command that installs and starts the gateway with the token.
	Install string `json:"install"`
	// Env is the token as an environment variable line (for an existing install).
	Env string `json:"env"`
	// Ports the gateway host must accept (firewall).
	Ports []string `json:"ports"`
}

// StartPairing creates a new one-time token for a gateway (replacing an older
// one) and returns the setup command. A paired gateway goes back to pending.
func (s *Service) StartPairing(ctx context.Context, id string) (*Pairing, error) {
	g, err := s.app.Store.Gateways().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	ident, err := s.identity(g.ID)
	if err != nil {
		return nil, fmt.Errorf("gateway identity: %w", err)
	}
	tok, err := pair.NewToken(PairTokenTTL)
	if err != nil {
		return nil, err
	}
	wasPaired := g.PairState == model.GatewayPaired
	exp := tok.Expires.UTC()
	g.PairState, g.GatewayPin, g.PairedAt = model.GatewayPending, "", nil
	g.PairToken, g.PairExpires, g.HomeFingerprint = tok.String(), &exp, ident.Fingerprint()
	if err := s.app.Store.Gateways().Update(ctx, g); err != nil {
		return nil, err
	}
	detail := "pairing token created"
	if wasPaired {
		detail = "re-pairing: the previous pairing no longer works"
		s.gatewaysChanged(ctx)
	}
	s.app.Audit(ctx, core.AuditEntry{Action: "gateway.pairing", Target: g.Name, Detail: detail, Result: "ok"})
	_, port, _ := model.SplitGatewayAddress(g.Address)
	return &Pairing{
		Token: tok.String(), ExpiresAt: exp, HomeFingerprint: ident.Fingerprint(),
		Install: installCommand(tok.String()),
		Env:     "RELAY_GATEWAY_PAIR_TOKEN=" + tok.String(),
		Ports:   []string{"80/tcp", "443/tcp", fmt.Sprintf("%d/tcp", port), fmt.Sprintf("%d/udp", port)},
	}, nil
}

func installCommand(token string) string {
	return strings.Join([]string{
		"git clone https://github.com/ollieInstantOffr/app-relay.git relay-gateway && cd relay-gateway",
		"RELAY_GATEWAY_PAIR_TOKEN=" + token + " docker compose -f deploy/gateway/docker-compose.yml up -d --build",
	}, "\n")
}

// Pair connects to a pending gateway and pairs with its token. The UI calls
// it repeatedly while the user starts the gateway.
func (s *Service) Pair(ctx context.Context, id string) (*model.Gateway, error) {
	g, err := s.app.Store.Gateways().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if g.PairState == model.GatewayPaired {
		return g, nil
	}
	if g.PairToken == "" {
		return nil, httpx.Errorf(http.StatusConflict, "no_pairing_token", "Create a pairing command for this gateway first.")
	}
	tok, err := pair.ParseToken(g.PairToken)
	if err != nil {
		return nil, fmt.Errorf("stored pairing token: %w", err)
	}
	if tok.Expired(s.now()) {
		return nil, httpx.Errorf(http.StatusGone, "token_expired", "The pairing token has expired. Create a new pairing command.")
	}
	ident, err := s.identity(g.ID)
	if err != nil {
		return nil, fmt.Errorf("gateway identity: %w", err)
	}
	addr := g.DialAddress()
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	d := net.Dialer{Timeout: 5 * time.Second}
	raw, err := d.DialContext(pctx, "tcp", addr)
	if err != nil {
		return nil, httpx.Errorf(http.StatusBadGateway, "gateway_unreachable",
			fmt.Sprintf("Can't reach the gateway at %s yet (%s). Check that it is running and TCP port %s is open.", addr, shortNetErr(err), portOf(addr)))
	}
	conn := tls.Client(raw, pair.ClientConfig(ident, "", pair.ALPN))
	defer conn.Close()
	pin, err := pair.Home(pctx, conn, tok)
	switch {
	case errors.Is(err, pair.ErrBadProof):
		s.app.Audit(ctx, core.AuditEntry{Action: "gateway.pair", Target: g.Name, Detail: "the gateway rejected the pairing token", Result: "failed"})
		return nil, httpx.Errorf(http.StatusConflict, "pairing_rejected",
			"The gateway rejected the pairing token. If it was paired before, run `relay gateway reset` on it (see the pairing instructions) and try again with a new command.")
	case errors.Is(err, pair.ErrExpired):
		return nil, httpx.Errorf(http.StatusGone, "token_expired", "The pairing token has expired. Create a new pairing command.")
	case err != nil:
		return nil, httpx.Errorf(http.StatusBadGateway, "pairing_failed",
			fmt.Sprintf("Pairing with %s failed: %s. Is the gateway running this Relay version's `relay gateway` with the pairing token?", addr, shortNetErr(err)))
	}
	now := s.now().UTC()
	g.PairState, g.GatewayPin, g.PairedAt = model.GatewayPaired, pin, &now
	g.PairToken, g.PairExpires, g.HomeFingerprint = "", nil, ident.Fingerprint()
	if err := s.app.Store.Gateways().Update(ctx, g); err != nil {
		return nil, err
	}
	s.app.Audit(ctx, core.AuditEntry{Action: "gateway.pair", Target: g.Name, Detail: "paired · gateway key " + pin, Result: "ok"})
	s.app.Activity(ctx, "tunnel.paired", "ok", fmt.Sprintf("Paired with gateway %s", g.Name), g.Name, addr)
	s.gatewaysChanged(ctx)
	return g, nil
}

func portOf(addr string) string {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Sprint(model.DefaultTunnelPort)
	}
	return p
}

// shortNetErr trims Go's operation prefixes from dial errors.
func shortNetErr(err error) string {
	var op *net.OpError
	if errors.As(err, &op) && op.Err != nil {
		err = op.Err
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"):
		return "connection refused"
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline"):
		return "timed out"
	case strings.Contains(msg, "no such host"):
		return "unknown host name"
	}
	return msg
}
