package api

import (
	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/opsapi"
)

func (s *Server) routesOps(r chi.Router) { opsapi.Routes(s.app, r) }
