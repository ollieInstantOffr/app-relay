package httpx

import (
	"net/http"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
)

// Hooks customise the generic CRUD endpoints for one entity kind. Feature
// slices assign them from their Routes/Register function (called at startup
// before the server handles requests) or from an init().
type Hooks[T any] struct {
	// BeforeSave may mutate next or reject the write. prev is nil on create.
	BeforeSave   func(r *http.Request, prev, next *T) error
	AfterSave    func(r *http.Request, prev, next *T)
	BeforeDelete func(r *http.Request, cur *T) error
	AfterDelete  func(r *http.Request, cur *T)
	// DecorateList returns what a list response serialises (items are redacted).
	DecorateList func(r *http.Request, items []T) any
}

var (
	HostHooks        Hooks[model.ProxyHost]
	RedirectHooks    Hooks[model.Redirect]
	StreamHooks      Hooks[model.Stream]
	AccessListHooks  Hooks[model.AccessList]
	CertificateHooks Hooks[model.Certificate]
	DNSProviderHooks Hooks[model.DNSProvider]
	BackendHooks     Hooks[model.Backend]
	FrontendHooks    Hooks[model.Frontend]
	GatewayHooks     Hooks[model.Gateway]
)

// SettingsHook customises one settings document. Values are pointers to the
// settings struct (e.g. *model.BackupSettings).
type SettingsHook struct {
	BeforeSave func(r *http.Request, prev, next any) error
	AfterSave  func(r *http.Request, prev, next any)
	// Decorate returns the response body for GET (defaults to the value).
	Decorate func(r *http.Request, v any) any
}

var SettingsHooks = map[string]*SettingsHook{}

// NetworkGuard optionally restricts the whole API/UI by client network
// (Settings → Users & access → Restrict admin UI). Set by the auth slice.
var NetworkGuard func(app *core.App, next http.Handler) http.Handler
