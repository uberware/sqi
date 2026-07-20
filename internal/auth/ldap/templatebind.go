// SPDX-License-Identifier: AGPL-3.0-or-later

package ldap

import (
	"context"
	"fmt"

	ldapv3 "github.com/go-ldap/ldap/v3"
)

// templateBindVerifier authenticates by formatting the user's DN from a
// template and binding as it directly — no service account.
//
// Because there is no service account, group resolution has to run on the
// user's OWN authenticated connection: after a successful bind it reads
// memberOf from the user's entry. That inherits memberOf's limitation —
// nested groups are not expanded — which is why config validation rejects
// nested_groups in this mode.
type templateBindVerifier struct {
	cfg  Config
	dial dialFunc
}

var _ Verifier = (*templateBindVerifier)(nil)

func newTemplateBind(cfg Config, dial dialFunc) *templateBindVerifier {
	return &templateBindVerifier{cfg: cfg, dial: dial}
}

// Verify implements Verifier.
func (v *templateBindVerifier) Verify(ctx context.Context, username, pw string) (Identity, error) {
	// See searchBindVerifier.Verify: an empty password is an unauthenticated
	// bind, which the directory answers with success.
	if pw == "" {
		return Identity{}, ErrInvalidCredentials
	}

	c, err := v.dial(ctx)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	// Best-effort close of a directory connection.
	defer func() { _ = c.Close() }()

	dn := fmt.Sprintf(v.cfg.UserDNTemplate, EscapeUsernameDN(username))
	if err := c.Bind(dn, pw); err != nil {
		return Identity{}, ErrInvalidCredentials
	}

	id := Identity{DN: dn, Username: username}
	// Read the entry on the now-authenticated connection. A directory that
	// hides a user's own attributes yields no groups, which MapRole resolves
	// to DefaultRole — the credentials were still proven valid by the bind,
	// so this must not fail the login.
	if e := v.readSelf(c, dn); e != nil {
		if name := firstAttr(e, v.cfg.UsernameAttr); name != "" {
			id.Username = name
		}
		id.DisplayName = firstAttr(e, v.cfg.DisplayNameAttr)
		id.Groups = attrValuesFold(e, "memberOf")
	}
	return id, nil
}

// readSelf fetches the bound user's own entry, or nil if unavailable.
func (v *templateBindVerifier) readSelf(c conn, dn string) *ldapv3.Entry {
	req := ldapv3.NewSearchRequest(
		dn,
		ldapv3.ScopeBaseObject, ldapv3.NeverDerefAliases,
		1, searchTimeLimit(v.cfg.Timeout), false,
		"(objectClass=*)",
		[]string{v.cfg.UsernameAttr, v.cfg.DisplayNameAttr, "memberOf"},
		nil,
	)
	res, err := c.Search(req)
	if err != nil || len(res.Entries) == 0 {
		return nil
	}
	return res.Entries[0]
}
