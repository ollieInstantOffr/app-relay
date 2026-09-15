package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/store"
)

// ResetPasswordCLI implements `relay users reset-password <username>`: it sets
// a random temporary password that must be changed at the next sign-in, signs
// the user out everywhere, clears failed sign-in throttling and prints the
// password. Two-factor settings are left unchanged.
func ResetPasswordCLI(ctx context.Context, st *store.Store, username string, out io.Writer) error {
	username = strings.TrimSpace(username)
	u, err := st.GetUserByUsername(ctx, username)
	if errors.Is(err, store.ErrNotFound) {
		users, lerr := st.ListUsers(ctx)
		if lerr != nil {
			return fmt.Errorf("no user named %q", username)
		}
		if len(users) == 0 {
			return fmt.Errorf("no user named %q — Relay has no accounts yet; open the web UI to run first-time setup", username)
		}
		names := make([]string, 0, len(users))
		for _, x := range users {
			names = append(names, x.Username)
		}
		return fmt.Errorf("no user named %q (existing users: %s)", username, strings.Join(names, ", "))
	}
	if err != nil {
		return err
	}
	temp := GeneratePassword(3)
	hash, err := HashPassword(temp)
	if err != nil {
		return err
	}
	u.PasswordHash = hash
	u.MustChangePassword = true
	if err := st.UpdateUser(ctx, u); err != nil {
		return err
	}
	n, err := st.DeleteUserSessions(ctx, u.ID, "")
	if err != nil {
		return err
	}
	_ = st.ClearLoginFailures(ctx, strings.ToLower(u.Username))
	_, _ = st.InsertAudit(ctx, store.AuditRow{
		At: time.Now(), ActorType: core.ActorSystem, ActorName: "cli", Action: "user.reset_password", Target: u.Username,
		Detail: fmt.Sprintf("relay users reset-password · %d session(s) signed out", n), Result: "ok",
	})
	fmt.Fprintf(out, "Password for %s has been reset.\n\n    Temporary password: %s\n\n", u.Username, temp)
	fmt.Fprintf(out, "Sign in with it; Relay will ask for a new password. %d session(s) were signed out.\n", n)
	if u.TOTPEnabled {
		fmt.Fprintln(out, "Two-factor authentication is still required at sign-in.")
	}
	if u.Disabled {
		fmt.Fprintln(out, "Note: this account is disabled — another admin must re-enable it.")
	}
	return nil
}
