// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import "net/http"

// Authenticator inspects a request's credentials and resolves them to a
// Principal. A non-nil error means the request is unauthenticated; the caller
// (REST middleware or WebSocket hook) responds 401 and does not proceed.
//
// The same interface serves both the REST middleware and the WebSocket
// upgrade, since both receive the raw *http.Request. Supporting several
// concurrent credential types later is simply another implementation that
// tries each in turn.
type Authenticator interface {
	Authenticate(r *http.Request) (Principal, error)
}

// anonymousAuthenticator always yields the fixed anonymous superuser principal
// and never errors. It is used whenever auth is disabled (A0: always).
type anonymousAuthenticator struct{}

// Anonymous returns an Authenticator that authenticates every request as the
// anonymous superuser principal.
func Anonymous() Authenticator { return anonymousAuthenticator{} }

func (anonymousAuthenticator) Authenticate(*http.Request) (Principal, error) {
	return Principal{
		DisplayName: "anonymous",
		Kind:        KindAnonymous,
		Superuser:   true,
	}, nil
}
