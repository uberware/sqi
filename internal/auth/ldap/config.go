// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ldap authenticates usernames and passwords against an LDAP or
// Active Directory server.
//
// LDAP is a login-time credential verifier, not an auth.Authenticator: a
// directory bind is too expensive to run on every request, and the directory
// has no notion of a session. POST /auth/login calls Verify for accounts whose
// store.User.AuthSource is "ldap" and, on success, mints the same server-side
// session a local password login produces, so RBAC, owner binding and
// WebSocket scoping treat both kinds of account alike.
package ldap

import (
	"errors"
	"log/slog"
	"time"

	"github.com/uberware/sqi/internal/auth/rolemap"
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

// RoleMapping is one group-DN → role rule. It is an alias for
// [rolemap.Mapping], so a role map passes to rolemap without conversion.
type RoleMapping = rolemap.Mapping

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
	// UniqueIDAttr names the attribute holding the entry's stable,
	// non-reusable identifier: objectGUID on Active Directory, entryUUID on
	// OpenLDAP and other RFC 4530 servers. Required: no default is correct
	// on both, and auto-detection would guess wrong on a server exposing
	// both.
	UniqueIDAttr string
	RoleSource   string
	RoleMap      []rolemap.Mapping
	DefaultRole  string
	// Logger receives the warning for a failed or empty nested-group
	// expansion, which falls back to flat memberOf and can demote an admin
	// who holds admin only through a nested group. Optional: nil disables
	// that logging.
	Logger *slog.Logger
}

// RoleSourceDirectory and RoleSourceLocal are the two role-authority modes.
// They are the shared [internal/auth/rolemap] values under LDAP-facing names;
// the enum has exactly one definition so LDAP and OIDC cannot drift.
const (
	// RoleSourceDirectory recomputes an LDAP user's role from their groups on
	// every login; the users API rejects role edits on such accounts.
	RoleSourceDirectory = rolemap.SourceDirectory
	// RoleSourceLocal seeds the role from groups at JIT-create only; admins
	// own it afterwards.
	RoleSourceLocal = rolemap.SourceLocal
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
	// ExternalID is the entry's stable, non-reusable identifier, read from the
	// attribute named by Config.UniqueIDAttr. Empty means the directory did not
	// return it, which the login path treats as a failure rather than falling
	// back to name matching.
	ExternalID string
	// Groups are the group DNs the entry belongs to.
	Groups []string
}
