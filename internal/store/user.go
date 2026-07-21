// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"time"
)

// Credential backends for [User.AuthSource].
const (
	// AuthSourceLocal verifies against the stored password hash.
	AuthSourceLocal = "local"
	// AuthSourceLDAP verifies against the configured directory.
	AuthSourceLDAP = "ldap"
	// AuthSourceOIDC verifies against the configured OIDC provider.
	AuthSourceOIDC = "oidc"
)

// User is a local account. PasswordHash is a Go-side field only; it is never
// marshaled into a REST response. Role is stored but not enforced until B1.
type User struct {
	ID           string
	Username     string
	DisplayName  string
	PasswordHash string
	Role         string
	// AuthSource names the credential backend that verifies this account:
	// [AuthSourceLocal] (the stored password hash), [AuthSourceLDAP] (the
	// directory), or [AuthSourceOIDC] (the OIDC provider). It is set at
	// creation and never changes — UpdateUser does not write it, so an account
	// cannot drift between backends.
	AuthSource string
	// ExternalID is the identity-provider-assigned identifier that is stable
	// across renames and is never reused: the OIDC "sub" claim, or the
	// directory attribute named by auth.ldap.unique_id_attr. Empty for local
	// accounts.
	//
	// This, not Username, is what an external login matches on. Matching on a
	// name lets a recycled address inherit a departed user's account and lets
	// a provider-side rename orphan one — both silently. Set at creation and
	// never changed: UpdateUser does not write it.
	ExternalID string
	Disabled   bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// UserStore is the persistence interface for [User] records.
type UserStore interface {
	// CreateUser inserts a new user. Returns [ErrConflict] if the username
	// (case-insensitive) already exists.
	CreateUser(ctx context.Context, u User) (User, error)
	// GetUser returns the user with the given ID, or [ErrNotFound].
	GetUser(ctx context.Context, id string) (User, error)
	// GetUserByUsername returns the user with the given username
	// (case-insensitive), or [ErrNotFound].
	GetUserByUsername(ctx context.Context, username string) (User, error)
	// GetUserByExternalID returns the account an external identity provider
	// owns, or [ErrNotFound]. authSource scopes the lookup so an OIDC "sub"
	// can never collide with an LDAP objectGUID.
	GetUserByExternalID(ctx context.Context, authSource, externalID string) (User, error)
	// ListUsers returns all users ordered by username.
	ListUsers(ctx context.Context) ([]User, error)
	// UpdateUser replaces display_name, role and disabled, and bumps
	// UpdatedAt. Returns [ErrNotFound].
	UpdateUser(ctx context.Context, u User) (User, error)
	// SetUserPassword replaces the stored password hash. Returns [ErrNotFound].
	SetUserPassword(ctx context.Context, id, passwordHash string) error
	// SetUserPasswordAndEvictSessions sets the password and deletes every
	// session for the user atomically. Returns [ErrNotFound] if id is unknown.
	//
	// The two must not be separate calls: a self-service password change tells
	// the user their other devices have been signed out, so a failure after
	// the password landed would make that claim a lie with no way to detect
	// it. Atomicity means the caller can report failure honestly — nothing
	// changed, retry.
	SetUserPasswordAndEvictSessions(ctx context.Context, id, passwordHash string) error
	// SetUserDisplayName updates only the display name, returning the updated
	// record. Returns [ErrNotFound] if id is unknown.
	//
	// Distinct from UpdateUser, which writes display_name, role, and disabled
	// together: a self-service caller may only touch the display name, and a
	// read-modify-write through UpdateUser would race with a concurrent admin
	// role or disabled change and silently revert it.
	SetUserDisplayName(ctx context.Context, id, displayName string) (User, error)
	// DeleteUser removes a user by ID (cascading their sessions). Returns
	// [ErrNotFound].
	DeleteUser(ctx context.Context, id string) error
	// CountUsers returns the number of users (for the bootstrap gate).
	CountUsers(ctx context.Context) (int, error)
	// CountAdmins returns the number of enabled accounts with role "admin"
	// (used by the last-admin lockout guard).
	CountAdmins(ctx context.Context) (int, error)
}
