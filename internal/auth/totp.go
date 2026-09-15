package auth

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"image/png"
	"net/http"
	"strings"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

const totpPeriod = 30

func normalizeCode(c string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' {
			return -1
		}
		return r
	}, strings.TrimSpace(c))
}

// verifyTOTP checks a 6-digit code (±1 step) and rejects reuse of a step
// that was already accepted for the user.
func (s *Service) verifyTOTP(userID, secret, code string) bool {
	code = normalizeCode(code)
	if secret == "" || len(code) != 6 {
		return false
	}
	for _, ch := range code {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	step := s.now().Unix() / totpPeriod
	s.totpMu.Lock()
	defer s.totpMu.Unlock()
	for _, d := range []int64{0, -1, 1} {
		st := step + d
		want, err := totp.GenerateCodeCustom(secret, time.Unix(st*totpPeriod, 0), totp.ValidateOpts{
			Period: totpPeriod, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
		})
		if err != nil {
			return false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			if last, ok := s.totpSteps[userID]; ok && st <= last {
				return false
			}
			s.totpSteps[userID] = st
			return true
		}
	}
	return false
}

type totpEnrollResponse struct {
	Secret     string `json:"secret"`
	OtpauthURL string `json:"otpauthUrl"`
	QRPng      string `json:"qrPng"`
}

func (s *Service) handleTOTPEnroll(w http.ResponseWriter, r *http.Request, u *store.User) {
	if u.TOTPEnabled {
		httpx.WriteError(w, http.StatusConflict, "totp_enabled", "Authenticator app 2FA is already on — turn it off first to enrol a new device")
		return
	}
	ctx := r.Context()
	issuer := "Relay"
	if name := strings.TrimSpace(s.general(ctx).InstanceName); name != "" && !strings.EqualFold(name, "Relay") {
		issuer = "Relay (" + name + ")"
	}
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer: issuer, AccountName: u.Username,
		Period: totpPeriod, SecretSize: 20, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	img, err := key.Image(240, 240)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	u.TOTPSecret = key.Secret()
	u.TOTPEnabled = false
	if err := s.app.Store.UpdateUser(ctx, u); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, totpEnrollResponse{
		Secret: key.Secret(), OtpauthURL: key.URL(),
		QRPng: "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()),
	})
}

type codeRequest struct {
	Code string `json:"code"`
}

func (s *Service) handleTOTPVerify(w http.ResponseWriter, r *http.Request, u *store.User) {
	var req codeRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	switch {
	case u.TOTPEnabled:
		httpx.WriteError(w, http.StatusConflict, "totp_enabled", "Authenticator app 2FA is already on")
		return
	case u.TOTPSecret == "":
		httpx.WriteError(w, http.StatusConflict, "totp_not_started", "Start enrolment first")
		return
	}
	if !s.verifyTOTP(u.ID, u.TOTPSecret, req.Code) {
		httpx.Fail(w, r, &model.ValidationError{Fields: map[string]string{"code": "That code didn't match — check the time on your device and try the next code"}})
		return
	}
	ctx := r.Context()
	u.TOTPEnabled = true
	if err := s.app.Store.UpdateUser(ctx, u); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	_ = s.app.Store.SetSessionMFAPending(ctx, u.ID, false)
	s.app.Audit(ctx, core.AuditEntry{Action: "auth.2fa_enabled", Target: u.Username, Detail: "authenticator app (TOTP)"})
	s.writeSession(w, r, http.StatusOK)
}

type passwordRequest struct {
	Password string `json:"password"`
}

func (s *Service) handleTOTPDisable(w http.ResponseWriter, r *http.Request, u *store.User) {
	var req passwordRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	ctx := r.Context()
	ip := core.ClientIP(r)
	uname := strings.ToLower(u.Username)
	if s.throttled(ctx, ip, uname) {
		s.writeThrottled(ctx, w)
		return
	}
	if !checkPassword(u.PasswordHash, req.Password) {
		s.loginFailed(ctx, ip, uname, u, "wrong password (turning off 2FA)")
		httpx.Fail(w, r, &model.ValidationError{Fields: map[string]string{"password": "Password is wrong"}})
		return
	}
	if !u.TOTPEnabled && u.TOTPSecret == "" {
		httpx.WriteError(w, http.StatusConflict, "totp_disabled", "Authenticator app 2FA is not on")
		return
	}
	if u.Role == core.RoleAdmin && u.TOTPEnabled && s.security(ctx).Require2FAForAdmins {
		if pk, err := s.app.Store.CountWebAuthnCredentials(ctx, u.ID); err != nil {
			httpx.Fail(w, r, err)
			return
		} else if pk == 0 {
			httpx.WriteError(w, http.StatusConflict, "2fa_required", "Admins must keep 2FA on — add a passkey before turning the authenticator app off")
			return
		}
	}
	wasEnabled := u.TOTPEnabled
	u.TOTPSecret = ""
	u.TOTPEnabled = false
	if err := s.app.Store.UpdateUser(ctx, u); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	s.totpMu.Lock()
	delete(s.totpSteps, u.ID)
	s.totpMu.Unlock()
	if wasEnabled {
		s.app.Audit(ctx, core.AuditEntry{Action: "auth.2fa_disabled", Target: u.Username, Detail: "authenticator app (TOTP)"})
	}
	s.writeSession(w, r, http.StatusOK)
}
