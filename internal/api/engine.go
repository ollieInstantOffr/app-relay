package api

import (
	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/apply"
)

func (s *Server) routesEngine(r chi.Router) { apply.Routes(s.app, r) }
