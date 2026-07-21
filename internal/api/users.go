// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// User-admin REST handlers (Phase 3, component A1). Routes require
// users.read (GET) or users.manage (create/update/set-password/delete), both
// admin-only in the built-in role matrix (internal/auth/policy). Mutations
// that would disable, demote, or delete the last enabled admin are rejected
// (409) so the user store can never lock itself out of admin access.
//
//	POST   /api/v1/users              — create
//	GET    /api/v1/users              — list
//	GET    /api/v1/users/{id}         — get
//	PATCH  /api/v1/users/{id}         — update display_name/role/disabled
//	PUT    /api/v1/users/{id}/password — set password
//	DELETE /api/v1/users/{id}         — delete

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/auth/password"
	"github.com/uberware/sqi/internal/auth/policy"
	"github.com/uberware/sqi/internal/auth/rolemap"
	"github.com/uberware/sqi/internal/store"
)

type usersHandler struct {
	store  store.Store
	logger *slog.Logger
	// ldapRoleSource mirrors auth.ldap.role_source and oidcRoleSource mirrors
	// auth.oidc.role_source. In "directory" mode the group mapping owns that
	// source's accounts' roles, so a role edit here must be refused rather than
	// accepted and silently reverted at next login. They are held separately
	// because the two providers are configured independently.
	ldapRoleSource string
	oidcRoleSource string
}

func newUsersHandler(st store.Store, logger *slog.Logger, ldapRoleSource, oidcRoleSource string) *usersHandler {
	return &usersHandler{
		store:          st,
		logger:         logger,
		ldapRoleSource: ldapRoleSource,
		oidcRoleSource: oidcRoleSource,
	}
}

type userCreateRequest struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

type userUpdateRequest struct {
	DisplayName *string `json:"display_name"`
	Role        *string `json:"role"`
	Disabled    *bool   `json:"disabled"`
}

type passwordRequest struct {
	Password string `json:"password"`
}

// validRole delegates to the policy matrix rather than restating the role
// list: a role accepted here but absent from the matrix would create a user
// with no permissions and no error to explain why.
func validRole(role string) bool {
	return policy.IsRole(role)
}

func (h *usersHandler) create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req userCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || req.Password == "" {
		writeProblem(w, r, http.StatusBadRequest, "username and password are required")
		return
	}
	if req.Role == "" {
		req.Role = "user"
	}
	if !validRole(req.Role) {
		writeProblem(w, r, http.StatusBadRequest, "invalid role")
		return
	}
	hash, err := password.Hash(req.Password)
	if err != nil {
		h.logger.ErrorContext(ctx, "users: hash failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to create user")
		return
	}
	created, err := h.store.CreateUser(ctx, store.User{
		ID: uuid.NewString(), Username: req.Username, DisplayName: req.DisplayName,
		PasswordHash: hash, Role: req.Role,
		// Directory accounts are provisioned by the login path on first
		// successful bind, never through this form.
		AuthSource: store.AuthSourceLocal,
	})
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeProblem(w, r, http.StatusConflict, "a user with that username already exists")
			return
		}
		h.logger.ErrorContext(ctx, "users: create failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to create user")
		return
	}
	writeJSON(w, http.StatusCreated, toUserResponse(created, h.ldapRoleSource, h.oidcRoleSource))
}

func (h *usersHandler) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	users, err := h.store.ListUsers(ctx)
	if err != nil {
		h.logger.ErrorContext(ctx, "users: list failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to list users")
		return
	}
	resp := make([]userResponse, len(users))
	for i, u := range users {
		resp[i] = toUserResponse(u, h.ldapRoleSource, h.oidcRoleSource)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *usersHandler) get(w http.ResponseWriter, r *http.Request) {
	u, err := h.store.GetUser(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.notFoundOr500(w, r, err, "retrieve")
		return
	}
	writeJSON(w, http.StatusOK, toUserResponse(u, h.ldapRoleSource, h.oidcRoleSource))
}

func (h *usersHandler) update(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	var req userUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}
	orig, err := h.store.GetUser(ctx, id)
	if err != nil {
		h.notFoundOr500(w, r, err, "retrieve")
		return
	}
	if req.Role != nil && *req.Role != orig.Role && directoryOwnsRole(orig, h.ldapRoleSource, h.oidcRoleSource) {
		writeProblem(w, r, http.StatusConflict, directoryRoleConflictDetail(orig))
		return
	}
	u := orig
	if req.DisplayName != nil {
		u.DisplayName = *req.DisplayName
	}
	if req.Role != nil {
		if !validRole(*req.Role) {
			writeProblem(w, r, http.StatusBadRequest, "invalid role")
			return
		}
		u.Role = *req.Role
	}
	if req.Disabled != nil {
		u.Disabled = *req.Disabled
	}
	if last, err := h.wouldRemoveLastAdmin(ctx, orig, u.Role == "admin" && !u.Disabled); err != nil {
		h.notFoundOr500(w, r, err, "update")
		return
	} else if last {
		writeProblem(w, r, http.StatusConflict, "cannot remove the last admin")
		return
	}
	updated, err := h.store.UpdateUser(ctx, u)
	if err != nil {
		h.notFoundOr500(w, r, err, "update")
		return
	}
	writeJSON(w, http.StatusOK, toUserResponse(updated, h.ldapRoleSource, h.oidcRoleSource))
}

