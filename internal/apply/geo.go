package apply

import (
	"context"

	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render"
)

// geoEnv points the renderers at the country database (Relay Edge) and the
// geo include with the networks of the allowed countries (nginx). The
// database is downloaded on first use; until then geo-blocking is skipped
// with a note in the rendered config.
func (s *Service) geoEnv(ctx context.Context, env *render.Env, snap *model.Snapshot) {
	g := s.app.GeoIP
	countries := model.GeoCountries(snap.Hosts)
	if g == nil || len(countries) == 0 {
		return
	}
	if err := g.Ensure(ctx); err != nil {
		s.log.Warn("geo-blocking skipped: no country database", "err", err)
		return
	}
	env.GeoIPCountry = g.Database()
	if env.GeoIPCountry == "" {
		return
	}
	f, err := g.CountryFile(ctx, countries)
	if err != nil {
		s.log.Warn("geo-blocking skipped for nginx: country file", "err", err)
		return
	}
	env.GeoCountryFile = f
}
