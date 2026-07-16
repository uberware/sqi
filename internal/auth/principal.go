// SPDX-License-Identifier: AGPL-3.0-or-later

// Package auth defines the authentication vocabulary shared across the sqi
// server surface: the request Principal, the pluggable Authenticator
// interface, and a no-op anonymous authenticator used when auth is disabled.
//
// A0 ships only the anonymous authenticator; real credential backends
// (sessions, API keys, LDAP, OIDC) are later Phase 3 components that implement
// this same interface.
package auth

import "context"

// Kind classifies the credential type behind a Principal.
type Kind string

const (
	// KindAnonymous is the stand-in identity injected when auth is disabled.
	KindAnonymous Kind = "anonymous"
	// KindUser is a human account (A1+).
	KindUser Kind = "user"
	// KindAPIKey is a long-lived machine credential (A2+).
	KindAPIKey Kind = "apikey"
	// KindService is a reserved internal/service identity.
	KindService Kind = "service"
)

// Principal is the authenticated identity attached to every request's context.
// It is populated by the auth middleware (REST) and the WebSocket hook.
type Principal struct {
	// Subject is the stable identity id; empty for the anonymous principal.
	Subject string
	// DisplayName is a human-facing label.
	DisplayName string
	// Roles are the identity's assigned roles. Empty in A0; populated from A1/B1.
	Roles []string
	// Kind classifies the credential type.
	Kind Kind
	// Superuser bypasses all authorization checks. Set on the anonymous
	// principal (auth off = no authorization) and reserved for bootstrap.
	Superuser bool
}

// principalCtxKey is the unexported context key for the request Principal.
type principalCtxKey struct{}

// NewContext returns a copy of ctx carrying p.
func NewContext(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

// FromContext returns the Principal carried by ctx, if any.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalCtxKey{}).(Principal)
	return p, ok
}
