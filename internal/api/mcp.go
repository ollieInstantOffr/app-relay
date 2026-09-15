package api

import (
	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/mcp"
)

func (s *Server) routesMCPAdmin(r chi.Router) { mcp.AdminRoutes(s.app, r) }
