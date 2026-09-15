package docker

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
)

// binding is a published TCP port.
type binding struct {
	Private int
	Public  int
	HostIP  string
}

// portConfig describes a container's ports: from the list summary for running
// containers, from inspect (configured bindings) for stopped ones.
type portConfig struct {
	ports    []core.ContainerPort
	bindings []binding
}

type upstreamInput struct {
	Local           bool   // daemon runs on Relay's host
	EndpointName    string // for reasons
	UpstreamAddress string // remote: address of the Docker host
	Running         bool
	HostNetwork     bool
	IP              string // container IP (running, not host network)
	Image           string
	Labels          map[string]string
	Ports           []core.ContainerPort
	Bindings        []binding
}

type upstreamResult struct {
	HTTP   bool
	Host   string // "" = not reachable
	Port   int    // upstream port (container port when not reachable)
	Scheme string
	Reason string
}

// resolveUpstream decides the address Relay proxies to:
//   - local endpoint, running container with an IP → IP:container port
//   - host networking → 127.0.0.1 (local) or the host address:container port
//   - otherwise a published port → 127.0.0.1 (local) or the host address:published port
func resolveUpstream(in upstreamInput) upstreamResult {
	g := Classify(in.Image, in.Ports, in.Labels)
	res := upstreamResult{HTTP: g.HTTP, Port: g.Port, Scheme: g.Scheme, Reason: g.Reason}
	hostAddr := func() (string, bool) {
		if in.Local {
			return "127.0.0.1", true
		}
		return in.UpstreamAddress, in.UpstreamAddress != ""
	}
	unreachable := ""
	switch {
	case in.HostNetwork:
		if h, ok := hostAddr(); ok && g.Port > 0 {
			res.Host = h
		} else if !ok {
			unreachable = "set an address for upstreams on " + in.EndpointName
		}
	case in.Local && in.Running && in.IP != "":
		if g.Port > 0 {
			res.Host = in.IP
		}
	default:
		b, found := pickBinding(in, g)
		if found {
			if b.Private != g.Port {
				bg := Classify(in.Image, []core.ContainerPort{{Private: b.Private, Proto: "tcp"}}, in.Labels)
				res.HTTP, res.Reason, res.Scheme = bg.HTTP, bg.Reason, bg.Scheme
			}
			ip := net.ParseIP(b.HostIP)
			switch {
			case b.HostIP == "" || ip == nil || ip.IsUnspecified():
				if h, ok := hostAddr(); ok {
					res.Host, res.Port = h, b.Public
				} else {
					unreachable = "set an address for upstreams on " + in.EndpointName
				}
			case ip.IsLoopback():
				if in.Local {
					res.Host, res.Port = "127.0.0.1", b.Public
				} else {
					unreachable = fmt.Sprintf("published on %s only on %s", b.HostIP, in.EndpointName)
				}
			default:
				res.Host, res.Port = b.HostIP, b.Public
			}
		}
	}

	if res.Host != "" {
		if !in.Running {
			if res.HTTP {
				res.Reason = fmt.Sprintf("stopped · starts on %s", net.JoinHostPort(res.Host, strconv.Itoa(res.Port)))
			} else {
				res.Reason = "stopped · " + res.Reason
			}
		}
		return res
	}
	switch {
	case unreachable != "":
		res.Reason = unreachable
	case !res.HTTP && res.Reason != "" && len(in.Bindings) == 0 && !(in.Local && in.Running):
		// not HTTP and nothing published: explain the missing port first for remote hosts
		if in.Local {
			res.Reason = "stopped · " + res.Reason
		} else {
			res.Reason = fmt.Sprintf("no published port on %s — publish a port to proxy it", in.EndpointName)
		}
	case !res.HTTP && res.Reason != "":
		if !in.Running {
			res.Reason = "stopped · " + res.Reason
		}
	case in.Local && !in.Running:
		res.Reason = "stopped — no published port"
	case in.Local:
		res.Reason = "no IP address — publish a port to proxy it"
	default:
		res.Reason = fmt.Sprintf("no published port on %s — publish a port to proxy it", in.EndpointName)
		if !in.Running {
			res.Reason = "stopped · " + res.Reason
		}
	}
	return res
}

// pickBinding finds the published port for the classified container port, or
// the best web-looking published port.
func pickBinding(in upstreamInput, g Guess) (binding, bool) {
	find := func(private int) (binding, bool) {
		var best binding
		ok := false
		for _, b := range in.Bindings {
			if b.Private != private {
				continue
			}
			ip := net.ParseIP(b.HostIP)
			// Prefer wildcard IPv4, then any non-loopback, then loopback.
			if !ok || (b.HostIP == "0.0.0.0") || (ip != nil && !ip.IsLoopback() && net.ParseIP(best.HostIP) != nil && net.ParseIP(best.HostIP).IsLoopback()) {
				best, ok = b, true
			}
		}
		return best, ok
	}
	if g.Port > 0 {
		if b, ok := find(g.Port); ok {
			return b, true
		}
		if strings.TrimSpace(in.Labels["relay.port"]) != "" {
			return binding{}, false // explicit port that isn't published
		}
	}
	if len(in.Bindings) == 0 {
		return binding{}, false
	}
	published := []core.ContainerPort{}
	for _, b := range in.Bindings {
		published = append(published, core.ContainerPort{Private: b.Private, Public: b.Public, Proto: "tcp"})
	}
	bg := Classify(in.Image, published, nil)
	if bg.Port > 0 {
		return find(bg.Port)
	}
	return binding{}, false
}

