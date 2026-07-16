// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"log/slog"
	"net/http"

	"github.com/uberware/sqi/internal/auth"
)

// Auth returns middleware that authenticates each request via authn and injects
// the resulting auth.Principal into the request context. On authentication
// failure it short-circuits with a 401 RFC 7807 problem-details response and
// does not call the wrapped handler.
//
// A nil authn is treated as auth.Anonymous() (auth disabled), so every request
// receives the anonymous superuser principal and behaviour is unchanged.
//
// Mount it on the REST resource routes only (not /ws, which is gated by its own
// upgrade hook, and not the public probes on the root router).
func Auth(authn auth.Authenticator, logger *slog.Logger) func(http.Handler) http.Handler {
	if authn == nil {
		authn = auth.Anonymous()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, err := authn.Authenticate(r)
			if err != nil {
				logger.DebugContext(
					r.Context(), "middleware: authentication failed",
					slog.String("path", r.URL.Path),
					slog.Any("error", err),
				)
				WriteProblem(w, r, http.StatusUnauthorized, "authentication required")
				return
			}
			next.ServeHTTP(w, r.WithContext(auth.NewContext(r.Context(), p)))
		})
	}
}
