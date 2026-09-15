package hostsapi

import (
	"slices"
	"strings"
	"testing"

	"github.com/instantoffr/relay/internal/model"
)

func TestCopyDomain(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"grafana.home.lan", 1, "copy-grafana.home.lan"},
		{"grafana.home.lan", 2, "copy2-grafana.home.lan"},
		{"*.home.lan", 1, "*.copy-home.lan"},
		{"nas", 1, "copy-nas"},
	}
	for _, c := range cases {
		if got := copyDomain(c.in, c.n); got != c.want {
			t.Errorf("copyDomain(%q,%d) = %q want %q", c.in, c.n, got, c.want)
		}
	}
	long := strings.Repeat("a", 63) + ".lan"
	got := copyDomain(long, 1)
	if model.HostDomainError(got) != "" {
		t.Errorf("long copy invalid: %q (%s)", got, model.HostDomainError(got))
	}
}

func TestDuplicateDomainsSkipsTaken(t *testing.T) {
	hosts := []model.ProxyHost{
		{Meta: model.Meta{ID: "a"}, Domains: []string{"grafana.home.lan"}},
		{Meta: model.Meta{ID: "b"}, Domains: []string{"copy-grafana.home.lan"}},
	}
	got := duplicateDomains([]string{"grafana.home.lan", "g.home.lan"}, hosts, nil)
	if !slices.Equal(got, []string{"copy2-grafana.home.lan", "copy-g.home.lan"}) {
		t.Errorf("got %v", got)
	}
}

func TestFindDomainOwner(t *testing.T) {
	hosts := []model.ProxyHost{{Meta: model.Meta{ID: "h1"}, Domains: []string{"home.lan", "www.home.lan"}}}
	redirects := []model.Redirect{
		{Meta: model.Meta{ID: "r1"}, Domains: []string{"old.home.lan"}},
		{Meta: model.Meta{ID: "r2"}, Domains: []string{"home.lan"}, FromPath: "/old-blog"},
	}
	type tc struct {
		name, domain, kind, exclude, from string
		want                              string // owner id or ""
	}
	for _, c := range []tc{
		{"host vs host", "www.home.lan", "host", "", "", "h1"},
		{"host excludes itself", "home.lan", "host", "h1", "", ""},
		{"host vs whole redirect", "old.home.lan", "host", "", "", "r1"},
		{"host ignores path redirect", "home.lan", "host", "h1", "", ""},
		{"whole redirect vs host", "home.lan", "redirect", "", "", "h1"},
		{"whole redirect vs path redirect", "old.home.lan", "redirect", "", "/", "r1"},
		{"path redirect next to host", "home.lan", "redirect", "", "/other", ""},
		{"same path redirect", "home.lan", "redirect", "", "/old-blog", "r2"},
		{"path redirect excludes itself", "home.lan", "redirect", "r2", "/old-blog", ""},
		{"path redirect vs whole redirect", "old.home.lan", "redirect", "", "/x", "r1"},
		{"free", "new.home.lan", "host", "", "", ""},
	} {
		o := findDomainOwner(hosts, redirects, c.domain, c.kind, c.exclude, c.from)
		got := ""
		if o != nil {
			got = o.ID
		}
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestCheckHostRefs(t *testing.T) {
	rf := &refs{
		hosts:       []model.ProxyHost{{Meta: model.Meta{ID: "other"}, Domains: []string{"taken.home.lan"}}},
		accessLists: map[string]string{"lan": "lan-only"},
		certs:       map[string]model.Certificate{"c1": {Name: "*.home.lan"}},
		backends:    map[string]string{},
	}
	h := &model.ProxyHost{
		Meta:          model.Meta{ID: "me"},
		Domains:       []string{"free.home.lan", "taken.home.lan"},
		AccessListID:  "gone",
		CertificateID: "c2",
		Locations:     []model.Location{{Path: "/a", AccessListID: "lan"}, {Path: "/b", AccessListID: "nope"}},
		RateLimit:     model.RateLimit{ExemptAccessListID: "nope"},
		Upstream:      model.Upstream{BackendID: "b1"},
	}
	e := model.Errs{}
	checkHostRefs(e, rf, h)
	for _, k := range []string{"domains.1", "accessListId", "certificateId", "locations.1.accessListId", "rateLimit.exemptAccessListId", "upstream.backendId"} {
		if _, ok := e[k]; !ok {
			t.Errorf("missing %s: %v", k, e)
		}
	}
	if !strings.Contains(e["domains.1"], "host taken.home.lan") {
		t.Errorf("conflict message: %q", e["domains.1"])
	}
	if _, ok := e["domains.0"]; ok {
		t.Error("free domain flagged")
	}
	if _, ok := e["locations.0.accessListId"]; ok {
		t.Error("existing access list flagged")
	}
}

func TestPrepareHostSystem(t *testing.T) {
	rf := &refs{accessLists: map[string]string{}, certs: map[string]model.Certificate{}, backends: map[string]string{}}
	prev := &model.ProxyHost{Meta: model.Meta{ID: "sys"}, System: true, Enabled: true, Domains: []string{"proxy.home.lan"},
		Upstream: model.Upstream{Scheme: "http", Host: "127.0.0.1", Port: 8181}}
	next := *prev
	next.System = false
	next.Domains = []string{"other.home.lan"}
	next.Enabled = false
	err := prepareHost(rf, prev, &next)
	if err == nil {
		t.Fatal("expected error")
	}
	var ve *model.ValidationError
	if !asValidation(err, &ve) || ve.Fields["domains"] == "" || ve.Fields["enabled"] == "" {
		t.Errorf("unexpected: %v", err)
	}
	if !next.System {
		t.Error("system flag must be preserved")
	}

	created := model.ProxyHost{System: true, Enabled: true, Domains: []string{"x.home.lan"}, Upstream: model.Upstream{Scheme: "http", Host: "10.0.0.1", Port: 80}}
	if err := prepareHost(rf, nil, &created); err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.System {
		t.Error("clients must not create system hosts")
	}
}

func asValidation(err error, ve **model.ValidationError) bool {
	v, ok := err.(*model.ValidationError)
	if ok {
		*ve = v
	}
	return ok
}

func TestCertUsers(t *testing.T) {
	hosts := []model.ProxyHost{
		{Meta: model.Meta{ID: "a"}, Domains: []string{"a.home.lan"}, CertificateID: "c"},
		{Meta: model.Meta{ID: "b"}, Domains: []string{"b.home.lan"}, CertificateID: "c"},
	}
	redirects := []model.Redirect{{Domains: []string{"www.home.lan"}, FromPath: "/x", CertificateID: "c"}}
	got := certUsers("c", "a", hosts, redirects, model.DefaultHostSettings{CertificateID: "c"}, model.GeneralSettings{}, model.DockerSettings{})
	want := []string{"b.home.lan", "redirect www.home.lan/x", "default TLS certificate"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
	if n := certUsers("c", "a", hosts[:1], nil, model.DefaultHostSettings{}, model.GeneralSettings{}, model.DockerSettings{}); len(n) != 0 {
		t.Errorf("expected unused, got %v", n)
	}
}
