// SPDX-License-Identifier: AGPL-3.0-or-later

package ldap

import "github.com/uberware/sqi/internal/auth/rolemap"

// MapRole returns the role for the entry's groups. Delegates to
// internal/auth/rolemap so LDAP and OIDC cannot drift apart on precedence.
func (c Config) MapRole(groups []string) (string, bool) {
	return rolemap.Map(c.RoleMap, c.DefaultRole, groups)
}
