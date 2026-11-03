// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nkeys"

	"github.com/uberware/sqi/internal/brokerauth"
)

const (
	// AccountAuth is the account that holds sqi's callout connection. Nothing
	// else lives in it.
	AccountAuth = "AUTH"

	// DefaultAccount is the account that holds sqi's streams, consumers, KV
	// bucket and workers, and sqi's data connection. Embedded mode always uses
	// it; external mode takes the name from configuration.
	DefaultAccount = "SQI"

	// accountSystem is the system account the rendered fragment declares.
	// Embedded mode leaves the system account to nats-server's default.
	accountSystem = "SYS"

	// accountGlobal is nats-server's built-in global account.
	accountGlobal = "$G"
)

// accountNameRE is the shape of an account name sqi accepts. It is narrower
// than nats-server's grammar so that the name can be written into a config
// file unquoted-safe and used as a directory name. internal/config applies the
// same rule to nats.external.account.
var accountNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Layout describes sqi's NATS account layout once, for both broker modes.
// [Layout.Apply] renders it as nats-server Options for the embedded broker;
// [Layout.ConfigFragment] renders the same layout as config-file text for an
// external cluster. The tests parse the fragment with nats-server's own config
// reader and compare the result with Apply's Options (accounts, each nkey's
// account and permissions, the callout, no_auth_user), and boot a server from
// each to check where JetStream places streams.
//
// The layout has two accounts. Account holds sqi's data and its data
// connection, which authenticates as DataUserPub with [brokerauth.ServerPermissions]
// (">" both ways) and is safe only because the account boundary scopes it.
// AUTH holds the callout connection, CalloutUserPub.
//
// With AuthEnabled, nats-server asks sqi's responder to authenticate every
// connection except the two sqi users, which are exempt through auth_users so
// that neither needs the responder to be running before it can connect. With
// auth off, there is no callout and anonymous connections are mapped to the
// data user, which is today's behavior inside a named account.
type Layout struct {
	// Account is the NATS account sqi's data lives in.
	Account string
	// AuthEnabled selects the auth-callout layout over the anonymous one.
	AuthEnabled bool
	// IssuerPub is the public account key the callout's user JWTs are signed
	// with. Required with AuthEnabled.
	IssuerPub string
	// DataUserPub is the public user key of sqi's data connection.
	DataUserPub string
	// CalloutUserPub is the public user key of sqi's callout connection.
	// Required with AuthEnabled; otherwise the AUTH account has no users.
	CalloutUserPub string
	// JetStreamMaxStore caps the file storage of Account's JetStream, in
	// bytes. 0 means unlimited.
	JetStreamMaxStore int64
}

// validate reports a layout nats-server would reject or, worse, accept with a
// meaning other than the one intended.
func (l Layout) validate() error {
	switch {
	case l.Account == "":
		return errors.New("bus: layout: account name is empty")
	case l.Account == AccountAuth || l.Account == accountSystem || l.Account == accountGlobal:
		return fmt.Errorf("bus: layout: account %q is reserved", l.Account)
	case !accountNameRE.MatchString(l.Account):
		return fmt.Errorf("bus: layout: account name %q must use only letters, digits, '_' and '-'", l.Account)
	case !nkeys.IsValidPublicUserKey(l.DataUserPub):
		return errors.New("bus: layout: data user is not a valid public user key")
	case l.JetStreamMaxStore < 0:
		return errors.New("bus: layout: negative JetStream storage cap")
	}
	if !l.AuthEnabled {
		return nil
	}
	if !nkeys.IsValidPublicAccountKey(l.IssuerPub) {
		return errors.New("bus: layout: issuer is not a valid public account key")
	}
	if !nkeys.IsValidPublicUserKey(l.CalloutUserPub) {
		return errors.New("bus: layout: callout user is not a valid public user key")
	}
	if l.CalloutUserPub == l.DataUserPub {
		return errors.New("bus: layout: the data and callout users must be different keys")
	}
	return nil
}

