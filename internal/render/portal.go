package render

import (
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/instantoffr/relay/internal/model"
)

// RelayLogin reports whether a host is protected by the built-in Relay login.
func RelayLogin(h *model.ProxyHost) bool {
	return h.ForwardAuth.Enabled && h.ForwardAuth.Provider == model.ForwardAuthRelay
}

// PrepareSnapshot returns snap with the parts Relay manages resolved: hosts
// protected by Relay login get their verify and sign-in URLs and the /.relay/
// location serving the login page. snap itself is not modified.
func PrepareSnapshot(snap *model.Snapshot, env Env) *model.Snapshot {
	need := false
	for i := range snap.Hosts {
		if RelayLogin(&snap.Hosts[i]) {
			need = true
			break
		}
	}
	if !need {
		return snap
	}
	out := *snap
	out.Hosts = make([]model.ProxyHost, len(snap.Hosts))
	copy(out.Hosts, snap.Hosts)
	for i := range out.Hosts {
		PrepareHost(&out.Hosts[i], env)
	}
	return &out
}

// PrepareHost resolves Relay login on one host (a no-op for other hosts).
func PrepareHost(h *model.ProxyHost, env Env) {
	if !RelayLogin(h) {
		return
	}
	id := url.QueryEscape(h.ID)
	h.ForwardAuth.VerifyURL = "http://" + env.AdminUpstream + PortalPrefix + "verify?host=" + id
	h.ForwardAuth.SignInURL = PortalPrefix + "login?host=" + id
	host, portStr, err := net.SplitHostPort(env.AdminUpstream)
	port, _ := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		host, port = "127.0.0.1", 8181
	}
	locs := make([]model.Location, 0, len(h.Locations)+1)
	for _, l := range h.Locations {
		if strings.TrimRight(l.Path, "/")+"/" != PortalPrefix {
			locs = append(locs, l)
		}
	}
	locs = append(locs, model.Location{
		ID: PortalLocationID, Path: PortalPrefix, Kind: model.LocationProxy, NoAuth: true,
		Upstream: model.Upstream{Scheme: "http", Host: host, Port: port},
	})
	h.Locations = locs
}
