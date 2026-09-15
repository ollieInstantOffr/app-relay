package render

import (
	"strings"
	"testing"

	"github.com/instantoffr/relay/internal/model"
)

func TestErrorPageHTML(t *testing.T) {
	set := model.DefaultErrorPages()
	set.BrandName = "Acme <Labs>"
	set.AccentColor = "#ff0066"
	p := set.Pages["502"]
	p.Title = `Down <script>`
	set.Pages["502"] = p

	got := ErrorPageHTML(set, "502", nil)
	for _, want := range []string{"ERROR 502", "Down &lt;script&gt;", "Acme &lt;Labs&gt;", "--accent:#ff0066", "noindex"} {
		if !strings.Contains(got, want) {
			t.Errorf("502 page lacks %q", want)
		}
	}
	if strings.Contains(got, "<script>") {
		t.Fatal("title must be escaped")
	}

	// Missing text falls back to the built-in text; bad colours to the default.
	set.AccentColor = "red"
	set.Pages["404"] = model.ErrorPage{}
	if got := ErrorPageHTML(set, "404", nil); !strings.Contains(got, "Page not found") || !strings.Contains(got, DefaultAccent) {
		t.Fatalf("404 fallback: %s", got)
	}

	// Custom HTML is served as is.
	set.Pages["503"] = model.ErrorPage{HTML: "<h1>custom</h1>"}
	if got := ErrorPageHTML(set, "503", nil); got != "<h1>custom</h1>" {
		t.Fatalf("custom html = %q", got)
	}

	// A host's maintenance text wins and refreshes the page.
	m := &model.Maintenance{Enabled: true, Message: "Back at 18:00\nThanks"}
	got = ErrorPageHTML(set, "maintenance", m)
	if !strings.Contains(got, "Back at 18:00<br>Thanks") || !strings.Contains(got, `http-equiv="refresh"`) || strings.Contains(got, "ERROR") {
		t.Fatalf("maintenance page: %s", got)
	}
}

func TestPrepareSnapshotRelayLogin(t *testing.T) {
	snap := &model.Snapshot{Hosts: []model.ProxyHost{
		{Meta: model.Meta{ID: "wiki"}, Domains: []string{"wiki.example.com"}, Enabled: true,
			ForwardAuth: model.ForwardAuth{Enabled: true, Provider: model.ForwardAuthRelay},
			Locations:   []model.Location{{ID: "mine", Path: "/.relay/", Kind: model.LocationDeny}, {ID: "api", Path: "/api/", Kind: model.LocationSame}}},
		{Meta: model.Meta{ID: "other"}, Domains: []string{"other.example.com"}, Enabled: true,
			ForwardAuth: model.ForwardAuth{Enabled: true, Provider: model.ForwardAuthAuthelia, VerifyURL: "http://127.0.0.1:9091/api/verify"}},
	}}
	env := DefaultEnv("/data", "/run/relay", "/var/log/relay")
	env.AdminUpstream = "127.0.0.1:9000"

	out := PrepareSnapshot(snap, env)
	if snap.Hosts[0].ForwardAuth.VerifyURL != "" || len(snap.Hosts[0].Locations) != 2 {
		t.Fatal("the input snapshot must not change")
	}
	wiki := out.Hosts[0]
	if wiki.ForwardAuth.VerifyURL != "http://127.0.0.1:9000/.relay/verify?host=wiki" || wiki.ForwardAuth.SignInURL != "/.relay/login?host=wiki" {
		t.Fatalf("urls = %q %q", wiki.ForwardAuth.VerifyURL, wiki.ForwardAuth.SignInURL)
	}
	if len(wiki.Locations) != 2 || wiki.Locations[0].ID != "api" {
		t.Fatalf("locations = %+v", wiki.Locations)
	}
	portal := wiki.Locations[1]
	if portal.ID != PortalLocationID || portal.Path != PortalPrefix || !portal.NoAuth || portal.Upstream.Port != 9000 || portal.Upstream.Host != "127.0.0.1" {
		t.Fatalf("portal location = %+v", portal)
	}
	if out.Hosts[1].ForwardAuth.VerifyURL != "http://127.0.0.1:9091/api/verify" || len(out.Hosts[1].Locations) != 0 {
		t.Fatalf("other host changed: %+v", out.Hosts[1])
	}

	plain := &model.Snapshot{Hosts: []model.ProxyHost{{Meta: model.Meta{ID: "x"}}}}
	if PrepareSnapshot(plain, env) != plain {
		t.Fatal("snapshots without Relay login are returned as is")
	}
}
