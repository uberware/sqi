// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Shared account resolution for externally-authenticated logins (Phase 3,
// components C1 and C2). LDAP and OIDC differ in how they verify a credential;
// they do not differ in what happens afterwards, so provisioning, role re-sync
// and collision refusal live here once.
//
// Accounts are matched on (auth_source, external_id), never on username. A
// name is not a stable identity: providers recycle email addresses, so a new
// hire receiving a departed admin's address would otherwise log straight into
// that admin's account — same role, same owned jobs, no error anywhere. A
// rename at the provider has the mirror failure, orphaning the account and
// provisioning a duplicate.

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
)

// externalIdentity is what a verified external login reports about the
// account, reduced to the fields provisioning needs. Both ldap.Identity and
// oidc.Identity map onto it.
type externalIdentity struct {
	// ExternalID is the provider's stable, non-reusable identifier: the OIDC
	// "sub" claim, or the configured LDAP unique-id attribute.
	ExternalID string
	// Username is the provider's spelling of the login name. Used for display
	// and uniqueness only — never for matching.
	Username string
	// DisplayName is the human-facing label. Consumed only at provisioning.
	DisplayName string
}

// externalPlaceholderHash occupies users.password_hash for externally-verified
// accounts. It is not a valid argon2id encoding, so password.Verify against it
// always fails — meaning even a future code path that mistakenly ran a local
// password check on such an account could not be satisfied by any password.
//
// Rows provisioned before C2 carry "!ldap" instead. No migration rewrites
// them: the property that matters is "not a valid argon2id encoding", which
// both satisfy.
const externalPlaceholderHash = "!external"

// Refusal reasons from resolveExternalUser. They are compared with errors.Is,
// never by string: the caller has to tell an operator misconfiguration (the
// provider returned no stable identifier — worth surfacing loudly) from a
// deliberate local override (the account is disabled — routine) in order to
// log them differently, while returning the identical 401 for both.
var (
	// errNoStableIdentifier means the verified identity carried no external
	// id, so there is nothing safe to match the account on.
	errNoStableIdentifier = errors.New("external login: no stable identifier")
	// errExternalAccountDisabled means the matched local row is disabled.
	errExternalAccountDisabled = errors.New("external login: account disabled")
)

// resolveExternalUser returns the local record for a verified external
// identity, provisioning it on first login and re-syncing the role when the
// provider owns it.
//
// providerOwnsRole is the caller's role_source decision, passed in rather than
// read here so LDAP and OIDC can be configured independently.
func (h *authHandler) resolveExternalUser(
	r *http.Request, authSource string, id externalIdentity, role string, providerOwnsRole bool,
) (store.User, error) {
	ctx := r.Context()
	if id.ExternalID == "" {
		// Refusing is the whole point of the identifier: a provider that
		// returns no stable id must not silently fall back to name matching,
		// which is the hazard this design removes.
		h.logger.ErrorContext(ctx, "auth: external login carried no stable identifier",
			slog.String("auth_source", authSource), slog.String("username", id.Username))
		return store.User{}, errNoStableIdentifier
	}

	existing, err := h.store.GetUserByExternalID(ctx, authSource, id.ExternalID)
	switch {
	case err == nil:
		// Known identity. The disabled flag is the operator's override and
		// holds regardless of what the provider says.
		if existing.Disabled {
			return store.User{}, errExternalAccountDisabled
		}
		return h.syncExternalRole(r, existing, role, providerOwnsRole)
	case errors.Is(err, store.ErrNotFound):
		return h.provisionExternalUser(r, authSource, id, role)
	default:
		h.logger.ErrorContext(ctx, "auth: external identity lookup failed", slog.Any("error", err))
		return store.User{}, err
	}
}

// provisionExternalUser creates the local record backing a new external
// identity.
func (h *authHandler) provisionExternalUser(
	r *http.Request, authSource string, id externalIdentity, role string,
) (store.User, error) {
	ctx := r.Context()
	created, err := h.store.CreateUser(ctx, store.User{
		ID:           uuid.NewString(),
		Username:     id.Username,
		DisplayName:  id.DisplayName,
		PasswordHash: externalPlaceholderHash,
		Role:         role,
		AuthSource:   authSource,
		ExternalID:   id.ExternalID,
	})
	if err == nil {
		return created, nil
	}
	if errors.Is(err, store.ErrConflict) {
		// The username is taken by a different account — either a local one or
		// another external identity that happens to share the name. Both are
		// refused, never adopted: adopting a local row would let anyone able to
		// provision an "admin" at the provider inherit the local admin, and
		// adopting another external row would hand one person another's
		// account. The operator's fix is to rename one of them.
		h.logger.WarnContext(ctx, "auth: external username collides with an existing account",
			slog.String("auth_source", authSource), slog.String("username", id.Username))
		return store.User{}, err
	}
	h.logger.ErrorContext(ctx, "auth: external user provisioning failed",
		slog.String("auth_source", authSource), slog.String("username", id.Username), slog.Any("error", err))
	return store.User{}, err
}

// syncExternalRole re-applies the mapped role when the provider owns it.
func (h *authHandler) syncExternalRole(
	r *http.Request, existing store.User, role string, providerOwnsRole bool,
) (store.User, error) {
	// role_source=local: the mapping seeded the role at creation and the local
	// column owns it from then on. Touching it here would silently revert an
	// admin's edit at the user's next login — the exact contradiction
	// role_source exists to prevent.
	if !providerOwnsRole || existing.Role == role {
		return existing, nil
	}
	updated := existing
	updated.Role = role
	// DisplayName and Username are passed through untouched: both are seeded
	// from the provider once at provisioning and owned locally afterwards, so a
	// self-service PATCH /auth/me edit survives the next login. AuthSource and
	// ExternalID are immutable through UpdateUser.
	out, err := h.store.UpdateUser(r.Context(), updated)
	if err != nil {
		// The credentials are valid and the only failure is the role refresh.
		// Log it and let them in with the stored role rather than locking out a
		// legitimate user over a transient store error.
		h.logger.WarnContext(r.Context(), "auth: external role sync failed",
			slog.String("username", existing.Username), slog.Any("error", err))
		return existing, nil
	}
	return out, nil
}
