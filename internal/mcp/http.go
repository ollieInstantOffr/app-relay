package mcp

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// AdminRoutes registers the MCP admin API: approvals, recent tool calls (runs behind auth).
func AdminRoutes(app *core.App, r chi.Router) {
	s, _ := app.MCP.(*Service)
	if s == nil {
		s = New(app)
	}
	httpx.SettingsHooks[model.SettingsMCP] = &httpx.SettingsHook{
		BeforeSave: func(r *http.Request, prev, next any) error {
			n, ok := next.(*model.MCPSettings)
			if !ok {
				return nil
			}
			return s.normalizeSettings(r, n)
		},
		AfterSave: func(r *http.Request, prev, next any) { s.notifyToolListChanged() },
	}

	r.Get("/approvals", s.handleListApprovals)
	r.Post("/approvals/{id}/approve", s.handleDecide(true))
	r.Post("/approvals/{id}/deny", s.handleDecide(false))
	r.Get("/mcp/calls", s.handleCalls)
	r.Get("/mcp/info", s.handleInfo)
	r.Get("/mcp/tools", s.handleTools)
}

// normalizeSettings validates and fills MCP settings before they are saved.
func (s *Service) normalizeSettings(r *http.Request, n *model.MCPSettings) error {
	e := model.Errs{}
	transports := []string{}
	for _, t := range n.Transports {
		t = strings.ToLower(strings.TrimSpace(t))
		switch t {
		case "http", "stdio":
			if !hasString(transports, t) {
				transports = append(transports, t)
			}
		default:
			e.Add("transports", "Unknown transport %q", t)
		}
	}
	sort.Strings(transports)
	n.Transports = transports
	if n.Enabled && len(transports) == 0 {
		e.Add("transports", "Pick at least one transport")
	}
	if n.ApprovalTimeoutMinutes < 1 || n.ApprovalTimeoutMinutes > 1440 {
		e.Add("approvalTimeoutMinutes", "Between 1 and 1440 minutes")
	}
	n.AccessListID = strings.TrimSpace(n.AccessListID)
	if n.AccessListID != "" {
		if _, err := s.app.Store.AccessLists().Get(r.Context(), n.AccessListID); err != nil {
			e.Add("accessListId", "Access list not found")
		}
	}
	tools := map[string]string{}
	for name, def := range store.MCPTools {
		tools[name] = def
	}
	for name, p := range n.Tools {
		if _, ok := store.MCPTools[name]; !ok {
			continue
		}
		valid := p == model.ToolDisabled
		if IsWriteTool(name) {
			valid = valid || p == model.ToolAllow || p == model.ToolConfirm
		} else {
			valid = valid || p == model.ToolRead
		}
		if !valid {
			e.Add("tools."+name, "%q is not a valid permission for %s", p, name)
			continue
		}
		tools[name] = p
	}
	n.Tools = tools
	return e.Err()
}

func (s *Service) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	switch status {
	case "", store.ApprovalPending:
		status = store.ApprovalPending
		s.expireDue(r.Context())
	case "all", store.ApprovalApproved, store.ApprovalDenied, store.ApprovalExpired:
	default:
		httpx.Fail(w, r, httpx.Errorf(http.StatusBadRequest, "bad_request", "status must be pending, approved, denied, expired or all"))
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := s.app.Store.MCPListApprovals(r.Context(), status, limit)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, items)
}

func (s *Service) handleDecide(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor := httpx.Actor(r)
		if actor.Type != core.ActorUser || (actor.Role != core.RoleAdmin && actor.Role != core.RoleEditor) {
			httpx.Fail(w, r, httpx.Errorf(http.StatusForbidden, "forbidden", "only signed-in admins and editors can decide approvals"))
			return
		}
		a, out, err := s.Decide(r.Context(), chi.URLParam(r, "id"), approve, actor)
		if err != nil {
			httpx.Fail(w, r, decideError(err))
			return
		}
		httpx.WriteJSON(w, http.StatusOK, decisionResponse{Approval: *a, Failed: out != nil && out.IsError})
	}
}

func (s *Service) handleCalls(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	rows, err := s.app.Store.ListAudit(r.Context(), store.AuditQuery{ActorType: core.ActorMCP, Limit: limit})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, rows)
}

type mcpInfo struct {
	Endpoint          string   `json:"endpoint"`
	Enabled           bool     `json:"enabled"`
	Transports        []string `json:"transports"`
	ToolCount         int      `json:"toolCount"`
	ConnectedSessions int      `json:"connectedSessions"`
	StdioCommand      string   `json:"stdioCommand"`
}

func (s *Service) handleInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.settings(ctx)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	general, err := store.LoadSettings[model.GeneralSettings](ctx, s.app.Store, model.SettingsGeneral)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	endpoint := ""
	if d := strings.TrimSpace(general.AdminDomain); d != "" {
		endpoint = "https://" + d + "/mcp"
	} else {
		scheme := "http"
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			scheme = "https"
		}
		endpoint = fmt.Sprintf("%s://%s/mcp", scheme, r.Host)
	}
	count := 0
	for _, t := range s.catalog {
		if EffectivePermission(st, t.Name) != model.ToolDisabled {
			count++
		}
	}
	transports := st.Transports
	if transports == nil {
		transports = []string{}
	}
	httpx.WriteJSON(w, http.StatusOK, mcpInfo{
		Endpoint:          endpoint,
		Enabled:           st.Enabled,
		Transports:        transports,
		ToolCount:         count,
		ConnectedSessions: s.ConnectedSessions(),
		StdioCommand:      "docker exec -i relay relay mcp-stdio --token rl_mcp_…",
	})
}

type toolRow struct {
	toolInfo
	DefaultPermission string `json:"defaultPermission"`
	Permission        string `json:"permission"`
}

func (s *Service) handleTools(w http.ResponseWriter, r *http.Request) {
	st, err := s.settings(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	rows := make([]toolRow, 0, len(s.catalog))
	for _, t := range s.catalog {
		rows = append(rows, toolRow{toolInfo: t, DefaultPermission: store.MCPTools[t.Name], Permission: EffectivePermission(st, t.Name)})
	}
	httpx.WriteJSON(w, http.StatusOK, rows)
}
