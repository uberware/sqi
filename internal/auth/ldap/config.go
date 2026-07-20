// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ldap authenticates usernames and passwords against an LDAP or
// Active Directory server.
//
// LDAP is a login-time credential verifier, NOT an auth.Authenticator: a
// directory bind is far too expensive to run on every request, and the
// directory has no notion of a session. POST /auth/login calls Verify for
// accounts whose store.User.AuthSource is "ldap" and, on success, mints the
// same server-side session a local password login produces. Everything
// downstream — RBAC, owner binding, WebSocket scoping — is therefore
// unchanged.
package ldap

import (
	"errors"
	"log/slog"
	"time"
)

// Errors returned by a Verifier. Callers must not distinguish
// ErrInvalidCredentials from a missing directory entry in what they send the
// client: both are a generic 401, or the endpoint enumerates usernames.
var (
	// ErrInvalidCredentials means the directory rejected the bind, or no
	// entry matched the username.
	ErrInvalidCredentials = errors.New("ldap: invalid credentials")
	// ErrNoRoleMatch means the user authenticated but matched no role
	// mapping and DefaultRole is empty, so the deployment refuses the login.
	ErrNoRoleMatch = errors.New("ldap: no role mapping matched and no default role is configured")
	// ErrUnavailable means the directory could not be reached or the service
	// account bind failed — an infrastructure fault, not a bad password.
	ErrUnavailable = errors.New("ldap: directory unavailable")
)

// RoleMapping is one group-DN → role rule.
type RoleMapping struct {
	Group string
	Role  string
}

// Config configures directory authentication. It mirrors config.LDAPConfig;
// the server converts between them so this package stays independent of the
// config loader.
type Config struct {
	URL             string
	StartTLS        bool
	TLSSkipVerify   bool
	CAFile          string
	Timeout         time.Duration
	BindDN          string
	BindPassword    string
	BaseDN          string
	UserFilter      string
	NestedGroups    bool
	UserDNTemplate  string
	UsernameAttr    string
	DisplayNameAttr string
	RoleSource      string
	RoleMap         []RoleMapping
	DefaultRole     string
	// Logger receives the operational warnings this package cannot resolve on
	// its own — currently only a failed nested-group expansion, which degrades
	// silently to flat memberOf and would otherwise demote admins who hold
	// admin solely through a nested group with no signal anywhere. Optional:
	// nil is safe and disables that logging.
	Logger *slog.Logger
}

// RoleSourceDirectory and RoleSourceLocal are the two role-authority modes.
const (
	// RoleSourceDirectory recomputes an LDAP user's role from their groups on
	// every login; the users API rejects role edits on such accounts.
	RoleSourceDirectory = "directory"
	// RoleSourceLocal seeds the role from groups at JIT-create only; admins
	// own it afterwards.
	RoleSourceLocal = "local"
)

// TemplateMode reports whether this config selects template bind (bind
// directly as the user) rather than search-then-bind.
func (c Config) TemplateMode() bool { return c.UserDNTemplate != "" }

// Identity is what a successful Verify reports about the directory entry.
type Identity struct {
	// DN is the entry's distinguished name.
	DN string
	// Username is the login name as the directory spells it. Preferred over
	// the string the caller typed, so casing is consistent across logins.
	Username string
	// DisplayName is the human-facing label. Consumed only at JIT-create.
	DisplayName string
	// Groups are the group DNs the entry belongs to.
	Groups []string
}
