package edge

import (
	"slices"
	"strings"
	"testing"
)

func TestGeoBlockCountries(t *testing.T) {
	env := testEnv()
	env.GeoIPCountry = "/data/geoip/dbip-country-lite.mmdb"
	cfg, _ := mustRender(t, richSnapshot(), env)
	if cfg.GeoIPDatabase != env.GeoIPCountry {
		t.Fatalf("geoipDatabase = %q", cfg.GeoIPDatabase)
	}
	found := false
	for _, h := range cfg.Hosts {
		if slices.Contains(h.Domains, "cloud.home.lan") {
			found = true
			if !slices.Equal(h.AllowCountries, []string{"DE", "US"}) {
				t.Errorf("cloud allowCountries = %v", h.AllowCountries)
			}
		} else if len(h.AllowCountries) > 0 {
			t.Errorf("host %s: unexpected allowCountries %v", h.ID, h.AllowCountries)
		}
	}
	if !found {
		t.Fatal("cloud.home.lan not rendered")
	}
	for _, n := range cfg.Notes {
		if strings.Contains(n, "geo-blocking") {
			t.Errorf("unexpected note %q", n)
		}
	}

	// Without a database nothing is enforced and the config says why.
	cfg, _ = mustRender(t, richSnapshot(), testEnv())
	if cfg.GeoIPDatabase != "" {
		t.Fatal("no database, no geoipDatabase")
	}
}
