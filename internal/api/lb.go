package api

import (
	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/lb"
)

func (s *Server) routesLB(r chi.Router) { lb.Routes(s.app, r) }
