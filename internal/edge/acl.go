// Package edge is Relay Edge: Relay's own reverse proxy engine, selectable
// instead of nginx. It serves the same proxy hosts, redirects, default host
// and streams from a JSON config pushed by the apply pipeline, and swaps
// configurations in memory without dropping connections.
package edge

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// IPRule is one allow/deny rule of an access list. CIDR is an address
// ("10.0.0.5"), a prefix ("192.168.0.0/16") or "all".
type IPRule struct {
	Allow bool   `json:"allow"`
	CIDR  string `json:"cidr"`
}

type compiledRule struct {
	allow  bool
	all    bool
	prefix netip.Prefix
}

// ipMatcher evaluates rules top to bottom; the first match wins.
type ipMatcher struct {
	rules []compiledRule
}

func compileIPRules(rules []IPRule) (*ipMatcher, error) {
	m := &ipMatcher{rules: make([]compiledRule, 0, len(rules))}
	for i, r := range rules {
		c := compiledRule{allow: r.Allow}
		v := strings.TrimSpace(r.CIDR)
		switch {
		case strings.EqualFold(v, "all"):
			c.all = true
		case strings.Contains(v, "/"):
			p, err := netip.ParsePrefix(v)
			if err != nil {
				return nil, fmt.Errorf("rule %d: invalid prefix %q", i+1, v)
			}
			c.prefix = p.Masked()
		default:
			a, err := netip.ParseAddr(v)
			if err != nil {
				return nil, fmt.Errorf("rule %d: invalid address %q", i+1, v)
			}
			a = a.Unmap()
			c.prefix = netip.PrefixFrom(a, a.BitLen())
		}
		m.rules = append(m.rules, c)
	}
	return m, nil
}

// decide reports whether addr is allowed and whether any rule matched. Like
// nginx's access module, an address that matches no rule is allowed.
func (m *ipMatcher) decide(addr netip.Addr) (allow, matched bool) {
	if m == nil {
		return true, false
	}
	addr = addr.Unmap()
	for _, r := range m.rules {
		if r.all || r.prefix.Contains(addr) {
			return r.allow, true
		}
	}
	return true, false
}

// hostAddr parses the IP of a "host:port" (or bare host) remote address.
func hostAddr(remote string) netip.Addr {
	host := remote
	if h, _, err := net.SplitHostPort(remote); err == nil {
		host = h
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}
