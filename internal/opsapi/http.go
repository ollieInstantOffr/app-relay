// Package opsapi is the ops slice API: docker discovery, notifications,
// backups and the Nginx Proxy Manager import.
package opsapi

import (
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/backup"
	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/docker"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/notify"
	"github.com/instantoffr/relay/internal/npmimport"
	"github.com/instantoffr/relay/internal/store"
)

func unavailable(what string) error {
	return httpx.Errorf(http.StatusServiceUnavailable, "unavailable", what+" is not available")
}

// decodeOptional decodes a JSON body when one was sent.
func decodeOptional(r *http.Request, v any) error {
	if r.ContentLength == 0 {
		return nil
	}
	if err := httpx.Decode(r, v); err != nil {
		if he, ok := err.(*httpx.HTTPError); ok && he.Message == "request body required" {
			return nil
		}
		return err
	}
	return nil
}

// Routes registers the ops slice API (runs behind auth).
func Routes(app *core.App, r chi.Router) {
	backup.RegisterSettingsHook()
	notify.RegisterSettingsHook()
	dockerSvc, _ := app.Docker.(*docker.Service)
	notifySvc, _ := app.Notify.(*notify.Service)
	backupSvc, _ := app.Backup.(*backup.Service)
	importSvc, _ := app.Importer.(*npmimport.Service)
	if dockerSvc != nil {
		dockerSvc.RegisterSettingsHook()
	}

	// ------------------------------------------------------------ docker
	r.Get("/docker/status", func(w http.ResponseWriter, r *http.Request) {
		if dockerSvc == nil {
			set, err := store.LoadSettings[model.DockerSettings](r.Context(), app.Store, model.SettingsDocker)
			if err != nil {
				httpx.Fail(w, r, err)
				return
			}
			httpx.WriteJSON(w, http.StatusOK, docker.Status{Enabled: set.Enabled, Endpoint: set.Endpoint, Error: "Docker discovery is not available in this build", Endpoints: []docker.EndpointStatus{}})
			return
		}
		httpx.WriteJSON(w, http.StatusOK, dockerSvc.Status(r.Context()))
	})

	r.Get("/docker/containers", func(w http.ResponseWriter, r *http.Request) {
		if app.Docker == nil {
			httpx.WriteJSON(w, http.StatusOK, []core.Container{})
			return
		}
		list, err := app.Docker.Containers(r.Context())
		if err != nil {
			// Not connected / disabled is reported by /docker/status; the list is just empty.
			if dockerSvc == nil || !dockerSvc.Status(r.Context()).Connected {
				httpx.WriteJSON(w, http.StatusOK, []core.Container{})
				return
			}
			httpx.Fail(w, r, httpx.Errorf(http.StatusBadGateway, "docker_error", err.Error()))
			return
		}
		httpx.WriteJSON(w, http.StatusOK, list)
	})

	r.Post("/docker/retry", func(w http.ResponseWriter, r *http.Request) {
		if dockerSvc == nil {
			httpx.Fail(w, r, unavailable("Docker discovery"))
			return
		}
		var body struct {
			EndpointID string `json:"endpointId"`
		}
		if err := decodeOptional(r, &body); err != nil {
			httpx.Fail(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, dockerSvc.Retry(r.Context(), body.EndpointID))
	})

	r.Post("/docker/endpoints/test", httpx.RequireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if dockerSvc == nil {
			httpx.Fail(w, r, unavailable("Docker discovery"))
			return
		}
		var body struct {
			Endpoint model.DockerEndpoint `json:"endpoint"`
		}
		if err := httpx.Decode(r, &body); err != nil {
			httpx.Fail(w, r, err)
			return
		}
		res, err := dockerSvc.TestEndpoint(r.Context(), body.Endpoint)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, res)
	}))

	r.Post("/docker/hosts", func(w http.ResponseWriter, r *http.Request) {
		if dockerSvc == nil {
			httpx.Fail(w, r, unavailable("Docker discovery"))
			return
		}
		var req docker.CreateRequest
		if err := httpx.Decode(r, &req); err != nil {
			httpx.Fail(w, r, err)
			return
		}
		res, err := dockerSvc.CreateHosts(r, req)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, res)
	})

	r.Post("/docker/backends/{backendId}/servers", func(w http.ResponseWriter, r *http.Request) {
		if dockerSvc == nil {
			httpx.Fail(w, r, unavailable("Docker discovery"))
			return
		}
		var body struct {
			EndpointID  string `json:"endpointId"`
			ContainerID string `json:"containerId"`
			Port        int    `json:"port"`
		}
		if err := httpx.Decode(r, &body); err != nil {
			httpx.Fail(w, r, err)
			return
		}
		b, err := dockerSvc.AddToBackend(r, chi.URLParam(r, "backendId"), body.EndpointID, body.ContainerID, body.Port)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, b)
	})

	// ------------------------------------------------------------ notifications
	r.Post("/notifications/test", httpx.RequireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if notifySvc == nil {
			httpx.Fail(w, r, unavailable("Notifications"))
			return
		}
		var body struct {
			ChannelID string                     `json:"channelId"`
			Channel   *model.NotificationChannel `json:"channel"`
		}
		if err := httpx.Decode(r, &body); err != nil {
			httpx.Fail(w, r, err)
			return
		}
		set, err := store.LoadSettings[model.NotificationSettings](r.Context(), app.Store, model.SettingsNotifications)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		var ch model.NotificationChannel
		switch {
		case body.Channel != nil:
			ch = *body.Channel
			notify.MergeSecrets(set.Channels, &ch)
		case body.ChannelID != "":
			found := false
			for _, c := range set.Channels {
				if c.ID == body.ChannelID {
					ch, found = c, true
				}
			}
			if !found {
				httpx.Fail(w, r, store.ErrNotFound)
				return
			}
		default:
			httpx.Fail(w, r, httpx.Errorf(http.StatusBadRequest, "bad_request", "channelId or channel is required"))
			return
		}
		if err := notifySvc.Test(r.Context(), ch); err != nil {
			if _, ok := err.(*model.ValidationError); ok {
				httpx.Fail(w, r, err)
				return
			}
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	r.Get("/notifications/log", func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		rows, err := app.Store.ListNotificationLog(r.Context(), limit)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, rows)
	})

	// ------------------------------------------------------------ backups
	r.Get("/backups", func(w http.ResponseWriter, r *http.Request) {
		if backupSvc == nil {
			httpx.Fail(w, r, unavailable("Backups"))
			return
		}
		rows, err := app.Store.ListBackups(r.Context())
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": rows, "status": backupSvc.Status(r.Context())})
	})

	r.Post("/backups", httpx.RequireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if backupSvc == nil {
			httpx.Fail(w, r, unavailable("Backups"))
			return
		}
		row, err := backupSvc.Create(r.Context(), backup.TriggerManual, "")
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		app.Audit(r.Context(), core.AuditEntry{Action: "backup.create", Target: row.File, Detail: fmt.Sprintf("%d bytes", row.Size), Result: "ok"})
		httpx.WriteJSON(w, http.StatusCreated, row)
	}))

	r.Get("/backups/{id}/download", httpx.RequireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if backupSvc == nil {
			httpx.Fail(w, r, unavailable("Backups"))
			return
		}
		rc, size, row, err := backupSvc.Open(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		defer rc.Close()
		app.Audit(r.Context(), core.AuditEntry{Action: "backup.download", Target: row.File, Result: "ok"})
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filepath.Base(row.File)))
		if size > 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		}
		_, _ = io.Copy(w, rc)
	}))

	r.Delete("/backups/{id}", httpx.RequireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if backupSvc == nil {
			httpx.Fail(w, r, unavailable("Backups"))
			return
		}
		id := chi.URLParam(r, "id")
		row, err := app.Store.GetBackup(r.Context(), id)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		if err := backupSvc.Delete(r.Context(), id); err != nil {
			httpx.Fail(w, r, err)
			return
		}
		app.Audit(r.Context(), core.AuditEntry{Action: "backup.delete", Target: row.File, Result: "ok"})
		w.WriteHeader(http.StatusNoContent)
	}))

	r.Post("/backups/{id}/restore", httpx.RequireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if backupSvc == nil {
			httpx.Fail(w, r, unavailable("Backups"))
			return
		}
		var body struct {
			Passphrase string `json:"passphrase"`
		}
		if err := decodeOptional(r, &body); err != nil {
			httpx.Fail(w, r, err)
			return
		}
		res, err := backupSvc.RestoreByID(r.Context(), chi.URLParam(r, "id"), body.Passphrase)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, res)
	}))

	r.Post("/backups/restore", httpx.RequireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if backupSvc == nil {
			httpx.Fail(w, r, unavailable("Backups"))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4<<30)
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			httpx.Fail(w, r, httpx.Errorf(http.StatusBadRequest, "bad_request", "upload a .relay.age file: "+err.Error()))
			return
		}
		defer r.MultipartForm.RemoveAll()
		f, hdr, err := r.FormFile("file")
		if err != nil {
			httpx.Fail(w, r, httpx.Errorf(http.StatusBadRequest, "bad_request", "file is required"))
			return
		}
		defer f.Close()
		if !strings.HasSuffix(strings.ToLower(hdr.Filename), ".age") {
			httpx.Fail(w, r, httpx.Errorf(http.StatusBadRequest, "bad_request", "expected a .relay.age backup archive"))
			return
		}
		res, err := backupSvc.Restore(r.Context(), f, r.FormValue("passphrase"))
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, res)
	}))

	r.Post("/backups/{id}/upload", httpx.RequireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if backupSvc == nil {
			httpx.Fail(w, r, unavailable("Backups"))
			return
		}
		row, err := backupSvc.Upload(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		app.Audit(r.Context(), core.AuditEntry{Action: "backup.upload", Target: row.File, Detail: row.RemoteKey, Result: "ok"})
		httpx.WriteJSON(w, http.StatusOK, row)
	}))

	// Off-site (S3) destination.
	r.Post("/backups/s3/test", httpx.RequireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if backupSvc == nil {
			httpx.Fail(w, r, unavailable("Backups"))
			return
		}
		var body model.BackupS3Settings
		if err := httpx.Decode(r, &body); err != nil {
			httpx.Fail(w, r, err)
			return
		}
		res, err := backupSvc.TestS3(r.Context(), body)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, res)
	}))

	r.Get("/backups/remote", httpx.RequireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if backupSvc == nil {
			httpx.Fail(w, r, unavailable("Backups"))
			return
		}
		items, err := backupSvc.RemoteList(r.Context())
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
	}))

	r.Post("/backups/remote/restore", httpx.RequireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if backupSvc == nil {
			httpx.Fail(w, r, unavailable("Backups"))
			return
		}
		var body struct {
			Key        string `json:"key"`
			Passphrase string `json:"passphrase"`
		}
		if err := httpx.Decode(r, &body); err != nil {
			httpx.Fail(w, r, err)
			return
		}
		res, err := backupSvc.RestoreRemote(r.Context(), body.Key, body.Passphrase)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, res)
	}))

	// ------------------------------------------------------------ NPM import
	r.Post("/import/npm/preview", httpx.RequireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if importSvc == nil {
			httpx.Fail(w, r, unavailable("Import"))
			return
		}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			r.Body = http.MaxBytesReader(w, r.Body, 1<<30)
			if err := r.ParseMultipartForm(32 << 20); err != nil {
				httpx.Fail(w, r, httpx.Errorf(http.StatusBadRequest, "bad_request", "upload database.sqlite: "+err.Error()))
				return
			}
			defer r.MultipartForm.RemoveAll()
			f, hdr, err := r.FormFile("database")
			if err != nil {
				httpx.Fail(w, r, httpx.Errorf(http.StatusBadRequest, "bad_request", "database file is required"))
				return
			}
			defer f.Close()
			pv, err := importSvc.PreviewUpload(r.Context(), f, hdr.Filename)
			if err != nil {
				httpx.Fail(w, r, err)
				return
			}
			httpx.WriteJSON(w, http.StatusOK, pv)
			return
		}
		var body struct {
			Path string `json:"path"`
		}
		if err := httpx.Decode(r, &body); err != nil {
			httpx.Fail(w, r, err)
			return
		}
		pv, err := importSvc.PreviewPath(r.Context(), body.Path)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, pv)
	}))

	r.Post("/import/npm/commit", httpx.RequireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if importSvc == nil {
			httpx.Fail(w, r, unavailable("Import"))
			return
		}
		var body struct {
			Token     string `json:"token"`
			Overwrite bool   `json:"overwrite"`
		}
		if err := httpx.Decode(r, &body); err != nil {
			httpx.Fail(w, r, err)
			return
		}
		res, err := importSvc.Commit(r, body.Token, body.Overwrite)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, res)
	}))
}