// hasCalloutUser reports whether the AUTH account gets a user. With auth off
// the callout connection does not exist.
func (l Layout) hasCalloutUser() bool {
	return l.AuthEnabled && l.CalloutUserPub != ""
}

// Apply sets the layout on o: Accounts, Nkeys, AuthCallout or NoAuthUser, and
// AlwaysEnableNonce. It replaces Accounts and Nkeys outright, so it expects
// Options that do not already define users.
//
// AlwaysEnableNonce is set in both modes. nats-server issues a nonce only when
// it has nkey users or trusted keys; the data user makes the first true here,
// but the flag keeps that from depending on how the layout is edited. The
// config-file form has no such key, so a cluster needs its own nkey users,
// which the fragment supplies.
//
// JetStream is not part of Apply. nats-server exposes no Options field that
// enables it for one named account, so the caller starts the server with
// Options.JetStream and then calls [Layout.EnableAccountJetStream].
func (l Layout) Apply(o *natsserver.Options) error {
	if err := l.validate(); err != nil {
		return err
	}

	data := natsserver.NewAccount(l.Account)
	auth := natsserver.NewAccount(AccountAuth)
	o.Accounts = []*natsserver.Account{data, auth}

	o.Nkeys = []*natsserver.NkeyUser{{
		Nkey:        l.DataUserPub,
		Permissions: brokerauth.ServerPermissions(),
		Account:     data,
	}}
	if l.hasCalloutUser() {
		o.Nkeys = append(o.Nkeys, &natsserver.NkeyUser{Nkey: l.CalloutUserPub, Account: auth})
	}

	o.AlwaysEnableNonce = true

	if l.AuthEnabled {
		o.NoAuthUser = ""
		o.AuthCallout = &natsserver.AuthCallout{
			Issuer:    l.IssuerPub,
			Account:   AccountAuth,
			AuthUsers: []string{l.DataUserPub, l.CalloutUserPub},
		}
		return nil
	}

	// nats-server resolves no_auth_user against nkey users by their public
	// key, so the data user itself serves. Anonymous connections therefore
	// land in Account with the data user's ">" grants, which is "no limits".
	o.AuthCallout = nil
	o.NoAuthUser = l.DataUserPub
	return nil
}

// jetStreamLimits is Account's JetStream limits: unlimited everywhere except
// file storage, which takes JetStreamMaxStore.
func (l Layout) jetStreamLimits() map[string]natsserver.JetStreamAccountLimits {
	maxStore := int64(-1)
	if l.JetStreamMaxStore > 0 {
		maxStore = l.JetStreamMaxStore
	}
	return map[string]natsserver.JetStreamAccountLimits{"": {
		MaxMemory:            -1,
		MaxStore:             maxStore,
		MaxStreams:           -1,
		MaxConsumers:         -1,
		MaxAckPending:        -1,
		MemoryMaxStreamBytes: -1,
		StoreMaxStreamBytes:  -1,
	}}
}

// EnableAccountJetStream enables JetStream on Account of a server started
// with Options.JetStream and the layout applied. Without it, JetStream is on
// for the server but for no account, and every stream request fails.
func (l Layout) EnableAccountJetStream(s *natsserver.Server) error {
	acc, err := s.LookupAccount(l.Account)
	if err != nil {
		return fmt.Errorf("bus: look up account %q: %w", l.Account, err)
	}
	if err := acc.EnableJetStream(l.jetStreamLimits(), nil); err != nil {
		return fmt.Errorf("bus: enable jetstream on account %q: %w", l.Account, err)
	}
	return nil
}

// ConfigFragment renders the layout as nats-server config text for an
// external cluster: an accounts block (Account with JetStream, AUTH, and an
// empty SYS), system_account, and then either an authorization block with
// auth_callout or a no_auth_user. The operator supplies everything else
// (listeners, TLS, clustering, the JetStream store).
//
// SYS is declared with no users. A harness or operator that needs a system
// user uses [Layout.ConfigFragmentWithSystemUser] rather than appending a
// second accounts block, which nats-server does not merge.
//
// It returns an error for a layout [Layout.Apply] would refuse: a reserved or
// malformed account name would otherwise render a duplicate map key, which the
// nats config parser resolves by silently keeping the last one.
func (l Layout) ConfigFragment() (string, error) {
	return l.ConfigFragmentWithSystemUser("")
}

