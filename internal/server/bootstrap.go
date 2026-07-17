// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/auth/password"
	"github.com/uberware/sqi/internal/store"
)

// BootstrapParams are the seed credentials for the first admin, sourced from
// config.AuthConfig.Bootstrap (SQI_AUTH_BOOTSTRAP_USERNAME /
// SQI_AUTH_BOOTSTRAP_PASSWORD).
type BootstrapParams struct {
	Username string
	Password string
}

// bootstrapAdmin seeds a single admin user when auth is enabled and no users
// exist. It is idempotent and never overwrites an existing account: once the
// users table is non-empty it is a no-op, even if p carries credentials. With
// no credentials configured on an empty table it logs a warning and leaves
// the server usable but unable to authenticate as anyone yet — matching A0's
// no-fail-closed philosophy — until an operator sets the bootstrap env vars
// and restarts.
func bootstrapAdmin(ctx context.Context, st store.Store, p BootstrapParams, logger *slog.Logger) error {
	n, err := st.CountUsers(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap: count users: %w", err)
	}
	if n > 0 {
		if p.Username != "" && p.Password != "" {
			// Bootstrap credentials were configured but there is already at
			// least one user (e.g. one created while auth was off, when
			// /users was unauthenticated) — bootstrap is a no-op and the
			// configured credentials are silently ignored. Without this log
			// an operator who restarts expecting a new admin gets nothing
			// and no explanation. Never log the credentials themselves.
			logger.InfoContext(ctx, "auth bootstrap credentials are configured but users already "+
				"exist — skipping bootstrap; use the /users API or an existing account to log in",
				slog.String("username", p.Username))
		}
		return nil
	}
	if p.Username == "" || p.Password == "" {
		logger.WarnContext(ctx, "auth is enabled but no users exist and no bootstrap "+
			"credentials are configured — set SQI_AUTH_BOOTSTRAP_USERNAME and "+
			"SQI_AUTH_BOOTSTRAP_PASSWORD and restart to create the first admin")
		return nil
	}
	hash, err := password.Hash(p.Password)
	if err != nil {
		return fmt.Errorf("bootstrap: hash password: %w", err)
	}
	if _, err := st.CreateUser(ctx, store.User{
		ID:           uuid.NewString(),
		Username:     p.Username,
		DisplayName:  p.Username,
		PasswordHash: hash,
		Role:         "admin",
	}); err != nil {
		return fmt.Errorf("bootstrap: create admin: %w", err)
	}
	logger.InfoContext(ctx, "seeded initial admin user — change its password after first login",
		slog.String("username", p.Username))
	return nil
}
