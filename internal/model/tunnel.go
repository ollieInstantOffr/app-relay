package model

import (
	"net"
	"strconv"
	"strings"
	"time"
)

// KindGateway is the table of tunnel gateways.
const KindGateway = "gateways"

// Gateway transports.
const (
	TransportAuto = "auto" // QUIC, falling back to HTTP/2 over TCP
	TransportQUIC = "quic"
	TransportTCP  = "tcp"
)

// Gateway pairing states.
const (
	GatewayPending = "pending" // waiting for the gateway to pair with the token
	GatewayPaired  = "paired"
)

// DefaultTunnelPort is the gateway's tunnel listener (UDP for QUIC, TCP for
// HTTP/2 and pairing).
const DefaultTunnelPort = 7443

// Gateway is a public tunnel gateway this Relay dials out to. Hosts and TCP
// streams reference it with TunnelGatewayID.
//
// Gateways are runtime state like certificates: saving one does not create a
// pending change; the tunnel engine picks changes up immediately. Publishing a
// host or stream through a gateway is a normal (pending) config change.
type Gateway struct {
	Meta
	Name      string `json:"name"`
	Address   string `json:"address"`   // host[:port] of the tunnel listener; port defaults to 7443
	Transport string `json:"transport"` // auto | quic | tcp
	Enabled   bool   `json:"enabled"`

	PairState string `json:"pairState"` // pending | paired
	// GatewayPin is the gateway key fingerprint, pinned when pairing succeeded.
	GatewayPin string `json:"gatewayPin,omitempty"`
	// HomeFingerprint is this Relay's key for the gateway (the gateway pins it).
	HomeFingerprint string     `json:"homeFingerprint,omitempty"`
	PairedAt        *time.Time `json:"pairedAt,omitempty"`
	// PairToken is the one-time pairing token while pending. Write-only for
	// the API: it is shown once when created and cleared after pairing.
	PairToken string `json:"pairToken,omitempty"`
	// PairExpires is when PairToken stops working.
	PairExpires *time.Time `json:"pairExpires,omitempty"`

	// Reported by the gateway over the tunnel.
	PublicIPs []string `json:"publicIps,omitempty"`
	Version   string   `json:"version,omitempty"`
}

// Redact hides the pairing token.
func (g *Gateway) Redact() {
	g.PairToken = ""
	if g.PublicIPs == nil {
		g.PublicIPs = []string{}
	}
}

// KeepSecrets keeps the server-managed fields: pairing state, pins, token and
// what the gateway reported can't be set through the API.
func (g *Gateway) KeepSecrets(prev any) error {
	g.Name = strings.TrimSpace(g.Name)
	g.Address = strings.TrimSpace(g.Address)
	if g.Transport == "" {
		g.Transport = TransportAuto
	}
	p, _ := prev.(*Gateway)
	if p == nil {
		g.PairState, g.GatewayPin, g.HomeFingerprint, g.PairedAt = GatewayPending, "", "", nil
		g.PairToken, g.PairExpires, g.PublicIPs, g.Version = "", nil, nil, ""
		return nil
	}
	g.PairState, g.GatewayPin, g.HomeFingerprint, g.PairedAt = p.PairState, p.GatewayPin, p.HomeFingerprint, p.PairedAt
	g.PairToken, g.PairExpires, g.PublicIPs, g.Version = p.PairToken, p.PairExpires, p.PublicIPs, p.Version
	return nil
}

func (g *Gateway) Validate() error {
	e := Errs{}
	switch {
	case g.Name == "":
		e.Add("name", "Name is required")
	case len(g.Name) > 64:
		e.Add("name", "At most 64 characters")
	}
	if _, _, err := SplitGatewayAddress(g.Address); err != nil {
		e.Add("address", "%s", err.Error())
	}
	switch g.Transport {
	case TransportAuto, TransportQUIC, TransportTCP:
	default:
		e.Add("transport", "Transport must be auto, quic or tcp")
	}
	return e.Err()
}

type addrError string

func (e addrError) Error() string { return string(e) }

// SplitGatewayAddress parses "host", "host:port" or "[v6]:port" (port
// defaults to DefaultTunnelPort).
func SplitGatewayAddress(addr string) (host string, port int, err error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", 0, addrError("Address is required")
	}
	if strings.Contains(addr, "://") || strings.ContainsAny(addr, "/ ") {
		return "", 0, addrError("Enter a host name or IP address, optionally with :port")
	}
	host, portStr, splitErr := net.SplitHostPort(addr)
	if splitErr != nil {
		host, portStr = strings.Trim(addr, "[]"), ""
	}
	port = DefaultTunnelPort
	if portStr != "" {
		p, err := strconv.Atoi(portStr)
		if err != nil || p < 1 || p > 65535 {
			return "", 0, addrError("Port must be between 1 and 65535")
		}
		port = p
	}
	if host == "" {
		return "", 0, addrError("Address is required")
	}
	if net.ParseIP(host) == nil && !validHostname(host) {
		return "", 0, addrError("Enter a valid host name or IP address")
	}
	return host, port, nil
}

func validHostname(h string) bool {
	if len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(h, "."), ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// DialAddress returns host:port for dialing.
func (g *Gateway) DialAddress() string {
	host, port, err := SplitGatewayAddress(g.Address)
	if err != nil {
		return g.Address
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}
