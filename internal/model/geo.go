package model

import (
	"net/netip"
	"sort"
	"strings"
)

// LocalNetworks are never geo-blocked: loopback, private, link-local and
// carrier-grade NAT addresses have no country. Both proxy engines use this list.
var LocalNetworks = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
}

// IsLocalAddr reports whether ip is in LocalNetworks.
func IsLocalAddr(ip netip.Addr) bool {
	ip = ip.Unmap().WithZone("")
	for _, p := range LocalNetworks {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// NormalizeCountries upper-cases, de-duplicates and sorts two-letter country
// codes; anything else is dropped. Returns nil when none are left.
func NormalizeCountries(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range in {
		c = strings.ToUpper(strings.TrimSpace(c))
		if len(c) != 2 || c[0] < 'A' || c[0] > 'Z' || c[1] < 'A' || c[1] > 'Z' || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// GeoCountries returns every country allowed by a host with geo-blocking on.
func GeoCountries(hosts []ProxyHost) []string {
	all := []string{}
	for _, h := range hosts {
		if h.GeoBlock.Enabled {
			all = append(all, h.GeoBlock.AllowCountries...)
		}
	}
	return NormalizeCountries(all)
}
