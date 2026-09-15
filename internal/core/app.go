// Package core wires Relay's services together. Feature packages import core
// and implement the interfaces declared in core/<area>.go; cmd/relay assigns
// the implementations to App.
package core

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/store"
)

var ErrNotImplemented = errors.New("not implemented")

// Service is a component with background work. Start must return promptly
// (spawn goroutines bound to ctx).
type Service interface {
	Start(ctx context.Context) error
}

type Config struct {
	Version  string
	DataDir  string // /data
	RunDir   string // /run/relay (agent sockets)
	LogDir   string // /var/log/relay (nginx logs)
	Listen   string // :8181
	DevMode  bool   // allow CORS from the Vite dev server, verbose logs
	DockerOK bool
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func ConfigFromEnv(version string) Config {
	return Config{
		Version: version,
		DataDir: getenv("RELAY_DATA_DIR", "/data"),
		RunDir:  getenv("RELAY_RUN_DIR", "/run/relay"),
		LogDir:  getenv("RELAY_LOG_DIR", "/var/log/relay"),
		Listen:  getenv("RELAY_LISTEN", ":8181"),
		DevMode: os.Getenv("RELAY_DEV") == "1",
	}
}

type App struct {
	Config Config
	Store  *store.Store
	Bus    *events.Bus
	Log    *slog.Logger

	Nginx   *agent.Client
	HAProxy *agent.Client

	Auth     Auth
	Engine   Engine
	LB       LoadBalancer
	Certs    Certs
	Logs     Logs
	Health   Health
	Docker   Docker
	Notify   Notifier
	Backup   Backups
	Importer Importer
	MCP      MCP
	Engines  Engines // engine image version checks & upgrades (core/engines_ext.go)
}

func New(cfg Config, st *store.Store, bus *events.Bus, log *slog.Logger) *App {
	return &App{
		Config:  cfg,
		Store:   st,
		Bus:     bus,
		Log:     log,
		Nginx:   agent.NewClient(agent.EngineNginx, agent.SocketPath(cfg.RunDir, agent.EngineNginx)),
		HAProxy: agent.NewClient(agent.EngineHAProxy, agent.SocketPath(cfg.RunDir, agent.EngineHAProxy)),
	}
}

// ChangeAction values for Changed.
const (
	ActionCreated = "created"
	ActionUpdated = "updated"
	ActionDeleted = "deleted"
)

// ConfigChange is the payload of events.ConfigChanged.
type ConfigChange struct {
	Kind   string `json:"kind"` // model.Kind* or "settings:<key>"
	ID     string `json:"id"`
	Name   string `json:"name"`
	Action string `json:"action"`
}

// Changed must be called after any write to configuration that affects the
// rendered nginx/haproxy config. The engine recomputes pending changes.
func (a *App) Changed(ctx context.Context, kind, id, name, action string) {
	a.Bus.Publish(events.ConfigChanged, ConfigChange{Kind: kind, ID: id, Name: name, Action: action})
}

// ClientIP returns the client address. X-Forwarded-For / X-Real-IP are only
// trusted from loopback (the admin UI host proxied by our own nginx).
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		if v := r.Header.Get("X-Real-IP"); v != "" {
			return strings.TrimSpace(v)
		}
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			parts := strings.Split(v, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	return host
}
