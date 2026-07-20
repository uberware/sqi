// SPDX-License-Identifier: AGPL-3.0-or-later

// Package oidc authenticates sqi web UI logins against an OAuth2/OIDC identity
// provider (Phase 3, C2).
//
// It provides the login-time half of the authorization-code flow with PKCE:
//
//   - Provider (provider.go) — lazy, retried discovery; AuthCodeURL to start a
//     login; Exchange to redeem the code and return a verified Identity with
//     signature, issuer, audience, expiry, and nonce all checked; EndSessionURL
//     for RP-initiated logout.
//   - FlowState (state.go) — the HMAC-signed state cookie carrying the state
//     value, nonce, and PKCE verifier across the redirect to the provider.
//   - Config and Identity (config.go) — the package-side configuration shape and
//     the claim-mapped result of a successful login, including role mapping.
//   - The mode constants below, describing reauth, logout, and role-source
//     policy.
//
// The package holds no HTTP routes and no persistence: it verifies a login and
// hands back an Identity, leaving session issuance and account provisioning to
// internal/api.
//
// Config is defined here rather than reused from internal/config so that this
// package and internal/api can depend on it without importing the config
// loader — the same reason toLDAPConfig exists in the LDAP integration.
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
