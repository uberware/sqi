// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"time"
)

// User is a local account. PasswordHash is a Go-side field only; it is never
// marshaled into a REST response. Role is stored but not enforced until B1.
type User struct {
	ID           string
	Username     string
	DisplayName  string
	PasswordHash string
	Role         string
	Disabled     bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
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
