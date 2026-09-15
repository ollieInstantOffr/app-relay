package auth

import (
	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
)

// PublicRoutes registers routes outside the protected group (handlers check
// authentication themselves):
//
//	GET    /auth/session                     Session (+ setupDone, sessionTtlHours, user.mustEnroll2fa …)
//	POST   /auth/login                       {username, password, totp?, remember}
//	POST   /auth/logout
//	POST   /auth/passkeys/login/begin|finish discoverable passkey sign-in (finish?remember=1)
//	POST   /auth/password                    {current, new}                    (signed-in user)
//	POST   /auth/totp/enroll|verify|disable  TOTP enrolment                    (signed-in user)
//	GET    /auth/passkeys, DELETE /auth/passkeys/{id}
//	POST   /auth/passkeys/register/begin|finish (finish?name=)               (signed-in user)
//	POST   /auth/request-access              {what, path}                      (signed-in user)
//	GET    /sessions[?scope=all], DELETE /sessions/{id}                        (signed-in user)
//	POST   /setup/admin                      {username, password, enroll2fa}   (only while no users exist)
//	GET    /setup/network, POST /setup/network {lanCidr, adminDomain}, POST /setup/finish (admin, until setup is done)
//
// Account routes live here rather than in Routes so viewers — whose writes the
// protected group rejects — can still change their password, 2FA and sessions.
func PublicRoutes(app *core.App, r chi.Router) {
	s := serviceOf(app)
	if s == nil {
		app.Log.Error("auth: app.Auth is not *auth.Service; auth routes disabled")
		return
	}
	s.registerSettingsHooks()

	r.Get("/auth/session", s.handleSession)
	r.Post("/auth/login", s.handleLogin)
	r.Post("/auth/logout", s.handleLogout)
	r.Post("/auth/passkeys/login/begin", s.handlePasskeyLoginBegin)
	r.Post("/auth/passkeys/login/finish", s.handlePasskeyLoginFinish)

	r.Post("/auth/password", s.requireUser(s.handleChangePassword))
	r.Post("/auth/totp/enroll", s.requireUser(s.handleTOTPEnroll))
	r.Post("/auth/totp/verify", s.requireUser(s.handleTOTPVerify))
	r.Post("/auth/totp/disable", s.requireUser(s.handleTOTPDisable))
	r.Get("/auth/passkeys", s.requireUser(s.handlePasskeysList))
	r.Post("/auth/passkeys/register/begin", s.requireUser(s.handlePasskeyRegisterBegin))
	r.Post("/auth/passkeys/register/finish", s.requireUser(s.handlePasskeyRegisterFinish))
	r.Delete("/auth/passkeys/{id}", s.requireUser(s.handlePasskeyDelete))
	r.Post("/auth/request-access", s.requireUser(s.handleRequestAccess))
	r.Get("/sessions", s.requireUser(s.handleSessionsList))
	r.Delete("/sessions/{id}", s.requireUser(s.handleSessionDelete))

	r.Post("/setup/admin", s.handleSetupAdmin)
	r.Get("/setup/network", s.requireSetupAdmin(s.handleSetupNetworkGet))
	r.Post("/setup/network", s.requireSetupAdmin(s.handleSetupNetworkPost))
	r.Post("/setup/finish", s.requireSetupAdmin(s.handleSetupFinish))
}

// Routes registers authenticated routes:
//
//	GET  /about                                          {version, installedAt, databaseBytes, goVersion}
//	GET  /users, POST /users, PUT|DELETE /users/{id}     (admin)
//	POST /users/{id}/reset-password → {password, signedOut, self}   (admin)
//	POST /users/{id}/reset-2fa                           (admin)
//	GET  /tokens?surface=mcp|rest, POST /tokens, DELETE /tokens/{id}
func Routes(app *core.App, r chi.Router) {
	s := serviceOf(app)
	if s == nil {
		return
	}
	r.Get("/about", s.handleAbout)

	r.Get("/users", httpx.RequireAdmin(s.handleUsersList))
	r.Post("/users", httpx.RequireAdmin(s.handleUserCreate))
	r.Put("/users/{id}", httpx.RequireAdmin(s.handleUserUpdate))
	r.Delete("/users/{id}", httpx.RequireAdmin(s.handleUserDelete))
	r.Post("/users/{id}/reset-password", httpx.RequireAdmin(s.handleUserResetPassword))
	r.Post("/users/{id}/reset-2fa", httpx.RequireAdmin(s.handleUserReset2FA))

	r.Get("/tokens", s.handleTokensList)
	r.Post("/tokens", s.handleTokenCreate)
	r.Delete("/tokens/{id}", s.handleTokenDelete)
}
