// SPDX-License-Identifier: AGPL-3.0-or-later

package client

import (
	"testing"

	"github.com/uberware/sqi/internal/auth/session"
)

// TestDefaultCookie_MatchesTheServer pins the client's session cookie name to
// the server's. The client keeps its own copy because importing the session
// package would pull the store and the password hashing into it.
func TestDefaultCookie_MatchesTheServer(t *testing.T) {
	if defaultCookie != session.DefaultCookieName {
		t.Fatalf("defaultCookie = %q, the server's session cookie is %q", defaultCookie, session.DefaultCookieName)
	}
}
