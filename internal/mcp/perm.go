package mcp

import (
	"strings"

	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// Tool kinds shown in the settings table.
const (
	KindRead  = "read"
	KindWrite = "write"
)

// writeTools change configuration or runtime state. Everything else in
// store.MCPTools is read-only.
var writeTools = map[string]bool{
	"create_host":         true,
	"update_host":         true,
	"delete_host":         true,
	"drain_server":        true,
	"request_certificate": true,
	"apply_changes":       true,
}

// IsWriteTool reports whether name is a write tool.
func IsWriteTool(name string) bool { return writeTools[name] }

// EffectivePermission resolves the permission for a tool from settings,
// falling back to the default in store.MCPTools. Values that make no sense
// for the tool's kind are coerced to the safe choice: a write tool can never
// run as "read" (it becomes "confirm"), a read tool never needs confirmation.
// Unknown tools are disabled.
func EffectivePermission(s model.MCPSettings, name string) string {
	def, known := store.MCPTools[name]
	if !known {
		return model.ToolDisabled
	}
	p := strings.TrimSpace(s.Tools[name])
	if p == "" {
		p = def
	}
	if IsWriteTool(name) {
		switch p {
		case model.ToolAllow, model.ToolConfirm, model.ToolDisabled:
			return p
		case model.ToolRead:
			return model.ToolConfirm
		}
		if def == model.ToolRead {
			return model.ToolConfirm
		}
		return def
	}
	switch p {
	case model.ToolRead, model.ToolDisabled:
		return p
	case model.ToolAllow, model.ToolConfirm:
		return model.ToolRead
	}
	return def
}

// ---------------------------------------------------------------- token scope

// scope holds a token's limitTo patterns (domain globs like *.home.lan and
// backend/stream names). An empty scope is unrestricted.
type scope []string

func newScope(patterns []string) scope {
	out := scope{}
	for _, p := range patterns {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (sc scope) limited() bool { return len(sc) > 0 }

// allows reports whether a single name (domain, backend, stream) is in scope.
func (sc scope) allows(name string) bool {
	if !sc.limited() {
		return true
	}
	for _, p := range sc {
		if globMatch(p, name) {
			return true
		}
	}
	return false
}

// allowsAll reports whether every name is in scope (a host is only in scope
// when all of its domains are).
func (sc scope) allowsAll(names []string) bool {
	if !sc.limited() {
		return true
	}
	if len(names) == 0 {
		return false
	}
	for _, n := range names {
		if !sc.allows(n) {
			return false
		}
	}
	return true
}

func (sc scope) String() string { return strings.Join(sc, ", ") }

// globMatch matches s against a case-insensitive glob where * matches any run
// of characters (including dots) and ? matches exactly one character. A
// literal "*" in s (a wildcard certificate) is matched by "*" in the pattern.
func globMatch(pattern, s string) bool {
	p := []rune(strings.ToLower(strings.TrimSpace(pattern)))
	t := []rune(strings.ToLower(strings.TrimSpace(s)))
	pi, ti := 0, 0
	star, mark := -1, 0
	for ti < len(t) {
		switch {
		case pi < len(p) && (p[pi] == '?' || (p[pi] != '*' && p[pi] == t[ti])):
			pi++
			ti++
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, ti
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			ti = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}
