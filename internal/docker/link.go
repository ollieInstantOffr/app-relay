package docker

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"

	"github.com/instantoffr/relay/internal/core"
)

// linkPending fills in and enables the disabled hosts that were created for a
// stopped local container, once that container runs and has an address.
// Caller holds syncMu.
func (s *Service) linkPending(ctx context.Context, m *managedState, c core.Container) bool {
	if c.State != "running" || c.UpstreamHost == "" {
		return false
	}
	changed := false
	for id, e := range m.Hosts {
		if !e.Link || endpointOf(e.Endpoint) != c.EndpointID || e.Container != c.Name {
			continue
		}
		host, err := s.app.Store.Hosts().Get(ctx, id)
		if err != nil {
			delete(m.Hosts, id) // host deleted meanwhile
			changed = true
			continue
		}
		if host.Enabled || host.Upstream.Host != placeholderHost(c.Name) {
			e.Link = false // edited by a user meanwhile: leave it alone
			changed = true
			continue
		}
		port := 0
		if e.Port > 0 {
			port = upstreamPortFor(c, e.Port)
		}
		if port == 0 {
			port = c.SuggestedPort
		}
		if port == 0 {
			s.warnOnce(ctx, sourceRef(c), "container started but its port is unknown — edit host "+host.Domains[0])
			continue
		}
		next := cloneHost(host)
		next.Upstream.Host, next.Upstream.Port, next.Enabled = c.UpstreamHost, port, true
		addr := net.JoinHostPort(c.UpstreamHost, strconv.Itoa(port))
		r := dockerRequest(ctx, http.MethodPut, "/api/hosts/"+id)
		if err := s.saveHost(r, host, next, fmt.Sprintf("linked to container %s (started) · upstream %s", onEndpoint(c.Name, c), addr)); err != nil {
			s.warnOnce(ctx, sourceRef(c), "could not link host "+host.Domains[0]+": "+err.Error())
			continue
		}
		e.Link = false
		changed = true
		s.app.Activity(ctx, "host.linked", "ok", fmt.Sprintf("%s linked to container %s (started)", next.Domains[0], c.Name), next.Domains[0], addr)
		s.publish("host_linked", c.Name)
	}
	return changed
}
