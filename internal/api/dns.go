package api

import (
	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/publicdns"
)

func (s *Server) routesDNS(r chi.Router) { publicdns.Routes(s.app, r) }
