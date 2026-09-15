package api

import (
	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/geoip"
)

func (s *Server) routesGeoIP(r chi.Router) { geoip.Routes(s.app, r) }