// ConfigFragmentWithSystemUser is [Layout.ConfigFragment] with one nkey user
// in the SYS account. An empty key leaves SYS without users. With auth on, the
// user is also added to auth_users, so it authenticates without the callout.
// A non-empty key must be a user public key distinct from sqi's own users.
func (l Layout) ConfigFragmentWithSystemUser(systemUserPub string) (string, error) {
	if err := l.validate(); err != nil {
		return "", err
	}
	if systemUserPub != "" {
		switch {
		case !nkeys.IsValidPublicUserKey(systemUserPub):
			return "", errors.New("bus: layout: system user is not a valid public user key")
		case systemUserPub == l.DataUserPub || systemUserPub == l.CalloutUserPub:
			return "", errors.New("bus: layout: the system user must differ from sqi's own users")
		}
	}
	var b strings.Builder
	b.WriteString("# sqi account layout.\n")
	if l.AuthEnabled {
		b.WriteString("# Do not add no_auth_user: with auth_callout it would let anonymous clients in.\n")
	}
	b.WriteString("accounts {\n")
	fmt.Fprintf(&b, "  %q {\n", l.Account)
	b.WriteString("    jetstream: ")
	if l.JetStreamMaxStore > 0 {
		fmt.Fprintf(&b, "{max_store: %d}\n", l.JetStreamMaxStore)
	} else {
		b.WriteString("enabled\n")
	}
	b.WriteString("    users: [\n")
	fmt.Fprintf(&b, "      {nkey: %q, permissions: %s}\n", l.DataUserPub, renderPermissions(brokerauth.ServerPermissions()))
	b.WriteString("    ]\n  }\n")

	fmt.Fprintf(&b, "  %q {\n", AccountAuth)
	if l.hasCalloutUser() {
		fmt.Fprintf(&b, "    users: [{nkey: %q}]\n", l.CalloutUserPub)
	}
	b.WriteString("  }\n")

	fmt.Fprintf(&b, "  %q {\n", accountSystem)
	if systemUserPub != "" {
		fmt.Fprintf(&b, "    users: [{nkey: %q}]\n", systemUserPub)
	}
	b.WriteString("  }\n}\n")
	fmt.Fprintf(&b, "system_account: %q\n", accountSystem)

	if l.AuthEnabled {
		b.WriteString("authorization {\n  auth_callout {\n")
		fmt.Fprintf(&b, "    issuer: %q\n", l.IssuerPub)
		fmt.Fprintf(&b, "    account: %q\n", AccountAuth)
		authUsers := []string{l.DataUserPub, l.CalloutUserPub}
		if systemUserPub != "" {
			// A user outside auth_users is sent to the callout, which would
			// refuse an unenrolled key, so the system user must be exempt.
			authUsers = append(authUsers, systemUserPub)
		}
		fmt.Fprintf(&b, "    auth_users: [%s]\n", quoteList(authUsers))
		b.WriteString("  }\n}\n")
	} else {
		fmt.Fprintf(&b, "no_auth_user: %q\n", l.DataUserPub)
	}
	return b.String(), nil
}

// renderPermissions renders p's allow lists as a config-file permissions map.
// Deny lists and response permissions are not used by sqi's own users.
func renderPermissions(p *natsserver.Permissions) string {
	quote := func(subjects []string) string { return "[" + quoteList(subjects) + "]" }
	var parts []string
	if p.Publish != nil {
		parts = append(parts, "publish: {allow: "+quote(p.Publish.Allow)+"}")
	}
	if p.Subscribe != nil {
		parts = append(parts, "subscribe: {allow: "+quote(p.Subscribe.Allow)+"}")
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// quoteList renders items as a comma-separated list of quoted strings.
func quoteList(items []string) string {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(q, ", ")
}
