// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// LDAP login path. Reached from authHandler.login for accounts whose
// store.User.AuthSource is store.AuthSourceLDAP, and for unknown usernames
// when a verifier is configured (just-in-time provisioning).
//
// Every failure here returns the same 401 body the local path returns.
// Distinguishing "no such directory entry" from "wrong password" — or from
// "this username is taken by a local account" — would turn login into a
// user-enumeration oracle.
//
// The 401 bodies are equalized across every failure path; the latencies are
// not. An observer who times /auth/login can tell an immediate rejection
// (disabled account, unrecognized auth_source) from a local argon2id check
// from a directory round trip, and so can learn which backend owns a
// username. An argon2id derivation and a WAN LDAP bind cannot be made to cost
// the same, so the mitigation is rate limiting rather than a timing fix in
// this file. sqi ships only a generic per-IP limiter over all of /api/v1
// (20 req/s, burst 40), which is not a brute-force control; see "Brute force"
// in docs/auth.md for what that does and does not cover, including AD
// badPwdCount lockout.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/uberware/sqi/internal/auth/ldap"
	"github.com/uberware/sqi/internal/store"
)

// loginLDAP authenticates username/password against the directory. existing
// is the row the login lookup found under the typed username, or nil when
// there is none. It is consulted only to refuse a locally disabled account
// before the bind: which account the identity belongs to is decided
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
		h.logLDAPAccountWithoutExternalID(ctx, existing, id.Username, err)
		writeProblem(w, r, http.StatusUnauthorized, invalidLoginDetail)
		return
	}

	if err := h.issueSession(w, r, u); err != nil {
		h.logger.ErrorContext(ctx, "auth: create session failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to create session")
		return
	}
	writeJSON(w, http.StatusOK, toUserResponse(u, h.ldapCfg.RoleSource, h.oidcCfg.RoleSource))
}

// logLDAPAccountWithoutExternalID explains the one login failure whose cause
// is invisible from every other vantage point.
//
// A legacy LDAP row carries an empty users.external_id. Accounts are matched
// on the directory's stable identifier, so such a user's login goes: the
// username lookup finds the row, the directory bind succeeds, the identity
// lookup misses because the stored identifier is empty, provisioning collides
// on the username, and the login is refused, on every attempt.
//
// The refusal is intended. Adopting the row when its stored identifier is
// empty would be username matching under another name, and would keep the
// recycled-identity hazard for the accounts most likely to be long-lived and
// privileged. An operator recreates legacy rows instead.
//
// Because the symptom is indistinguishable from a wrong password, this error
// log names the account and the remedy. It is the only thing that differs:
// the response is the same equalized 401 as every other failure, because a
// distinguishable body would turn the legacy state into a user-enumeration
// oracle.
//
// Gated on ErrConflict. A verified identity that carried no identifier at all
// is a different fault (auth.ldap.unique_id_attr is wrong for this server)
// with its own log in resolveExternalUser, and blaming a legacy row for it
// would send the operator to the wrong fix.
//
// existing is nil when the caller was reached under an alias: the login
// lookup runs on the typed username (say the UPN "alice@example.com"), which
// misses a row stored under the directory's own spelling ("alice"), the
// spelling the collision happened on. Re-reading by directoryUsername
// (id.Username from the verified identity, the name provisioning collided on)
// recovers the row so the diagnostic also fires for the alias case
// docs/auth.md covers. A failed re-read logs nothing.
func (h *authHandler) logLDAPAccountWithoutExternalID(ctx context.Context, existing *store.User, directoryUsername string, err error) {
	if !errors.Is(err, store.ErrConflict) {
		return
	}
	if existing == nil {
		row, lookupErr := h.store.GetUserByUsername(ctx, directoryUsername)
		if lookupErr != nil {
			return
		}
		existing = &row
	}
	if existing.AuthSource != store.AuthSourceLDAP || existing.ExternalID != "" {
		return
	}
	h.logger.ErrorContext(ctx,
		"auth: ldap account has no external_id and cannot log in: "+
			"the directory identity cannot be matched to it and "+
			"provisioning collides on the username. Delete and recreate the account (the next "+
			"login will provision it with the directory's identifier). It is not adopted "+
			"automatically: that would be username matching, which lets a recycled directory "+
			"identity inherit this account.",
		slog.String("username", existing.Username),
		slog.String("user_id", existing.ID))
}
