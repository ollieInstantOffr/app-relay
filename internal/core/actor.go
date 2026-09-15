package core

import (
	"context"
	"time"

	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/store"
)

const (
	ActorUser   = "user"
	ActorToken  = "token" // REST API token
	ActorMCP    = "mcp"   // MCP client via token
	ActorSystem = "system"
	ActorDocker = "docker"

	RoleAdmin  = "admin"
	RoleEditor = "editor"
	RoleViewer = "viewer"

	ScopeRead  = "read"
	ScopeWrite = "write"
)

// Actor is whoever performs an action.
type Actor struct {
	Type       string `json:"type"`
	ID         string `json:"id"`   // user id or token id
	Name       string `json:"name"` // username, token/client name
	Role       string `json:"role"` // effective role
	TokenID    string `json:"tokenId,omitempty"`
	Scope      string `json:"scope,omitempty"` // tokens: read | write
	ClientName string `json:"clientName,omitempty"`
	IP         string `json:"ip,omitempty"`
	SessionID  string `json:"-"`
}

var SystemActor = Actor{Type: ActorSystem, Name: "system", Role: RoleAdmin}
var DockerActor = Actor{Type: ActorDocker, Name: "docker", Role: RoleEditor}

func (a Actor) IsZero() bool  { return a.Type == "" }
func (a Actor) IsAdmin() bool { return a.Role == RoleAdmin }

// CanWrite reports whether the actor may change configuration.
func (a Actor) CanWrite() bool {
	if a.Type == ActorToken || a.Type == ActorMCP {
		return a.Scope == ScopeWrite
	}
	return a.Role == RoleAdmin || a.Role == RoleEditor
}

// Label is the short actor string used in config versions ("admin", "mcp:claude").
func (a Actor) Label() string {
	switch a.Type {
	case ActorMCP:
		return "mcp:" + a.Name
	case ActorToken:
		return "token:" + a.Name
	case "":
		return "system"
	}
	return a.Name
}

type actorKey struct{}

type internalCallKey struct{}

// WithInternalCall marks ctx as an in-process API request made on behalf of
// the actor already in ctx (MCP tools reusing the REST handlers). Only code in
// this process can set it; requests from the network never carry it.
func WithInternalCall(ctx context.Context) context.Context {
	return context.WithValue(ctx, internalCallKey{}, true)
}

// IsInternalCall reports whether ctx belongs to an in-process API request.
func IsInternalCall(ctx context.Context) bool {
	v, _ := ctx.Value(internalCallKey{}).(bool)
	return v
}

func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

func ActorFrom(ctx context.Context) Actor {
	if a, ok := ctx.Value(actorKey{}).(Actor); ok {
		return a
	}
	return Actor{}
}

// AuditEntry is one row of the audit log. Actor comes from ctx unless set.
type AuditEntry struct {
	Actor   *Actor
	Action  string // host.update, auth.login, drain_server …
	Target  string
	Detail  string
	Result  string // ok | applied | pending | denied | blocked | auto | reverted | failed
	Version *int64
}

func (a *App) Audit(ctx context.Context, e AuditEntry) {
	actor := ActorFrom(ctx)
	if e.Actor != nil {
		actor = *e.Actor
	}
	if actor.IsZero() {
		actor = SystemActor
	}
	if e.Result == "" {
		e.Result = "ok"
	}
	name := actor.Name
	if actor.Type == ActorMCP && actor.ClientName != "" {
		name = actor.ClientName
	}
	row := store.AuditRow{
		At: time.Now(), ActorType: actor.Type, ActorID: actor.ID, ActorName: name,
		Action: e.Action, Target: e.Target, Detail: e.Detail, Version: e.Version, Result: e.Result, IP: actor.IP,
	}
	id, err := a.Store.InsertAudit(context.WithoutCancel(ctx), row)
	if err != nil {
		a.Log.Error("audit insert", "err", err)
		return
	}
	row.ID = id
	a.Bus.Publish(events.AuditAppended, row)
}

// Activity records a dashboard activity item ("Certificate renewed for …").
func (a *App) Activity(ctx context.Context, kind, level, title, subject, detail string) {
	row := store.ActivityRow{At: time.Now(), Kind: kind, Level: level, Title: title, Subject: subject, Detail: detail}
	id, err := a.Store.InsertActivity(context.WithoutCancel(ctx), row)
	if err != nil {
		a.Log.Error("activity insert", "err", err)
		return
	}
	row.ID = id
	a.Bus.Publish(events.ActivityAppended, row)
}
