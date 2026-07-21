// SPDX-License-Identifier: AGPL-3.0-or-later

package oidc

import (
	"log/slog"

	"github.com/uberware/sqi/internal/auth/rolemap"
)

// Config configures OAuth2/OIDC single sign-on. It mirrors config.OIDCConfig;
// the server converts between them so this package stays independent of the
// config loader, exactly as internal/auth/ldap does.
type Config struct {
	// Issuer is the provider's discovery root. Discovery is performed lazily
	// on first use, never in New: a briefly unreachable identity provider must
	// not stop the scheduler from starting.
	Issuer string
	// ClientID is the application's registered identifier. It is also the
	// audience every ID token must carry.
	ClientID string
	// ClientSecret is the application's secret, sent only to the token
	// endpoint. Never logged.
	ClientSecret string
	// RedirectURL is the absolute callback URL registered with the provider.
	RedirectURL string
	// Scopes requested at authorization. "openid" is added if absent — the
	// flow returns no ID token without it.
	Scopes []string

	// UsernameClaim names the claim holding the login name. Used for display
	// and uniqueness only; accounts are matched on "sub".
	UsernameClaim string
	// DisplayNameClaim names the claim holding the human-facing name.
	DisplayNameClaim string
	// GroupsClaim names the claim holding group memberships, fed to MapRole.
	GroupsClaim string
	// RoleSource is RoleSourceDirectory or RoleSourceLocal.
	RoleSource string
	// DefaultRole applies when no mapping matches. Empty rejects the login.
	DefaultRole string
	// RoleMap maps group claims to roles, first match wins; order is
	// precedence.
	RoleMap []rolemap.Mapping

	// ReauthMode is one of ReauthAfterLogout, ReauthAlways, ReauthNever.
	ReauthMode string
	// LogoutMode is LogoutLocal or LogoutProvider.
	LogoutMode string
	// PostLogoutRedirectURL is where the provider returns the browser after a
	// provider-side logout. Only read when LogoutMode is LogoutProvider.
	PostLogoutRedirectURL string
	// ButtonLabel is the login page's SSO button text.
	ButtonLabel string

	// Logger receives the operational warnings this package cannot resolve on
	// its own — currently only a failed discovery attempt, which would
	// otherwise present to a user as an unexplained login failure. Optional:
	// nil is safe and disables that logging.
	Logger *slog.Logger
}

// MapRole returns the role for an identity's groups. Delegates to
// internal/auth/rolemap so OIDC and LDAP cannot drift apart on precedence.
func (c Config) MapRole(groups []string) (string, bool) {
	return rolemap.Map(c.RoleMap, c.DefaultRole, groups)
}

// Identity is what a successful Exchange reports about the authenticated user.
type Identity struct {
	// Subject is the provider's stable, non-reusable identifier for the user
	// ("sub"). This is what accounts are matched on — never the username,
	// which providers allow people to change.
	Subject string
	// Username is the login name, read from Config.UsernameClaim.
	Username string
	// DisplayName is the human-facing label, read from
	// Config.DisplayNameClaim. Consumed only at provisioning.
	DisplayName string
	// Groups are the memberships read from Config.GroupsClaim.
	Groups []string
}
