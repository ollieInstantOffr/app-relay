package api

import (
	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/hostsapi"
)

func (s *Server) routesHosts(r chi.Router) { hostsapi.Routes(s.app, r) }