func (h *usersHandler) setPassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	var req passwordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Password == "" {
		writeProblem(w, r, http.StatusBadRequest, "password is required")
		return
	}
	target, err := h.store.GetUser(ctx, id)
	if err != nil {
		h.notFoundOr500(w, r, err, "retrieve")
		return
	}
	if !passwordEditable(target) {
		writeProblem(w, r, http.StatusConflict, externalPasswordSetConflictDetail(target))
		return
	}
	hash, err := password.Hash(req.Password)
	if err != nil {
		h.logger.ErrorContext(ctx, "users: hash failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to set password")
		return
	}
	if err := h.store.SetUserPassword(ctx, id, hash); err != nil {
		h.notFoundOr500(w, r, err, "update")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *usersHandler) delete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	target, err := h.store.GetUser(ctx, id)
	if err != nil {
		h.notFoundOr500(w, r, err, "delete")
		return
	}
	if last, err := h.wouldRemoveLastAdmin(ctx, target, false); err != nil {
		h.notFoundOr500(w, r, err, "delete")
		return
	} else if last {
		writeProblem(w, r, http.StatusConflict, "cannot remove the last admin")
		return
	}
	if err := h.store.DeleteUser(ctx, id); err != nil {
		h.notFoundOr500(w, r, err, "delete")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// wouldRemoveLastAdmin reports whether changing/removing target would drop the
// enabled-admin count to zero. isStillAdmin is the target's admin status AFTER
// the pending change (false for delete, disable, or demotion away from admin).
func (h *usersHandler) wouldRemoveLastAdmin(ctx context.Context, target store.User, isStillAdmin bool) (bool, error) {
	if target.Role != "admin" || target.Disabled {
		return false, nil // target isn't a live admin; removing it changes nothing
	}
	if isStillAdmin {
		return false, nil // still a live admin after the change
	}
	n, err := h.store.CountAdmins(ctx)
	if err != nil {
		return false, err
	}
	return n <= 1, nil
}

// directoryOwnsRole reports whether u's role is recomputed from an external
// provider's groups at every login, making a local edit a no-op that reverts
// silently.
//
// This is the single source of truth for the rule, and it is deliberately a
// package-level function rather than a method: the PATCH guard below and the
// role_editable field on the wire format (see toUserResponse) must never be
// able to disagree. Both conditions matter per source — an LDAP account under
// role_source=local IS editable, and so is an OIDC one.
//
// The two role sources are separate parameters, not one value, because LDAP
// and OIDC are configured independently: an operator may trust one provider's
// group memberships and not the other's.
//
// A no-op "change" (same role in, same role out) is deliberately allowed by
// the caller's *req.Role != orig.Role check: a client that PATCHes the whole
// object back unchanged should not be rejected for a field it did not alter.
func directoryOwnsRole(u store.User, ldapRoleSource, oidcRoleSource string) bool {
	switch u.AuthSource {
	case store.AuthSourceLDAP:
		return ldapRoleSource == rolemap.SourceDirectory
	case store.AuthSourceOIDC:
		return oidcRoleSource == rolemap.SourceDirectory
	default:
		return false
	}
}

// passwordEditable reports whether this account has a local password to set at
// all. Only local accounts do: an LDAP or OIDC account's credentials live with
// the provider, and its stored hash is an unusable placeholder that no password
// would ever match.
//
// Like directoryOwnsRole above, this is deliberately a package-level function
// and the single source of truth for the rule. Its three consumers — the admin
// guard in setPassword, the self-service guard in changePassword, and the
// password_editable field on the wire format (see toUserResponse) — must never
// be able to disagree, because a client that is told a password is settable and
// then gets a 409 is exactly the bug the field exists to prevent.
//
// Unlike directoryOwnsRole this needs no role-source arguments: the rule turns
// on auth_source alone. It stays a named predicate anyway so the wire field and
// the two guards share one definition rather than three copies of the same
// comparison.
func passwordEditable(u store.User) bool {
	return u.AuthSource == store.AuthSourceLocal
}

// externalAuthority names the system that owns an externally-authenticated
// account, and the config block an operator would go to in order to change its
// behavior. Both differ per source — pointing an SSO deployment at "the
// directory" and auth.ldap.* sends them looking for a system they may not even
// have — and the messages below are the only reason the distinction exists, so
// it is made once here rather than re-branched in each of them.
func externalAuthority(u store.User) (noun, configPrefix string) {
	if u.AuthSource == store.AuthSourceOIDC {
		return "the identity provider", "auth.oidc"
	}
	return "the directory", "auth.ldap"
}

// directoryRoleConflictDetail explains a rejected role edit on an account whose
// role the provider recomputes at every login.
func directoryRoleConflictDetail(u store.User) string {
	noun, prefix := externalAuthority(u)
	return fmt.Sprintf(
		"this account's role is managed by %s (%s.role_source=directory); change the user's group membership instead",
		noun, prefix,
	)
}

// externalPasswordSetConflictDetail is the counterpart for an admin setting a
// password on an externally-authenticated account.
func externalPasswordSetConflictDetail(u store.User) string {
	noun, _ := externalAuthority(u)
	return fmt.Sprintf("this account authenticates against %s and has no local password", noun)
}

// externalPasswordConflictDetail is the same wording for a user changing their
// own password.
func externalPasswordConflictDetail(u store.User) string {
	noun, _ := externalAuthority(u)
	return fmt.Sprintf("your account authenticates against %s; change your password there", noun)
}

func (h *usersHandler) notFoundOr500(w http.ResponseWriter, r *http.Request, err error, verb string) {
	if errors.Is(err, store.ErrNotFound) {
		writeProblem(w, r, http.StatusNotFound, "user not found")
		return
	}
	h.logger.ErrorContext(r.Context(), "users: "+verb+" failed", slog.Any("error", err))
	writeProblem(w, r, http.StatusInternalServerError, "failed to "+verb+" user")
}
