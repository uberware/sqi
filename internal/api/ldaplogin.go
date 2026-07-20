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
//
// Security posture, stated plainly: the 401 *bodies* are equalized across
// every failure path, but the *latencies* are not. An observer who times
// /auth/login can still tell an immediate rejection (disabled account,
// unrecognized auth_source) from a local argon2id check from a directory
// round trip, and so can learn something about which backend owns a
// username. Equalizing that is not achievable — an argon2id derivation and a
// WAN LDAP bind cannot be made to cost the same — so the mitigation is rate
// limiting, not a timing fix in this file. Note that sqi ships only a generic
// per-IP limiter over all of /api/v1 (20 req/s, burst 40), which is not a
// brute-force control; see "Brute force" in docs/auth.md for what that does
// and does not cover, including AD badPwdCount lockout.

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/uberware/sqi/internal/auth/ldap"
	"github.com/uberware/sqi/internal/store"
)

// loginLDAP authenticates username/password against the directory. existing
// is the row the login lookup found under the typed username, or nil when
// there is none. It is consulted only to refuse a locally disabled account
// before the bind: which account the identity actually belongs to is decided
// afterwards by resolveExternalUser, from the directory's stable identifier
// rather than the name the caller typed.
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

	u, err := h.resolveExternalUser(r, store.AuthSourceLDAP, externalIdentity{
		ExternalID:  id.ExternalID,
		Username:    id.Username,
		DisplayName: id.DisplayName,
	}, role, h.ldapCfg.RoleSource == ldap.RoleSourceDirectory)
	if err != nil {
		writeProblem(w, r, http.StatusUnauthorized, invalidLoginDetail)
		return
	}

	if err := h.issueSession(w, r, u); err != nil {
		h.logger.ErrorContext(ctx, "auth: create session failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to create session")
		return
	}
	writeJSON(w, http.StatusOK, toUserResponse(u, h.ldapCfg.RoleSource))
}
