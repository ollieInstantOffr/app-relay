// Package tunnel renders tunnel.json for the relay-tunnel engine: which
// hosts and TCP stream ports are published through each gateway, and the
// proxy engine ingress sockets they go to. Gateway addresses and keys are
// runtime state (gateways.json) and not part of the release.
package tunnel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render"
	tunnelcfg "github.com/instantoffr/relay/internal/tunnel"
)

// GatewaysFile is where Relay writes the runtime gateway list.
func GatewaysFile(env render.Env) string {
	dir := env.DataDir
	if dir == "" {
		dir = "/data"
	}
	return filepath.Join(dir, "tunnel", tunnelcfg.GatewaysFileName)
}

// RuntimeSocket is the tunnel engine's status socket.
func RuntimeSocket(env render.Env) string {
	dir := env.RunDir
	if dir == "" {
		dir = "/run/relay"
	}
	return filepath.Join(dir, tunnelcfg.RuntimeSocketName)
}

// Published reports whether anything in snap is published through a tunnel
// (the tunnel engine only runs then).
func Published(snap *model.Snapshot) bool {
	for i := range snap.Hosts {
		if h := &snap.Hosts[i]; h.Enabled && len(h.Domains) > 0 && render.HostPublished(h) {
			return true
		}
	}
	for i := range snap.Streams {
		if s := &snap.Streams[i]; s.Enabled && render.StreamPublished(s) {
			return true
		}
	}
	return false
}

// Render returns the tunnel engine's file set.
func Render(snap *model.Snapshot, env render.Env) (agent.Files, error) {
	engine := snap.General.ProxyEngine
	if engine == "" {
		engine = agent.EngineNginx
	}
	cfg := tunnelcfg.Config{
		Schema:        1,
		GatewaysFile:  GatewaysFile(env),
		RuntimeSocket: RuntimeSocket(env),
		Targets: tunnelcfg.Targets{
			HTTP:  env.TunnelHTTPSocket(engine),
			HTTPS: env.TunnelHTTPSSocket(engine),
		},
		Routes: []tunnelcfg.Route{},
	}
	var errs []string
	routes := map[string]*tunnelcfg.Route{}
	route := func(id string) *tunnelcfg.Route {
		if r := routes[id]; r != nil {
			return r
		}
		r := &tunnelcfg.Route{GatewayID: id, Names: []string{}, TCP: []tunnelcfg.TCPRoute{}}
		routes[id] = r
		return r
	}
	names := map[string]map[string]string{} // gateway → name → host id
	for i := range snap.Hosts {
		h := &snap.Hosts[i]
		if !h.Enabled || !render.HostPublished(h) {
			continue
		}
		r := route(h.TunnelGatewayID)
		if names[r.GatewayID] == nil {
			names[r.GatewayID] = map[string]string{}
		}
		for _, d := range h.Domains {
			d = strings.ToLower(strings.TrimSpace(d))
			if d == "" {
				continue
			}
			if _, dup := names[r.GatewayID][d]; dup {
				continue // the first host wins, like server_name
			}
			names[r.GatewayID][d] = h.ID
			r.Names = append(r.Names, d)
		}
	}
	ports := map[string]map[uint16]string{} // gateway → port → stream name
	for i := range snap.Streams {
		s := &snap.Streams[i]
		if !s.Enabled || !render.StreamPublished(s) {
			continue
		}
		lo, hi, err := parsePorts(s.ListenPorts)
		if err != nil {
			errs = append(errs, fmt.Sprintf("stream %s: %v", s.Name, err))
			continue
		}
		if hi-lo > 1000 {
			errs = append(errs, fmt.Sprintf("stream %s: tunnels publish at most 1000 ports per stream", s.Name))
			continue
		}
		r := route(s.TunnelGatewayID)
		if ports[r.GatewayID] == nil {
			ports[r.GatewayID] = map[uint16]string{}
		}
		for p := lo; p <= hi; p++ {
			if other, dup := ports[r.GatewayID][uint16(p)]; dup {
				errs = append(errs, fmt.Sprintf("stream %s: port %d is already published through the same gateway by stream %s", s.Name, p, other))
				continue
			}
			ports[r.GatewayID][uint16(p)] = s.Name
			r.TCP = append(r.TCP, tunnelcfg.TCPRoute{Port: uint16(p), Socket: env.TunnelStreamSocket(engine, p)})
		}
	}
	ids := make([]string, 0, len(routes))
	for id := range routes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		r := routes[id]
		sort.Strings(r.Names)
		sort.Slice(r.TCP, func(i, j int) bool { return r.TCP[i].Port < r.TCP[j].Port })
		cfg.Routes = append(cfg.Routes, *r)
	}
	if err := cfg.Validate(); err != nil {
		errs = append(errs, err.Error())
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(cfg); err != nil {
		return nil, err
	}
	files := agent.Files{tunnelcfg.ConfigFile: buf.String()}
	if len(errs) > 0 {
		return files, fmt.Errorf("tunnel render: %s", strings.Join(errs, "; "))
	}
	return files, nil
}

func parsePorts(s string) (lo, hi int, err error) {
	s = strings.TrimSpace(s)
	a, b, isRange := strings.Cut(s, "-")
	lo, err = strconv.Atoi(strings.TrimSpace(a))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid port %q", s)
	}
	hi = lo
	if isRange {
		if hi, err = strconv.Atoi(strings.TrimSpace(b)); err != nil {
			return 0, 0, fmt.Errorf("invalid port range %q", s)
		}
	}
	if lo < 1 || hi > 65535 || hi < lo {
		return 0, 0, fmt.Errorf("invalid port range %q", s)
	}
	return lo, hi, nil
}