// summaryPorts reads ports of a running container from the list response.
func summaryPorts(sm container.Summary) portConfig {
	pc := portConfig{ports: []core.ContainerPort{}}
	idx := map[string]int{}
	for _, p := range sm.Ports {
		proto := p.Type
		if proto == "" {
			proto = "tcp"
		}
		key := fmt.Sprintf("%d/%s", p.PrivatePort, proto)
		if i, ok := idx[key]; ok {
			if pc.ports[i].Public == 0 && p.PublicPort != 0 {
				pc.ports[i].Public = int(p.PublicPort)
			}
		} else {
			idx[key] = len(pc.ports)
			pc.ports = append(pc.ports, core.ContainerPort{Private: int(p.PrivatePort), Public: int(p.PublicPort), Proto: proto})
		}
		if p.PublicPort != 0 && proto == "tcp" {
			pc.bindings = append(pc.bindings, binding{Private: int(p.PrivatePort), Public: int(p.PublicPort), HostIP: p.IP})
		}
	}
	sort.Slice(pc.ports, func(i, j int) bool { return pc.ports[i].Private < pc.ports[j].Private })
	return pc
}

// inspectPorts reads the configured ports of a (stopped) container.
func inspectPorts(resp container.InspectResponse) portConfig {
	pc := portConfig{ports: []core.ContainerPort{}}
	idx := map[string]int{}
	add := func(private int, proto string, public int) {
		key := fmt.Sprintf("%d/%s", private, proto)
		if i, ok := idx[key]; ok {
			if pc.ports[i].Public == 0 {
				pc.ports[i].Public = public
			}
			return
		}
		idx[key] = len(pc.ports)
		pc.ports = append(pc.ports, core.ContainerPort{Private: private, Public: public, Proto: proto})
	}
	if resp.Config != nil {
		for p := range resp.Config.ExposedPorts {
			add(p.Int(), p.Proto(), 0)
		}
	}
	if resp.ContainerJSONBase != nil && resp.HostConfig != nil {
		for p, bs := range resp.HostConfig.PortBindings {
			for _, b := range bs {
				public, _ := strconv.Atoi(b.HostPort)
				add(p.Int(), p.Proto(), public)
				if public > 0 && p.Proto() == "tcp" {
					pc.bindings = append(pc.bindings, binding{Private: p.Int(), Public: public, HostIP: b.HostIP})
				}
			}
		}
	}
	sort.Slice(pc.ports, func(i, j int) bool { return pc.ports[i].Private < pc.ports[j].Private })
	sort.Slice(pc.bindings, func(i, j int) bool { return pc.bindings[i].Private < pc.bindings[j].Private })
	return pc
}

// buildContainer converts a Docker summary into Relay's container view.
func buildContainer(ep model.DockerEndpoint, sm container.Summary, pc portConfig) core.Container {
	name := sm.ID
	if len(sm.Names) > 0 {
		name = strings.TrimPrefix(sm.Names[0], "/")
	}
	labels := sm.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	running := string(sm.State) == "running"
	mode := sm.HostConfig.NetworkMode
	ip := ""
	if running && mode != "host" && sm.NetworkSettings != nil {
		ip = PickIP(mode, sm.NetworkSettings.Networks)
	}
	r := resolveUpstream(upstreamInput{
		Local: isLocal(ep), EndpointName: ep.Name, UpstreamAddress: effectiveUpstream(ep), Running: running,
		HostNetwork: mode == "host", IP: ip, Image: sm.Image, Labels: labels, Ports: pc.ports, Bindings: pc.bindings,
	})
	return core.Container{
		ID: sm.ID, Name: name, Image: sm.Image, State: string(sm.State), IP: ip, Ports: pc.ports, Labels: labels,
		HTTP: r.HTTP, SuggestedPort: r.Port, Reason: r.Reason,
		EndpointID: ep.ID, EndpointName: ep.Name, UpstreamHost: r.Host,
	}
}

// containerScheme picks http/https for a container's upstream port.
func containerScheme(c core.Container) string {
	if s := strings.ToLower(c.Labels["relay.scheme"]); s == "http" || s == "https" {
		return s
	}
	if c.UpstreamHost != "" && c.UpstreamHost != c.IP {
		for _, p := range c.Ports {
			if p.Public == c.SuggestedPort && p.Public != 0 {
				return SchemeForPort(p.Private)
			}
		}
	}
	return SchemeForPort(c.SuggestedPort)
}

// upstreamPorts are the ports usable on UpstreamHost: container ports when
// proxying to the container IP or host network, published ports otherwise.
func upstreamPorts(c core.Container) map[int]bool {
	m := map[int]bool{}
	published := false
	for _, p := range c.Ports {
		if p.Public != 0 {
			published = true
		}
	}
	for _, p := range c.Ports {
		if c.UpstreamHost == c.IP || !published {
			m[p.Private] = true
		} else if p.Public != 0 {
			m[p.Public] = true
		}
	}
	return m
}

// sourceRef is the proxy host sourceRef for a container.
func sourceRef(c core.Container) string { return c.EndpointName + "/" + c.Name }
