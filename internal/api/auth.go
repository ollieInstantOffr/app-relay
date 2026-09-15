package api

import (
	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/auth"
)

func (s *Server) routesAuth(r chi.Router) { auth.PublicRoutes(s.app, r) }

func (s *Server) routesUsers(r chi.Router) { auth.Routes(s.app, r) }
