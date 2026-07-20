// SPDX-License-Identifier: AGPL-3.0-or-later

// Package rolemap resolves an external identity's group memberships to one of
// B1's roles.
//
// Shared by LDAP (group DNs) and OIDC (a groups claim) so the precedence rule
// cannot drift between them: an operator who reorders auth.ldap.role_map
// expects auth.oidc.role_map to behave the same way.
package rolemap

import "strings"

// Mapping is one group → role rule.
type Mapping struct {
	Group string
	Role  string
}

// Map returns the role for groups, or defaultRole when nothing matches.
//
// Iteration is over mappings, not over groups: config order IS the precedence
// mechanism, so a user in both an admin group and an artist group gets whatever
// the operator listed first. Iterating groups instead would make the result
// depend on the order the provider happened to return them — non-deterministic
// and unexplainable.
//
// The false return means "reject this login": an empty defaultRole is how a
// deployment requires group membership to sign in at all.
func Map(mappings []Mapping, defaultRole string, groups []string) (string, bool) {
	for _, m := range mappings {
		for _, g := range groups {
			if strings.EqualFold(strings.TrimSpace(g), strings.TrimSpace(m.Group)) {
				return m.Role, true
			}
		}
	}
	if defaultRole == "" {
		return "", false
	}
	return defaultRole, true
}
