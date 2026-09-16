package edge

import (
	"fmt"
	"math"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/instantoffr/relay/internal/agent"
	edgecfg "github.com/instantoffr/relay/internal/edge"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render"
)

const (
	defaultIdleTimeoutMs = 10 * 60 * 1000 // proxy_timeout 10m
	connectTimeoutMs     = 10 * 1000      // proxy_connect_timeout 10s
)

func (r *renderer) enabledStreams() []*model.Stream {
	var out []*model.Stream
	for i := range r.snap.Streams {
		if s := &r.snap.Streams[i]; s.Enabled {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (r *renderer) streams() []edgecfg.Stream {
	out := []edgecfg.Stream{}
	for _, s := range r.enabledStreams() {
		if st, ok := r.stream(s); ok {
			out = append(out, st)
		}
	}
	return out
}

// stream resolves one stream; ok is false (and the render fails) when it
// can't be served, exactly where the nginx renderer skips it.
func (r *renderer) stream(s *model.Stream) (edgecfg.Stream, bool) {
	lo, hi, err := parsePorts(s.ListenPorts)
	if err != nil {
		r.fail("stream %s: %v", s.Name, err)
		return edgecfg.Stream{}, false
	}
	fwdHost := strings.TrimSpace(s.ForwardHost)
	flo, fhi := lo, hi
	if s.BackendID != "" {
		fe := r.backendFrontend(s.BackendID)
		if fe == "" {
			name := s.BackendID
			if b := r.backends[s.BackendID]; b != nil {
				name = b.Name
			}
			r.fail("stream %s: backend %s has no enabled TCP frontend bound to 127.0.0.1", s.Name, name)
			return edgecfg.Stream{}, false
		}
		h, p, _ := net.SplitHostPort(fe)
		fwdHost = h
		flo, _ = strconv.Atoi(p)
		fhi = flo
	} else if strings.TrimSpace(s.ForwardPorts) != "" {
		if flo, fhi, err = parsePorts(s.ForwardPorts); err != nil {
			r.fail("stream %s: forward %v", s.Name, err)
			return edgecfg.Stream{}, false
		}
	}
	if fwdHost == "" {
		r.fail("stream %s: forward host is empty", s.Name)
		return edgecfg.Stream{}, false
	}
	if flo != fhi && fhi-flo != hi-lo {
		r.fail("stream %s: listen range %s and forward range %s differ in size", s.Name, s.ListenPorts, s.ForwardPorts)
		return edgecfg.Stream{}, false
	}
	isIP := net.ParseIP(fwdHost) != nil
	switch {
	case flo == fhi, isIP && flo == lo:
	case isIP:
		if hi-lo > 5000 {
			r.fail("stream %s: port range too large", s.Name)
			return edgecfg.Stream{}, false
		}
	default:
		if hi-lo > 1000 {
			r.fail("stream %s: port ranges to a hostname are limited to 1000 ports", s.Name)
			return edgecfg.Stream{}, false
		}
	}

	st := edgecfg.Stream{
		ID: s.ID, Name: s.Name,
		ListenAddr: strings.TrimSpace(s.ListenAddress),
		ListenLo:   lo, ListenHi: hi,
		ForwardHost: fwdHost, ForwardLo: flo, ForwardHi: fhi,
		ProxyProtocol:    s.ProxyProtocol,
		IdleTimeoutMs:    idleTimeoutMs(s.IdleTimeout),
		ConnectTimeoutMs: connectTimeoutMs,
	}
	switch s.Protocol {
	case "udp":
		st.UDP = true
	case "both":
		st.TCP, st.UDP = true, true
	default:
		st.TCP = true
	}
	if render.StreamPublished(s) {
		if hi-lo > 1000 {
			r.fail("stream %s: tunnels publish at most 1000 ports per stream", s.Name)
			return edgecfg.Stream{}, false
		}
		for p := lo; p <= hi; p++ {
			st.TunnelSockets = append(st.TunnelSockets, edgecfg.TunnelSocket{Port: p, Socket: r.env.TunnelStreamSocket(agent.EngineEdge, p)})
		}
	}
	return st, true
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

var timeRe = regexp.MustCompile(`^([0-9]+)(ms|s|m|h|d)?$`)

// idleTimeoutMs converts an nginx time (no unit = seconds); unset or invalid
// values use nginx's 10m default.
func idleTimeoutMs(s string) int {
	m := timeRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return defaultIdleTimeoutMs
	}
	mult := int64(1000)
	switch m[2] {
	case "ms":
		mult = 1
	case "m":
		mult = 60 * 1000
	case "h":
		mult = 60 * 60 * 1000
	case "d":
		mult = 24 * 60 * 60 * 1000
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || n > math.MaxInt32/mult {
		return math.MaxInt32
	}
	if n == 0 {
		// nginx times out immediately; 0 means "default" in edge.json.
		return 1
	}
	return int(n * mult)
}

// backendFrontend returns the 127.0.0.1 bind of the first enabled TCP
// frontend (by creation time) whose default backend is backendID.
func (r *renderer) backendFrontend(backendID string) string {
	fes := append([]model.Frontend(nil), r.snap.Frontends...)
	sort.SliceStable(fes, func(i, j int) bool { return fes[i].CreatedAt.Before(fes[j].CreatedAt) })
	for _, f := range fes {
		if !f.Enabled || f.Mode != "tcp" || f.DefaultBackendID != backendID {
			continue
		}
		h, p, err := net.SplitHostPort(strings.TrimSpace(f.Bind))
		if err != nil || h != "127.0.0.1" {
			continue
		}
		if _, err := strconv.Atoi(p); err != nil {
			continue
		}
		return net.JoinHostPort(h, p)
	}
	return ""
}
