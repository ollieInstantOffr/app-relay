package lbengine

import "strings"

// UsableBackends parses "show stat" CSV (HAProxy or Relay Balancer) and
// reports, per backend name, whether at least one server can take new traffic
// (status UP, including "UP 1/3" while going down, or "no check"). Backends
// without such a server map to false; frontends and the stats proxy are skipped.
func UsableBackends(csv string) map[string]bool {
	out := map[string]bool{}
	idx := map[string]int{}
	for _, line := range strings.Split(csv, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			clear(idx)
			for i, col := range strings.Split(line[2:], ",") {
				idx[col] = i
			}
			continue
		}
		if line == "" || len(idx) == 0 {
			continue
		}
		f := strings.Split(line, ",")
		get := func(col string) string {
			if i, ok := idx[col]; ok && i < len(f) {
				return f[i]
			}
			return ""
		}
		px, sv, st := get("pxname"), get("svname"), get("status")
		if px == "" || px == "stats" || sv == "FRONTEND" {
			continue
		}
		if sv != "BACKEND" && (strings.HasPrefix(st, "UP") || st == "no check") {
			out[px] = true
		} else if _, seen := out[px]; !seen {
			out[px] = false
		}
	}
	return out
}
