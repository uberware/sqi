// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"errors"
	"net/http"
)

// chainAuthenticator tries each authenticator in order and returns the first
// Principal produced without error.
type chainAuthenticator struct {
	authns []Authenticator
}

// Chain returns an Authenticator that tries each authenticator in order and
// returns the first successful Principal. An authenticator with no credential
// to offer errors (e.g. apikey.ErrNoCredential) and the chain moves on. If
// every authenticator errors, the last error is returned so middleware.Auth
// renders a 401. Order is caller-defined; the server wires key-first so a
// machine's Bearer credential leads and the browser cookie is the fallback.
func Chain(authns ...Authenticator) Authenticator {
	return chainAuthenticator{authns: authns}
}

// Authenticate implements Authenticator.
func (c chainAuthenticator) Authenticate(r *http.Request) (Principal, error) {
	err := errors.New("auth: no authenticators configured")
	for _, a := range c.authns {
		p, aerr := a.Authenticate(r)
		if aerr == nil {
			return p, nil
		}
		err = aerr
	}
	return Principal{}, err
}
