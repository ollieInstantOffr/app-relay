package api

import (
	"net/http"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
)

var (
	writeJSON  = httpx.WriteJSON
	writeError = httpx.WriteError
	decode     = httpx.Decode
)

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) { httpx.Fail(w, r, err) }

func actorOf(r *http.Request) core.Actor { return httpx.Actor(r) }
