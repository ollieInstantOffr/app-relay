package api

import (
	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/logs"
)

func (s *Server) routesObserve(r chi.Router) { logs.Routes(s.app, r) }
