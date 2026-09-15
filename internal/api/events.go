package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// GET /api/events — Server-Sent Events stream of bus events.
// ?topics=a,b limits to topic prefixes. High-volume "log." topics are only
// sent when explicitly requested.
func (s *Server) routesEvents(r chi.Router) {
	r.Get("/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeError(w, http.StatusInternalServerError, "no_stream", "streaming unsupported")
			return
		}
		var prefixes []string
		if t := r.URL.Query().Get("topics"); t != "" {
			prefixes = strings.Split(t, ",")
		}
		match := func(topic string) bool {
			if len(prefixes) == 0 {
				return !strings.HasPrefix(topic, "log.")
			}
			for _, p := range prefixes {
				if strings.HasPrefix(topic, p) {
					return true
				}
			}
			return false
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "retry: 3000\n\n")
		flusher.Flush()

		ch, cancel := s.app.Bus.Subscribe(512)
		defer cancel()
		tick := time.NewTicker(15 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-tick.C:
				fmt.Fprint(w, ": ping\n\n")
				flusher.Flush()
			case ev, ok := <-ch:
				if !ok {
					return
				}
				if !match(ev.Topic) {
					continue
				}
				b, err := json.Marshal(ev)
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "data: %s\n\n", b)
				flusher.Flush()
			}
		}
	})
}
