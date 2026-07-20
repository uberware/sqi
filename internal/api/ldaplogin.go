// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// LDAP login path (Phase 3, component C1). Reached from authHandler.login for
// accounts whose store.User.AuthSource is store.AuthSourceLDAP, and for
// unknown usernames when a verifier is configured (just-in-time
// provisioning).
//
// Every failure here returns the same 401 body the local path returns.
// Distinguishing "no such directory entry" from "wrong password" — or from
// "this username is taken by a local account" — would turn login into a
// user-enumeration oracle.

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/auth/ldap"
	"github.com/uberware/sqi/internal/store"
)

// ldapPlaceholderHash occupies users.password_hash for directory accounts.
// It is not a valid argon2id encoding, so password.Verify against it always
// fails — meaning that even if a future code path mistakenly ran a local
// password check on a directory account, no password could satisfy it.
const ldapPlaceholderHash = "!ldap"

// loginLDAP authenticates username/password against the directory. existing
// is the already-loaded user record, or nil when this is a first login that
// may provision one.
func (h *authHandler) loginLDAP(w http.ResponseWriter, r *http.Request, username, pw string, existing *store.User) {
	ctx := r.Context()

	if h.ldapVerifier == nil {
		// A directory account exists but LDAP has been switched off. There is
		// no credential that can satisfy it; say so in the log, not to the
		// client.
		h.logger.WarnContext(ctx, "auth: ldap account but no verifier configured",
			slog.String("username", username))
		writeProblem(w, r, http.StatusUnauthorized, invalidLoginDetail)
		return
	}

	// Refuse a locally disabled account before touching the directory: the
	// disabled flag is the operator's override and must hold regardless of
	// what the directory thinks.
	if existing != nil && existing.Disabled {
		writeProblem(w, r, http.StatusUnauthorized, invalidLoginDetail)
		return
	}

	id, err := h.ldapVerifier.Verify(ctx, username, pw)
	if err != nil {
		// An unreachable directory is an operations event and is logged as
		// one; a rejected password is routine and is not. Neither changes
		// what the client sees.
		if errors.Is(err, ldap.ErrUnavailable) {
			h.logger.ErrorContext(ctx, "auth: ldap directory unavailable", slog.Any("error", err))
		}
		writeProblem(w, r, http.StatusUnauthorized, invalidLoginDetail)
		return
	}

	role, ok := h.ldapCfg.MapRole(id.Groups)
	if !ok {
		// default_role is empty and nothing matched: this deployment requires
		// group membership to sign in at all.
		h.logger.InfoContext(ctx, "auth: ldap login rejected, no role mapping matched",
			slog.String("username", username), slog.Any("groups", id.Groups))
		writeProblem(w, r, http.StatusUnauthorized, invalidLoginDetail)
		return
	}

	u, err := h.resolveLDAPUser(r, existing, id, role)
	if err != nil {
		writeProblem(w, r, http.StatusUnauthorized, invalidLoginDetail)
		return
	}

	if err := h.issueSession(w, r, u); err != nil {
		h.logger.ErrorContext(ctx, "auth: create session failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to create session")
		return
	}
	writeJSON(w, http.StatusOK, toUserResponse(u))
}

// resolveLDAPUser returns the local record for a verified directory identity,
// provisioning it on first login and re-syncing the role when the directory
// owns it.
func (h *authHandler) resolveLDAPUser(
	r *http.Request, existing *store.User, id ldap.Identity, role string,
) (store.User, error) {
	if existing == nil {
		return h.provisionLDAPUser(r, id, role)
	}
	// role_source=local: the mapping seeded the role at creation and the
	// local column owns it from then on. Touching it here would silently
	// revert an admin's edit at the user's next login — the exact
	// contradiction role_source exists to prevent.
	if h.ldapCfg.RoleSource != ldap.RoleSourceDirectory || existing.Role == role {
		return *existing, nil
	}
	updated := *existing
	updated.Role = role
	// DisplayName is passed through untouched: it is seeded from the
	// directory once at provisioning and owned locally afterwards, so a
	// self-service PATCH /auth/me edit survives the next login. AuthSource is
	// immutable through UpdateUser, so this cannot change the backend either.
	out, err := h.store.UpdateUser(r.Context(), updated)
	if err != nil {
		// The credentials are valid and the only failure is the role
		// refresh. Log it and let them in with the stored role rather than
		// locking out a legitimate user over a transient store error.
		h.logger.WarnContext(r.Context(), "auth: ldap role sync failed",
			slog.String("username", existing.Username), slog.Any("error", err))
		return *existing, nil
	}
	return out, nil
}

// provisionLDAPUser creates the local record backing a directory identity.
func (h *authHandler) provisionLDAPUser(r *http.Request, id ldap.Identity, role string) (store.User, error) {
	ctx := r.Context()
	// Prefer the directory's spelling of the name so casing is stable across
	// logins regardless of what the user typed.
	username := id.Username
	created, err := h.store.CreateUser(ctx, store.User{
		ID:           uuid.NewString(),
		Username:     username,
		DisplayName:  id.DisplayName,
		PasswordHash: ldapPlaceholderHash,
		Role:         role,
		AuthSource:   store.AuthSourceLDAP,
	})
	if err == nil {
		return created, nil
	}
	if errors.Is(err, store.ErrConflict) {
		// A local account already owns this username. Adopting it — flipping
		// auth_source to "ldap" — would mean anyone able to create a
		// directory account named "admin" inherits the local admin's
		// privileges. Refuse, and give the operator a signal they can act on;
		// the client still sees only the generic 401.
		h.logger.WarnContext(ctx, "auth: ldap username collides with an existing local account",
			slog.String("username", username))
		return store.User{}, err
	}
	h.logger.ErrorContext(ctx, "auth: ldap user provisioning failed",
		slog.String("username", username), slog.Any("error", err))
	return store.User{}, err
}
