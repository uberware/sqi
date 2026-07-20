// SPDX-License-Identifier: AGPL-3.0-or-later

// Package oidc will authenticate sqi web UI logins against an OAuth2/OIDC
// identity provider (Phase 3, C2).
//
// Today this package holds only the mode constants shared between
// internal/config (which validates an operator's auth.oidc.* configuration
// against them) and internal/api (which will act on them once the callback
// route exists). A later task grows it into the provider itself: discovery,
// the authorization-code exchange, ID-token verification, and claim mapping.
//
// The constants live here rather than in internal/config so that
// internal/api can depend on them without importing internal/config —
// internal/api must not import the config loader, which is also why
// toLDAPConfig exists in the LDAP integration.
package oidc

// Reauth modes control when the provider is asked to re-prompt for
// credentials (prompt=login) on login.
const (
	// ReauthAfterLogout re-prompts only on the login that follows an
	// explicit logout, so "log out" means the next person at a shared
	// workstation must authenticate. This is the default.
	ReauthAfterLogout = "after_logout"
	// ReauthAlways re-prompts on every login. A hard guarantee, at the cost
	// of SSO's silent-login convenience.
	ReauthAlways = "always"
	// ReauthNever never re-prompts; silent re-login is always permitted.
	ReauthNever = "never"
)

// Logout modes control whether logging out of sqi also ends the session at
// the identity provider.
const (
	// LogoutLocal ends only the local sqi session. This is the default —
	// provider logout signs the user out of every company tool sharing that
	// provider, so it is off unless asked for.
	LogoutLocal = "local"
	// LogoutProvider also redirects through the provider's end-session
	// endpoint, ending the session everywhere it is trusted.
	LogoutProvider = "provider"
)

// Role sources decide who owns an SSO user's role after provisioning.
const (
	// RoleSourceDirectory recomputes the role from claims on every login;
	// the users API rejects role edits on an SSO account.
	RoleSourceDirectory = "directory"
	// RoleSourceLocal has claims seed the role at provisioning only;
	// admins own it afterwards and the API allows edits.
	RoleSourceLocal = "local"
)
