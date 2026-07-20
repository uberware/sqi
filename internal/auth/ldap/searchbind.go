// SPDX-License-Identifier: AGPL-3.0-or-later

package ldap

import (
	"context"
	"fmt"

	ldapv3 "github.com/go-ldap/ldap/v3"
)

// matchingRuleInChain is Active Directory's LDAP_MATCHING_RULE_IN_CHAIN OID.
// A filter using it walks nested group membership transitively, which the
// flat memberOf attribute does not.
const matchingRuleInChain = "1.2.840.113556.1.4.1941"

// searchBindVerifier authenticates by binding as a service account, searching
// for the user's entry, then re-binding as that entry with the user's
// password.
type searchBindVerifier struct {
	cfg  Config
	dial dialFunc
}

var _ Verifier = (*searchBindVerifier)(nil)

func newSearchBind(cfg Config, dial dialFunc) *searchBindVerifier {
	return &searchBindVerifier{cfg: cfg, dial: dial}
}

// Verify implements Verifier.
func (v *searchBindVerifier) Verify(ctx context.Context, username, pw string) (Identity, error) {
	// An empty password must never reach the directory: LDAP treats a bind
	// with an empty password as an *unauthenticated* bind and returns
	// success, which would authenticate anyone who submits a blank password.
	if pw == "" {
		return Identity{}, ErrInvalidCredentials
	}

	c, err := v.dial(ctx)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	// Best-effort close of a directory connection.
	defer func() { _ = c.Close() }()

	if err := c.Bind(v.cfg.BindDN, v.cfg.BindPassword); err != nil {
		// The service account is ours, not the user's: a failure here is an
		// operations problem and must not be reported as a bad password.
		return Identity{}, fmt.Errorf("%w: service account bind: %w", ErrUnavailable, err)
	}

	e, err := v.findUser(c, username)
	if err != nil {
		return Identity{}, err
	}

	if err := c.Bind(e.DN, pw); err != nil {
		return Identity{}, ErrInvalidCredentials
	}

	groups := e.GetAttributeValues("memberOf")
	if v.cfg.NestedGroups {
		nested, nerr := v.nestedGroups(c, e.DN)
		if nerr == nil {
			groups = nested
		}
		// A failed nested lookup falls back to the flat memberOf values
		// already in hand rather than failing the login: degraded group
		// resolution beats locking everyone out of a working directory.
	}

	name := firstAttr(e, v.cfg.UsernameAttr)
	if name == "" {
		name = username
	}
	return Identity{
		DN:          e.DN,
		Username:    name,
		DisplayName: firstAttr(e, v.cfg.DisplayNameAttr),
		Groups:      groups,
	}, nil
}

// findUser searches BaseDN for the single entry matching username.
func (v *searchBindVerifier) findUser(c conn, username string) (*ldapv3.Entry, error) {
	filter := fmt.Sprintf(v.cfg.UserFilter, EscapeUsernameFilter(username))
	req := ldapv3.NewSearchRequest(
		v.cfg.BaseDN,
		ldapv3.ScopeWholeSubtree, ldapv3.NeverDerefAliases,
		2, // size limit 2: enough to detect an ambiguous filter, no more
		searchTimeLimit(v.cfg.Timeout),
		false,
		filter,
		[]string{v.cfg.UsernameAttr, v.cfg.DisplayNameAttr, "memberOf"},
		nil,
	)
	res, err := c.Search(req)
	if err != nil {
		return nil, fmt.Errorf("%w: user search: %w", ErrUnavailable, err)
	}
	// Zero entries and two entries are both credential failures from the
	// caller's point of view, but for different reasons: nobody matched, or
	// the filter is ambiguous and binding as "the first" would be a coin
	// flip over which identity the caller receives.
	if len(res.Entries) != 1 {
		return nil, ErrInvalidCredentials
	}
	return res.Entries[0], nil
}

// nestedGroups resolves transitive group membership for userDN.
func (v *searchBindVerifier) nestedGroups(c conn, userDN string) ([]string, error) {
	filter := fmt.Sprintf("(member:%s:=%s)", matchingRuleInChain, ldapv3.EscapeFilter(userDN))
	req := ldapv3.NewSearchRequest(
		v.cfg.BaseDN,
		ldapv3.ScopeWholeSubtree, ldapv3.NeverDerefAliases,
		0, searchTimeLimit(v.cfg.Timeout), false,
		filter,
		[]string{"dn"},
		nil,
	)
	res, err := c.Search(req)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(res.Entries))
	for _, e := range res.Entries {
		out = append(out, e.DN)
	}
	return out, nil
}
