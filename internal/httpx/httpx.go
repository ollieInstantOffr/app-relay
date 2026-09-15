// Package httpx holds HTTP helpers shared by the API server and the feature
// slices' route packages (so slices never need to import internal/api).
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

type errorBody struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

// HTTPError lets handlers and hooks return a specific status and code.
type HTTPError struct {
	Status  int
	Code    string
	Message string
}

func (e *HTTPError) Error() string { return e.Message }

func Errorf(status int, code, message string) error {
	return &HTTPError{Status: status, Code: code, Message: message}
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, map[string]errorBody{"error": {Code: code, Message: message}})
}

// Fail maps an error to an HTTP response.
func Fail(w http.ResponseWriter, r *http.Request, err error) {
	var he *HTTPError
	var ve *model.ValidationError
	var ua agent.ErrUnavailable
	switch {
	case errors.As(err, &he):
		WriteError(w, he.Status, he.Code, he.Message)
	case errors.As(err, &ve):
		WriteJSON(w, http.StatusUnprocessableEntity, map[string]errorBody{"error": {Code: "invalid", Message: ve.Error(), Fields: ve.Fields}})
	case errors.Is(err, store.ErrNotFound):
		WriteError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, store.ErrConflict):
		WriteError(w, http.StatusConflict, "conflict", "already exists")
	case errors.Is(err, core.ErrUnauthenticated):
		WriteError(w, http.StatusUnauthorized, "unauthenticated", "sign in required")
	case errors.Is(err, core.ErrForbidden):
		WriteError(w, http.StatusForbidden, "forbidden", "not allowed")
	case errors.Is(err, core.ErrNotImplemented):
		WriteError(w, http.StatusNotImplemented, "not_implemented", "not implemented yet")
	case errors.As(err, &ua):
		WriteError(w, http.StatusServiceUnavailable, "engine_unavailable", err.Error())
	case errors.Is(err, context.Canceled):
		WriteError(w, 499, "canceled", "request canceled")
	default:
		slog.Error("api error", "method", r.Method, "path", r.URL.Path, "err", err)
		WriteError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

// Decode reads a JSON body (max 16 MB). Empty-string timestamps sent by UI
// drafts ("createdAt": "", "notAfter": "") are treated as absent so they
// don't fail time.Time decoding.
func Decode(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		return Errorf(http.StatusBadRequest, "bad_request", "read body: "+err.Error())
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return Errorf(http.StatusBadRequest, "bad_request", "request body required")
	}
	if err := json.Unmarshal(body, v); err != nil {
		var te *time.ParseError
		if !errors.As(err, &te) {
			return Errorf(http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
		}
		cleaned, cerr := dropEmptyTimes(body)
		if cerr != nil {
			return Errorf(http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
		}
		if err := json.Unmarshal(cleaned, v); err != nil {
			return Errorf(http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
		}
	}
	return nil
}

// dropEmptyTimes removes object keys whose value is "" and whose name looks
// like a timestamp (…At, notBefore, notAfter), at any depth.
func dropEmptyTimes(body []byte) ([]byte, error) {
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	var walk func(x any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			for k, val := range t {
				if s, ok := val.(string); ok && s == "" && (strings.HasSuffix(k, "At") || k == "notBefore" || k == "notAfter") {
					delete(t, k)
					continue
				}
				walk(val)
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(doc)
	return json.Marshal(doc)
}

// Actor returns the authenticated actor for the request.
func Actor(r *http.Request) core.Actor { return core.ActorFrom(r.Context()) }

// RequireAdmin wraps a handler that only admins may call.
func RequireAdmin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !Actor(r).IsAdmin() {
			WriteError(w, http.StatusForbidden, "admin_only", "only admins can do this")
			return
		}
		h(w, r)
	}
}

// InUse builds a 409 error listing dependents ("used by grafana.home.lan, …").
func InUse(what string, names []string) error {
	return Errorf(http.StatusConflict, "in_use", what+" is used by "+strings.Join(names, ", "))
}
