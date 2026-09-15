package api

import (
	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/acme"
)

func (s *Server) routesCerts(r chi.Router) { acme.Routes(s.app, r) }
