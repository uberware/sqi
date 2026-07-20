// SPDX-License-Identifier: AGPL-3.0-or-later

package ldap

import "strings"

// MapRole resolves a role from the user's group DNs.
//
// First match in RoleMap order wins — config order IS the precedence
// mechanism, deliberately, so an operator reads their intent off the file
// instead of memorizing a built-in privilege ranking. The iteration is
// therefore over the mappings (ordered) and not over the groups (whatever
// order the directory happened to return).
//
// When nothing matches, DefaultRole applies. An empty DefaultRole means the
// deployment requires group membership to sign in at all, so ok is false and
// the caller must reject the login.
//
// DN comparison is case-insensitive: AD treats DNs that way and returns
// inconsistent casing in practice, so a case-sensitive compare would silently
// drop an admin to the default role.
func (c Config) MapRole(groups []string) (string, bool) {
	for _, m := range c.RoleMap {
		for _, g := range groups {
			if strings.EqualFold(strings.TrimSpace(g), strings.TrimSpace(m.Group)) {
				return m.Role, true
			}
		}
	}
	if c.DefaultRole == "" {
		return "", false
	}
	return c.DefaultRole, true
}
