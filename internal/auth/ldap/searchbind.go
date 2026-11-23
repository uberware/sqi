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
	// with an empty password as an unauthenticated bind and returns success,
	// which would authenticate anyone who submits a blank password.
	if pw == "" {
		return Identity{}, ErrInvalidCredentials
	}

	c, err := v.dial(ctx)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	// Best-effort close of a directory connection.
	defer func() { _ = c.Close() }()

	// An empty BindDN means anonymous search: an OpenLDAP deployment where
	// the directory is world-readable and no service account exists. go-ldap
	// rejects a bind of "" with "" client-side before any network I/O, so
	// binding would fail every login as ErrUnavailable. Skipping the bind
	// leaves the connection anonymous.
	if v.cfg.BindDN != "" {
		if err := c.Bind(v.cfg.BindDN, v.cfg.BindPassword); err != nil {
			// The service account is ours, not the user's: a failure here is an
			// operations problem and must not be reported as a bad password.
			return Identity{}, fmt.Errorf("%w: service account bind: %w", ErrUnavailable, err)
		}
	}

	e, err := v.findUser(c, username)
	if err != nil {
		return Identity{}, err
	}

	// Groups are resolved before the user re-bind, while the connection
	// still carries the service account's rights. The nested expansion
	// searches group objects, which ordinary users often may not read; run
	// after the re-bind it would fail on every login and always fall back to
	// flat memberOf.
	//
	// Nothing here is returned to the caller unless the bind below succeeds,
	// and that bind (the user's DN with the user's password) is the sole
	// gate.
	groups := v.resolveGroups(ctx, c, e)

	if err := c.Bind(e.DN, pw); err != nil {
		return Identity{}, ErrInvalidCredentials
	}

	name := firstAttr(e, v.cfg.UsernameAttr)
	if name == "" {
		name = username
	}
	return Identity{
		DN:          e.DN,
		Username:    name,
		DisplayName: firstAttr(e, v.cfg.DisplayNameAttr),
		ExternalID:  uniqueID(e, v.cfg.UniqueIDAttr),
		Groups:      groups,
	}, nil
}

// resolveGroups returns the group DNs for entry e, expanding nested
// membership when configured.
//
// A failed nested lookup falls back to the flat memberOf values already in
// hand rather than failing the login, so a broken expansion does not lock
// everyone out of a working directory. It is logged at WARN because the
// fallback is otherwise invisible: a persistently failing expansion demotes
// every admin who holds admin only through a nested group. ctx is used only
// to correlate the warning with its login; the search itself is bounded by
// the connection's timeout.
func (v *searchBindVerifier) resolveGroups(ctx context.Context, c conn, e *ldapv3.Entry) []string {
	groups := e.GetEqualFoldAttributeValues("memberOf")
	if !v.cfg.NestedGroups {
		return groups
	}
	nested, err := v.nestedGroups(c, e.DN)
	if err != nil {
		v.warnNestedFallback(ctx, e.DN, "error", err)
		return groups
	}
	// A successful search that matched nothing, for a user whose flat
	// memberOf is not empty, points to a misscoped search base. The
	// expansion reuses BaseDN, which is the user search base; with base_dn
	// set to "OU=Users,..." while groups live under "OU=Groups", every login
	// gets a successful, empty result. Replacing the flat memberOf with that
	// would map every user, admins included, onto default_role with no
	// error. Keep the flat values and warn, as the error path does.
	if len(nested) == 0 && len(groups) > 0 {
		v.warnNestedFallback(ctx, e.DN, "reason",
			"nested search succeeded but matched no groups; check that auth.ldap.base_dn covers the group tree")
		return groups
	}
	return nested
}

// warnNestedFallback logs the one WARN that makes a degraded expansion
// visible. Both fallback paths share it, so an operator watching for "nested
// group expansion failed" also sees the misscoped-base_dn case, which is
// otherwise indistinguishable from a user with no groups.
func (v *searchBindVerifier) warnNestedFallback(ctx context.Context, userDN, key string, val any) {
	if v.cfg.Logger == nil {
		return
	}
	v.cfg.Logger.WarnContext(ctx, "ldap: nested group expansion failed, falling back to flat memberOf",
		"user_dn", userDN, key, val)
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
		// UniqueIDAttr must be named explicitly: entryUUID is an operational
		// attribute (RFC 4530), which directories omit from a response unless
		// asked for by name. Dropping it here does not error; every login
		// then carries an empty identifier and is refused.
		[]string{v.cfg.UsernameAttr, v.cfg.DisplayNameAttr, v.cfg.UniqueIDAttr, "memberOf"},
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
