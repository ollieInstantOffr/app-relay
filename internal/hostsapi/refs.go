package hostsapi

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// refs is a read-only view of everything hosts and redirects reference.
type refs struct {
	hosts       []model.ProxyHost
	redirects   []model.Redirect
	accessLists map[string]string // id → name
	certs       map[string]model.Certificate
	backends    map[string]string // id → name
}

func loadRefs(ctx context.Context, st *store.Store) (*refs, error) {
	rf := &refs{accessLists: map[string]string{}, certs: map[string]model.Certificate{}, backends: map[string]string{}}
	var err error
	if rf.hosts, err = st.Hosts().List(ctx); err != nil {
		return nil, err
	}
	if rf.redirects, err = st.Redirects().List(ctx); err != nil {
		return nil, err
	}
	lists, err := st.AccessLists().List(ctx)
	if err != nil {
		return nil, err
	}
	for _, l := range lists {
		rf.accessLists[l.ID] = l.Name
	}
	certs, err := st.Certificates().List(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range certs {
		rf.certs[c.ID] = c
	}
	backends, err := st.Backends().List(ctx)
	if err != nil {
		return nil, err
	}
	for _, b := range backends {
		rf.backends[b.ID] = b.Name
	}
	return rf, nil
}

func first(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return ss[0]
}

func redirectName(v *model.Redirect) string { return first(v.Domains) + v.FromPath }

// domainOwner is the entity that already serves a domain.
type domainOwner struct {
	Kind string `json:"kind"` // host | redirect
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (o *domainOwner) message() string { return fmt.Sprintf("Already used by %s %s", o.Kind, o.Name) }

// findDomainOwner reports which other host or redirect conflicts with serving
// domain from an entity of kind ("host" | "redirect") with id excludeID.
//
//   - A host conflicts with other hosts and whole-domain redirects.
//   - A whole-domain redirect conflicts with hosts and any redirect on the domain.
//   - A path redirect (fromPath set) conflicts with whole-domain redirects and
//     redirects for the same path; it may live next to a host on that domain.
func findDomainOwner(hosts []model.ProxyHost, redirects []model.Redirect, domain, kind, excludeID, fromPath string) *domainOwner {
	if fromPath == "/" {
		fromPath = ""
	}
	whole := kind == "host" || fromPath == ""
	if whole {
		for i := range hosts {
			h := &hosts[i]
			if kind == "host" && h.ID == excludeID {
				continue
			}
			if slices.Contains(h.Domains, domain) {
				return &domainOwner{Kind: "host", ID: h.ID, Name: first(h.Domains)}
			}
		}
	}
	for i := range redirects {
		v := &redirects[i]
		if kind == "redirect" && v.ID == excludeID {
			continue
		}
		if !slices.Contains(v.Domains, domain) {
			continue
		}
		var conflict bool
		switch {
		case kind == "host":
			conflict = v.IsWholeDomain()
		case fromPath == "":
			conflict = true
		default:
			conflict = v.IsWholeDomain() || v.FromPath == fromPath
		}
		if conflict {
			return &domainOwner{Kind: "redirect", ID: v.ID, Name: redirectName(v)}
		}
	}
	return nil
}

func checkHostRefs(e model.Errs, rf *refs, h *model.ProxyHost) {
	for i, d := range h.Domains {
		f := fmt.Sprintf("domains.%d", i)
		if _, bad := e[f]; bad || model.HostDomainError(d) != "" {
			continue
		}
		if o := findDomainOwner(rf.hosts, rf.redirects, d, "host", h.ID, ""); o != nil {
			e.Add(f, "%s", o.message())
		}
	}
	if id := h.AccessListID; id != "" {
		if _, ok := rf.accessLists[id]; !ok {
			e.Add("accessListId", "This access list no longer exists")
		}
	}
	for i, l := range h.Locations {
		if l.AccessListID == "" {
			continue
		}
		if _, ok := rf.accessLists[l.AccessListID]; !ok {
			e.Add(fmt.Sprintf("locations.%d.accessListId", i), "This access list no longer exists")
		}
	}
	if id := h.RateLimit.ExemptAccessListID; id != "" {
		if _, ok := rf.accessLists[id]; !ok {
			e.Add("rateLimit.exemptAccessListId", "This access list no longer exists")
		}
	}
	if id := h.CertificateID; id != "" {
		if _, ok := rf.certs[id]; !ok {
			e.Add("certificateId", "This certificate no longer exists")
		}
	}
	if id := h.Upstream.BackendID; id != "" {
		if _, ok := rf.backends[id]; !ok {
			e.Add("upstream.backendId", "This load-balancer backend no longer exists")
		}
	}
}

func checkRedirectRefs(e model.Errs, rf *refs, v *model.Redirect) {
	for i, d := range v.Domains {
		f := fmt.Sprintf("domains.%d", i)
		if _, bad := e[f]; bad || model.HostDomainError(d) != "" {
			continue
		}
		if o := findDomainOwner(rf.hosts, rf.redirects, d, "redirect", v.ID, v.FromPath); o != nil {
			e.Add(f, "%s", o.message())
		}
	}
	if id := v.CertificateID; id != "" {
		if _, ok := rf.certs[id]; !ok {
			e.Add("certificateId", "This certificate no longer exists")
		}
	}
}

// certUsers lists what (besides excludeHostID) references certificate id.
func certUsers(id, excludeHostID string, hosts []model.ProxyHost, redirects []model.Redirect,
	dh model.DefaultHostSettings, gen model.GeneralSettings, dock model.DockerSettings) []string {
	out := []string{}
	if id == "" {
		return out
	}
	for i := range hosts {
		if hosts[i].ID != excludeHostID && hosts[i].CertificateID == id {
			out = append(out, first(hosts[i].Domains))
		}
	}
	for i := range redirects {
		if redirects[i].CertificateID == id {
			out = append(out, "redirect "+redirectName(&redirects[i]))
		}
	}
	if dh.CertificateID == id {
		out = append(out, "default TLS certificate")
	}
	if gen.Defaults.CertificateID == id {
		out = append(out, "new host defaults")
	}
	if dock.DefaultCertID == id {
		out = append(out, "Docker discovery defaults")
	}
	return out
}

func loadCertUsers(ctx context.Context, st *store.Store, id, excludeHostID string) ([]string, error) {
	hosts, err := st.Hosts().List(ctx)
	if err != nil {
		return nil, err
	}
	redirects, err := st.Redirects().List(ctx)
	if err != nil {
		return nil, err
	}
	dh, err := store.LoadSettings[model.DefaultHostSettings](ctx, st, model.SettingsDefaultHost)
	if err != nil {
		return nil, err
	}
	gen, err := store.LoadSettings[model.GeneralSettings](ctx, st, model.SettingsGeneral)
	if err != nil {
		return nil, err
	}
	dock, err := store.LoadSettings[model.DockerSettings](ctx, st, model.SettingsDocker)
	if err != nil {
		return nil, err
	}
	return certUsers(id, excludeHostID, hosts, redirects, dh, gen, dock), nil
}

// copyDomain prefixes the first label: grafana.home.lan → copy-grafana.home.lan
// (copy2-, copy3- … for n > 1); wildcards keep their "*." prefix.
func copyDomain(d string, n int) string {
	prefix := "copy-"
	if n > 1 {
		prefix = fmt.Sprintf("copy%d-", n)
	}
	wild := strings.HasPrefix(d, "*.")
	rest := strings.TrimPrefix(d, "*.")
	label, tail, hasTail := strings.Cut(rest, ".")
	label = prefix + label
	if len(label) > 63 {
		label = strings.TrimRight(label[:63], "-")
	}
	out := label
	if hasTail {
		out += "." + tail
	}
	if wild {
		out = "*." + out
	}
	return out
}

// duplicateDomains returns free copy-… names for every domain.
func duplicateDomains(domains []string, hosts []model.ProxyHost, redirects []model.Redirect) []string {
	taken := map[string]bool{}
	out := make([]string, 0, len(domains))
	for _, d := range domains {
		for n := 1; n < 1000; n++ {
			c := copyDomain(d, n)
			if taken[c] || findDomainOwner(hosts, redirects, c, "host", "", "") != nil {
				continue
			}
			taken[c] = true
			out = append(out, c)
			break
		}
	}
	return out
}

// mergeInto copies validation field errors into e; other errors are returned.
func mergeInto(e model.Errs, err error) error {
	if err == nil {
		return nil
	}
	var ve *model.ValidationError
	if errors.As(err, &ve) {
		for k, v := range ve.Fields {
			e.Add(k, "%s", v)
		}
		return nil
	}
	return err
}

// errText flattens an error for bulk result rows.
func errText(err error) string {
	var ve *model.ValidationError
	var he *httpx.HTTPError
	switch {
	case errors.As(err, &ve):
		keys := make([]string, 0, len(ve.Fields))
		for k := range ve.Fields {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, ve.Fields[k])
		}
		return strings.Join(parts, "; ")
	case errors.As(err, &he):
		return he.Message
	case errors.Is(err, store.ErrNotFound):
		return "not found"
	}
	return err.Error()
}

var safeIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
