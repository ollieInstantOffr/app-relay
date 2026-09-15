// Package edge is Relay Edge: Relay's own reverse proxy engine, selectable
// instead of nginx. It serves the same proxy hosts, redirects, default host
// and streams from a JSON config pushed by the apply pipeline, and swaps
// configurations in memory without dropping connections.
package edge

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
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
		if strings.EqualFold(strings.TrimSpace(r.CIDR), "all") {
			c.all = true
		} else {
			p, err := parsePrefix(r.CIDR)
			if err != nil {
				return nil, fmt.Errorf("rule %d: %w", i+1, err)
			}
			c.prefix = p
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

// parsePrefix accepts an address or a CIDR prefix.
func parsePrefix(s string) (netip.Prefix, error) {
	v := strings.TrimSpace(s)
	if strings.Contains(v, "/") {
		p, err := netip.ParsePrefix(v)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("invalid prefix %q", v)
		}
		if p.Addr().Is4In6() {
			p = netip.PrefixFrom(p.Addr().Unmap(), max(p.Bits()-96, 0))
		}
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(v)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("invalid address %q", v)
	}
	a = a.Unmap().WithZone("")
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// prefixMap maps prefixes to values with longest-prefix lookup (nginx geo).
// Lookups mask the address once per distinct prefix length, so large
// blocklists stay O(lengths) instead of O(entries).
type prefixMap[V any] struct {
	entries map[netip.Prefix]V
	v4, v6  []int // distinct prefix lengths, longest first
}

func newPrefixMap[V any]() *prefixMap[V] {
	return &prefixMap[V]{entries: map[netip.Prefix]V{}}
}

// add sets the value of p; the first value added for a prefix wins.
func (m *prefixMap[V]) add(p netip.Prefix, v V) {
	if _, dup := m.entries[p]; dup {
		return
	}
	m.entries[p] = v
	lens := &m.v6
	if p.Addr().Is4() {
		lens = &m.v4
	}
	if !slices.Contains(*lens, p.Bits()) {
		*lens = append(*lens, p.Bits())
		slices.Sort(*lens)
		slices.Reverse(*lens)
	}
}

func (m *prefixMap[V]) lookup(addr netip.Addr) (V, bool) {
	var zero V
	if m == nil || len(m.entries) == 0 || !addr.IsValid() {
		return zero, false
	}
	addr = addr.Unmap()
	lens := m.v6
	if addr.Is4() {
		lens = m.v4
	}
	for _, bits := range lens {
		p, err := addr.Prefix(bits)
		if err != nil {
			continue
		}
		if v, ok := m.entries[p]; ok {
			return v, true
		}
	}
	return zero, false
}

func (m *prefixMap[V]) len() int {
	if m == nil {
		return 0
	}
	return len(m.entries)
}

// hostAddr parses the IP of a "host:port" (or bare host) remote address.
func hostAddr(remote string) netip.Addr {
	if ap, err := netip.ParseAddrPort(remote); err == nil {
		return ap.Addr().Unmap().WithZone("")
	}
	host := remote
	if h, _, err := net.SplitHostPort(remote); err == nil {
		host = h
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap().WithZone("")
}
