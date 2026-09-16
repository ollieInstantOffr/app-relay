package render

import (
	"fmt"
	"path/filepath"

	"github.com/instantoffr/relay/internal/model"
)

// Tunnel ingress: the tunnel engine hands connections that arrived through a
// gateway to the active proxy engine over unix sockets in <RunDir>/tunnel.
// The proxy engine requires a PROXY protocol v2 header on them (written by the
// tunnel engine, never by the gateway) and serves only hosts and streams
// published through a tunnel there. Socket names carry the engine so nginx and
// Relay Edge never remove each other's sockets during an engine switch.

// TunnelDir is the directory holding the tunnel ingress sockets.
func (e Env) TunnelDir() string {
	dir := e.RunDir
	if dir == "" {
		dir = "/run/relay"
	}
	return filepath.Join(dir, "tunnel")
}

// TunnelHTTPSocket is where engine accepts plain HTTP from the tunnel.
func (e Env) TunnelHTTPSocket(engine string) string {
	return filepath.Join(e.TunnelDir(), engine+"-http.sock")
}

// TunnelHTTPSSocket is where engine accepts TLS from the tunnel.
func (e Env) TunnelHTTPSSocket(engine string) string {
	return filepath.Join(e.TunnelDir(), engine+"-https.sock")
}

// TunnelStreamSocket is where engine accepts a TCP stream's public port from the tunnel.
func (e Env) TunnelStreamSocket(engine string, port int) string {
	return filepath.Join(e.TunnelDir(), fmt.Sprintf("%s-stream-%d.sock", engine, port))
}

// HostPublished reports whether a host is published through a tunnel gateway.
func HostPublished(h *model.ProxyHost) bool { return h.TunnelGatewayID != "" }

// StreamPublished reports whether a stream's TCP part is published through a
// tunnel gateway (UDP is not carried by tunnels).
func StreamPublished(s *model.Stream) bool {
	return s.TunnelGatewayID != "" && s.Protocol != "udp"
}
