package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

type crudSpec[T any, PT interface {
	*T
	model.Entity
}] struct {
	path     string // "/hosts"
	noun     string // audit action prefix: "host"
	repo     func(*store.Store) *store.Repo[T, PT]
	hooks    *httpx.Hooks[T]
	name     func(*T) string
	noCreate bool // creation handled by a custom endpoint
	config   bool // writes change rendered config
}

func (s *Server) routesCRUD(r chi.Router) {
	mountCRUD(s, r, crudSpec[model.ProxyHost, *model.ProxyHost]{
		path: "/hosts", noun: "host", repo: (*store.Store).Hosts, hooks: &httpx.HostHooks, config: true,
		name: func(h *model.ProxyHost) string { return first(h.Domains) },
	})
	mountCRUD(s, r, crudSpec[model.Redirect, *model.Redirect]{
		path: "/redirects", noun: "redirect", repo: (*store.Store).Redirects, hooks: &httpx.RedirectHooks, config: true,
		name: func(v *model.Redirect) string { return first(v.Domains) + v.FromPath },
	})
	mountCRUD(s, r, crudSpec[model.Stream, *model.Stream]{
		path: "/streams", noun: "stream", repo: (*store.Store).Streams, hooks: &httpx.StreamHooks, config: true,
		name: func(v *model.Stream) string { return v.Name },
	})
	mountCRUD(s, r, crudSpec[model.AccessList, *model.AccessList]{
		path: "/access-lists", noun: "access_list", repo: (*store.Store).AccessLists, hooks: &httpx.AccessListHooks, config: true,
		name: func(v *model.AccessList) string { return v.Name },
	})
	mountCRUD(s, r, crudSpec[model.Certificate, *model.Certificate]{
		path: "/certificates", noun: "certificate", repo: (*store.Store).Certificates, hooks: &httpx.CertificateHooks, config: true, noCreate: true,
		name: func(v *model.Certificate) string { return v.Name },
	})
	mountCRUD(s, r, crudSpec[model.DNSProvider, *model.DNSProvider]{
		path: "/dns-providers", noun: "dns_provider", repo: (*store.Store).DNSProviders, hooks: &httpx.DNSProviderHooks,
		name: func(v *model.DNSProvider) string { return v.Name },
	})
	mountCRUD(s, r, crudSpec[model.Backend, *model.Backend]{
		path: "/backends", noun: "backend", repo: (*store.Store).Backends, hooks: &httpx.BackendHooks, config: true,
		name: func(v *model.Backend) string { return v.Name },
	})
	mountCRUD(s, r, crudSpec[model.Frontend, *model.Frontend]{
		path: "/frontends", noun: "frontend", repo: (*store.Store).Frontends, hooks: &httpx.FrontendHooks, config: true,
		name: func(v *model.Frontend) string { return v.Name },
	})
}

func first(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return ss[0]
}

func redacted[T any](v T) T {
	if rd, ok := any(&v).(model.Redactor); ok {
		rd.Redact()
	}
	return v
}

func mountCRUD[T any, PT interface {
	*T
	model.Entity
}](s *Server, r chi.Router, spec crudSpec[T, PT]) {
	repo := func() *store.Repo[T, PT] { return spec.repo(s.app.Store) }
	kind := repo().Kind()

	r.Get(spec.path, func(w http.ResponseWriter, r *http.Request) {
		items, err := repo().List(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		for i := range items {
			items[i] = redacted(items[i])
		}
		if spec.hooks.DecorateList != nil {
			writeJSON(w, http.StatusOK, spec.hooks.DecorateList(r, items))
			return
		}
		writeJSON(w, http.StatusOK, items)
	})

	r.Get(spec.path+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, err := repo().Get(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, redacted(*v))
	})

	save := func(w http.ResponseWriter, r *http.Request, prev *T, next *T) bool {
		if sk, ok := any(next).(model.SecretKeeper); ok {
			var p any
			if prev != nil {
				p = prev
			}
			if err := sk.KeepSecrets(p); err != nil {
				s.fail(w, r, err)
				return false
			}
		}
		if spec.hooks.BeforeSave != nil {
			if err := spec.hooks.BeforeSave(r, prev, next); err != nil {
				s.fail(w, r, err)
				return false
			}
		}
		if v, ok := any(next).(model.Validator); ok {
			if err := v.Validate(); err != nil {
				s.fail(w, r, err)
				return false
			}
		}
		return true
	}

	if !spec.noCreate {
		r.Post(spec.path, func(w http.ResponseWriter, r *http.Request) {
			next := new(T)
			if err := decode(r, next); err != nil {
				s.fail(w, r, err)
				return
			}
			PT(next).GetMeta().ID = ""
			if !save(w, r, nil, next) {
				return
			}
			if err := repo().Create(r.Context(), next); err != nil {
				s.fail(w, r, err)
				return
			}
			name := spec.name(next)
			s.app.Audit(r.Context(), core.AuditEntry{Action: spec.noun + ".create", Target: name, Result: "saved"})
			if spec.config {
				s.app.Changed(r.Context(), kind, PT(next).GetMeta().ID, name, core.ActionCreated)
			}
			if spec.hooks.AfterSave != nil {
				spec.hooks.AfterSave(r, nil, next)
			}
			writeJSON(w, http.StatusCreated, redacted(*next))
		})
	}

	r.Put(spec.path+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		prev, err := repo().Get(r.Context(), id)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		next := new(T)
		if err := decode(r, next); err != nil {
			s.fail(w, r, err)
			return
		}
		PT(next).GetMeta().ID = id
		if !save(w, r, prev, next) {
			return
		}
		if err := repo().Update(r.Context(), next); err != nil {
			s.fail(w, r, err)
			return
		}
		name := spec.name(next)
		s.app.Audit(r.Context(), core.AuditEntry{Action: spec.noun + ".update", Target: name, Result: "saved"})
		if spec.config {
			s.app.Changed(r.Context(), kind, id, name, core.ActionUpdated)
		}
		if spec.hooks.AfterSave != nil {
			spec.hooks.AfterSave(r, prev, next)
		}
		writeJSON(w, http.StatusOK, redacted(*next))
	})

	r.Delete(spec.path+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		cur, err := repo().Get(r.Context(), id)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if spec.hooks.BeforeDelete != nil {
			if err := spec.hooks.BeforeDelete(r, cur); err != nil {
				s.fail(w, r, err)
				return
			}
		}
		if err := repo().Delete(r.Context(), id); err != nil {
			s.fail(w, r, err)
			return
		}
		name := spec.name(cur)
		s.app.Audit(r.Context(), core.AuditEntry{Action: spec.noun + ".delete", Target: name, Result: "saved"})
		if spec.config {
			s.app.Changed(r.Context(), kind, id, name, core.ActionDeleted)
		}
		if spec.hooks.AfterDelete != nil {
			spec.hooks.AfterDelete(r, cur)
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
