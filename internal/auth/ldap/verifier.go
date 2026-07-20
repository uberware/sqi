// SPDX-License-Identifier: AGPL-3.0-or-later

package ldap

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	ldapv3 "github.com/go-ldap/ldap/v3"
)

// Verifier authenticates a username/password against a directory and reports
// the entry's DN and group memberships.
//
// Implementations must return ErrInvalidCredentials for anything the user
// could have caused (bad password, no such entry) and ErrUnavailable for
// infrastructure faults, so the caller can log the second and stay silent
// about the first.
type Verifier interface {
	Verify(ctx context.Context, username, password string) (Identity, error)
}

// conn is the slice of the go-ldap client this package uses. Keeping it
// narrow and unexported is what makes both verifiers testable without a
// directory: every test drives a fake conn instead of a live server.
type conn interface {
	Bind(username, password string) error
	Search(req *ldapv3.SearchRequest) (*ldapv3.SearchResult, error)
	Close() error
}

// dialFunc opens a connection to the directory.
type dialFunc func(ctx context.Context) (conn, error)

// New returns the Verifier selected by cfg: template bind when
// UserDNTemplate is set, search-then-bind otherwise. Config validation has
// already rejected the ambiguous and empty cases.
func New(cfg Config) (Verifier, error) {
	dial, err := realDialer(cfg)
	if err != nil {
		return nil, err
	}
	if cfg.TemplateMode() {
		return newTemplateBind(cfg, dial), nil
	}
	return newSearchBind(cfg, dial), nil
}

// realDialer builds the production dialFunc. The TLS config is assembled once
// here rather than per-connection so a bad CA file fails at startup instead of
// on a user's first login attempt.
func realDialer(cfg Config) (dialFunc, error) {
	tlsCfg := &tls.Config{
		InsecureSkipVerify: cfg.TLSSkipVerify, //nolint:gosec // G402: operator-controlled, validated and WARN-logged at boot
		MinVersion:         tls.VersionTLS12,
	}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("ldap: read ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ldap: ca_file %q contains no usable certificates", cfg.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	// Bound the TCP connect itself. Without an explicit dialer go-ldap falls
	// back to its package-level DefaultTimeout of 60 seconds, so an operator
	// who wrote timeout: 5s would still wait a minute per login against a
	// blackholed directory. A non-positive timeout keeps go-ldap's default
	// rather than becoming an unbounded connect.
	opts := []ldapv3.DialOpt{ldapv3.DialWithTLSConfig(tlsCfg)}
	if cfg.Timeout > 0 {
		opts = append(opts, ldapv3.DialWithDialer(&net.Dialer{Timeout: cfg.Timeout}))
	}

	return func(_ context.Context) (conn, error) {
		c, err := ldapv3.DialURL(cfg.URL, opts...)
		if err != nil {
			return nil, err
		}
		// SetTimeout BEFORE StartTLS, not after. go-ldap arms a per-request
		// timer only when its internal requestTimeout is already > 0 at send
		// time, and SetTimeout is the only thing that sets it. Called after the
		// StartTLS branch, the StartTLS extended request would go out with no
		// timer at all and block forever against a server that completes the
		// TCP handshake and then goes silent.
		c.SetTimeout(cfg.Timeout)
		if cfg.StartTLS {
			if err := c.StartTLS(tlsCfg); err != nil {
				// Already failing; the StartTLS error is the one that matters.
				_ = c.Close()
				return nil, err
			}
		}
		return c, nil
	}, nil
}

// searchTimeLimit converts cfg.Timeout into the SearchRequest TimeLimit, which
// the protocol carries as whole seconds.
//
// The naive int(d.Seconds()) truncates, and RFC 4511 §4.5.1.5 gives 0 the
// meaning "no client-requested time limit" — so a configured timeout of, say,
// 500ms would reach the server as *unlimited*, the exact opposite of what the
// operator wrote. Config validation only requires timeout > 0, so sub-second
// values are reachable. Anything positive therefore floors to 1 second, the
// smallest limit the protocol can express. A non-positive duration keeps 0,
// where "no limit" is the honest reading of an unset timeout.
//
// The client-side deadline set by conn.SetTimeout still enforces the exact
// configured duration; this only governs the limit the server is asked for.
func searchTimeLimit(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	if secs := int(d.Seconds()); secs > 0 {
		return secs
	}
	return 1
}

// firstAttr returns the first value of the named attribute, or "".
func firstAttr(e *ldapv3.Entry, name string) string {
	if name == "" {
		return ""
	}
	vals := e.GetAttributeValues(name)
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}

// attrValuesFold returns the values of the named attribute, matching the
// attribute name case-insensitively.
//
// RFC 4512 §2.5 makes attribute descriptions case-insensitive, and a directory
// is free to echo back "memberof" or "MEMBEROF" whatever case was requested.
// Entry.GetAttributeValues compares byte-for-byte, so against such a server a
// case-sensitive lookup yields zero groups — which is not an error anywhere:
// it silently drops every role mapping and demotes the user to DefaultRole.
func attrValuesFold(e *ldapv3.Entry, name string) []string {
	for _, a := range e.Attributes {
		if strings.EqualFold(a.Name, name) {
			return a.Values
		}
	}
	return nil
}
