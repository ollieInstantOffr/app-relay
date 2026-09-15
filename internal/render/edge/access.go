package edge

import (
	"sort"
	"strings"

	edgecfg "github.com/instantoffr/relay/internal/edge"
	"github.com/instantoffr/relay/internal/model"
)

// accessLists returns every access list referenced by a rendered location.
func (r *renderer) accessLists() map[string]edgecfg.AccessList {
	out := map[string]edgecfg.AccessList{}
	for id := range r.usedLists {
		if al := r.lists[id]; al != nil {
			out[id] = accessList(al)
		}
	}
	return out
}

// accessList resolves an access list like the nginx allow/deny/auth_basic
// directives: rules stop after an "all" rule, invalid rules are skipped, and
// a trailing "deny all" is added when at least one address was allowed.
func accessList(al *model.AccessList) edgecfg.AccessList {
	out := edgecfg.AccessList{Name: al.Name, Rules: []edgecfg.IPRule{}}
	lastAll, anyAllow := false, false
	for _, rule := range al.Rules {
		cidr := strings.TrimSpace(rule.CIDR)
		allow := rule.Action == "allow"
		if strings.EqualFold(cidr, "all") {
			out.Rules = append(out.Rules, edgecfg.IPRule{Allow: allow, CIDR: "all"})
			lastAll = true
			break
		}
		if !validCIDR(cidr) {
			continue
		}
		out.Rules = append(out.Rules, edgecfg.IPRule{Allow: allow, CIDR: cidr})
		if allow {
			anyAllow = true
		}
	}
	hasRules := len(out.Rules) > 0
	if hasRules && anyAllow && !lastAll {
		out.Rules = append(out.Rules, edgecfg.IPRule{Allow: false, CIDR: "all"})
	}
	if al.BasicAuth.Enabled {
		out.BasicAuth = &edgecfg.BasicAuth{
			Realm:     orDefault(al.BasicAuth.Realm, "Restricted"),
			UsersFile: "htpasswd/" + safeID(al.ID),
		}
		out.SatisfyAny = hasRules && al.SatisfyAny
	}
	return out
}

// htpasswd renders the basic-auth users file (identical to the nginx renderer,
// so the apply service's redaction keeps masking the hashes).
func htpasswd(al *model.AccessList) string {
	users := append([]model.BasicAuthUser(nil), al.BasicAuth.Users...)
	sort.SliceStable(users, func(i, j int) bool { return users[i].Username < users[j].Username })
	var b strings.Builder
	b.WriteString("# " + oneLine(al.Name) + "\n")
	for _, u := range users {
		name := strings.TrimSpace(u.Username)
		if name == "" || u.PasswordHash == "" || strings.ContainsAny(name, ":\n") {
			continue
		}
		b.WriteString(name + ":" + strings.TrimSpace(u.PasswordHash) + "\n")
	}
	return b.String()
}
