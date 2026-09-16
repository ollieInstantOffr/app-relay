package api

import (
	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/tunnels"
)

func (s *Server) routesTunnels(r chi.Router) { tunnels.Routes(s.app, r) }
