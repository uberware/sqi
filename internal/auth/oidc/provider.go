// SPDX-License-Identifier: AGPL-3.0-or-later

package oidc

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sync"
	"time"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// discoveryTimeout bounds a single lazy discovery attempt. A login that has to
// wait on an unreachable provider should fail in seconds, not hang the request.
const discoveryTimeout = 15 * time.Second

// discoveryFailureBackoff is how long a failed discovery attempt is remembered.
// Within the window, callers are handed the recorded error immediately instead
// of queueing behind their own full-timeout attempt. It is deliberately short:
// the point is to collapse a thundering herd into one attempt, not to cache the
// failure — an unreachable provider must recover without a process restart.
const discoveryFailureBackoff = 5 * time.Second

// Errors returned by a Provider. Callers must not distinguish them in what
// they show the user: an ID token that fails any check is a failed login, and
// saying which check failed helps only an attacker probing the deployment.
var (
	// ErrDiscovery means the provider's discovery document could not be
	// fetched or parsed — an infrastructure fault, not a bad login. The next
	// attempt retries.
	ErrDiscovery = errors.New("oidc: provider discovery failed")
	// ErrTokenInvalid means the authorization code exchange returned no ID
	// token, or one that failed signature, issuer, audience, expiry, or nonce
	// validation.
	ErrTokenInvalid = errors.New("oidc: id token invalid")
)

// Provider is the login-time half of OIDC: build an authorization URL, redeem
// the code the browser comes back with, and (optionally) build the provider's
// end-session URL.
type Provider interface {
	// AuthCodeURL builds the URL the browser is redirected to in order to
	// start a login. forceReauth adds prompt=login.
	AuthCodeURL(state, nonce, codeChallenge string, forceReauth bool) (string, error)
	// Exchange redeems an authorization code and returns the verified
	// identity. Every ID-token check — signature, issuer, audience, expiry,
	// nonce — must pass or it returns an error.
	Exchange(ctx context.Context, code, codeVerifier, nonce string) (Identity, error)
	// EndSessionURL returns the provider's RP-initiated logout URL, or false
	// when discovery advertised no end_session_endpoint so the caller degrades
	// to local logout rather than emitting a broken URL.
	EndSessionURL(postLogoutRedirect string) (string, bool)
}

// provider implements Provider against a real identity provider.
//
// Discovery is lazy and is retried after failure. Deliberately NOT sync.Once:
// Once would cache the first failure forever, so a provider that was
// unreachable during the boot-time first login would require a server restart
// to recover. Configuration faults abort boot; network faults must not.
//
// The fetch itself happens OUTSIDE the mutex, with one attempt elected and the
// rest of a concurrent batch waiting on its result. Holding the lock across the
// network call made N simultaneous logins strictly serial — against a dead
// provider the Nth waited N×discoveryTimeout, each pinning an HTTP handler
// goroutine.
type provider struct {
	cfg Config

	mu       sync.Mutex
	discover *coreoidc.Provider
	metadata providerMetadata
	// inflight is the discovery attempt currently in progress, if any.
	inflight *discoveryCall
	// failedAt/failErr remember the most recent failure for
	// failureBackoff, so a herd arriving behind a dead provider costs one
	// attempt rather than one per caller.
	failedAt time.Time
	failErr  error
	// failureBackoff is discoveryFailureBackoff except in tests, which
	// zero it to assert that a failure is retried immediately.
	failureBackoff time.Duration
}

// discoveryCall is one in-progress discovery attempt. Waiters block on done and
// then read d/err, which the electee writes before closing it.
type discoveryCall struct {
	done chan struct{}
	d    *coreoidc.Provider
	err  error
}

// providerMetadata holds the discovery-document fields go-oidc does not expose
// through its own accessors.
type providerMetadata struct {
	EndSessionEndpoint string `json:"end_session_endpoint"`
}

// New builds a Provider. It performs no network I/O — the identity provider is
// not contacted until the first login.
func New(cfg Config) Provider {
	return &provider{cfg: cfg, failureBackoff: discoveryFailureBackoff}
}

// resolve returns the discovered provider, fetching it on first use and after
// any previous failure. Concurrent callers share a single attempt.
func (p *provider) resolve(ctx context.Context) (*coreoidc.Provider, error) {
	call, wait := p.claimDiscovery()
	if wait != nil {
		return wait.d, wait.err
	}
	if call == nil {
		// A batch is already in flight; join it rather than starting another.
		return p.joinDiscovery(ctx)
	}

	d, md, err := p.fetch(ctx)

	p.mu.Lock()
	p.inflight = nil
	if err != nil {
		p.failedAt = time.Now()
		p.failErr = err
		call.err = err
	} else {
		p.discover = d
		p.metadata = md
		p.failErr = nil
		call.d = d
	}
	p.mu.Unlock()
	close(call.done)
	return d, err
}

// claimDiscovery decides this caller's role under the lock. It returns a
// non-nil call when this caller must perform the fetch, a non-nil wait when the
// answer is already known, and (nil, nil) when a fetch is in flight to join.
func (p *provider) claimDiscovery() (call, settled *discoveryCall) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.discover != nil {
		return nil, &discoveryCall{d: p.discover}
	}
	if p.failErr != nil && time.Since(p.failedAt) < p.failureBackoff {
		return nil, &discoveryCall{err: p.failErr}
	}
	if p.inflight != nil {
		return nil, nil
	}
	p.inflight = &discoveryCall{done: make(chan struct{})}
	return p.inflight, nil
}

// joinDiscovery waits for the in-flight attempt claimed by another caller.
func (p *provider) joinDiscovery(ctx context.Context) (*coreoidc.Provider, error) {
	p.mu.Lock()
	call := p.inflight
	p.mu.Unlock()
	if call == nil {
		// It finished between claimDiscovery and here; re-read the result.
		return p.resolve(ctx)
	}
	select {
	case <-call.done:
		return call.d, call.err
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %w", ErrDiscovery, ctx.Err())
	}
}

// fetch performs the discovery request. It touches no provider state, so it is
// safe to call with the mutex released.
func (p *provider) fetch(ctx context.Context) (*coreoidc.Provider, providerMetadata, error) {
	d, err := coreoidc.NewProvider(ctx, p.cfg.Issuer)
	if err != nil {
		if p.cfg.Logger != nil {
			p.cfg.Logger.WarnContext(ctx, "oidc discovery failed; will retry on the next login",
				"issuer", p.cfg.Issuer, "error", err)
		}
		return nil, providerMetadata{}, fmt.Errorf("%w: %w", ErrDiscovery, err)
	}

	var md providerMetadata
	if err := d.Claims(&md); err != nil && p.cfg.Logger != nil {
		// Not fatal: only end_session_endpoint lives here, and its absence
		// degrades to local logout.
		p.cfg.Logger.WarnContext(ctx, "oidc discovery document has unreadable extra claims",
			"issuer", p.cfg.Issuer, "error", err)
	}
	return d, md, nil
}

// oauth2Config builds the OAuth2 client config against a discovered provider.
func (p *provider) oauth2Config(d *coreoidc.Provider) oauth2.Config {
	scopes := slices.Clone(p.cfg.Scopes)
	if !slices.Contains(scopes, coreoidc.ScopeOpenID) {
		// Without "openid" the provider returns no ID token at all, and every
		// downstream check would have nothing to verify.
		scopes = append([]string{coreoidc.ScopeOpenID}, scopes...)
	}
	return oauth2.Config{
		ClientID:     p.cfg.ClientID,
		ClientSecret: p.cfg.ClientSecret,
		Endpoint:     d.Endpoint(),
		RedirectURL:  p.cfg.RedirectURL,
		Scopes:       scopes,
	}
}

// AuthCodeURL implements Provider.
func (p *provider) AuthCodeURL(state, nonce, codeChallenge string, forceReauth bool) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), discoveryTimeout)
	defer cancel()

	d, err := p.resolve(ctx)
	if err != nil {
		return "", err
	}

	opts := []oauth2.AuthCodeOption{
		coreoidc.Nonce(nonce),
		// PKCE. The challenge is computed by the caller from the verifier it
		// sealed into the state cookie, so the code is useless to anyone who
		// intercepts it without that cookie.
		oauth2.SetAuthURLParam("code_challenge", codeChallenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	}
	if forceReauth {
		opts = append(opts, oauth2.SetAuthURLParam("prompt", "login"))
	}

	cfg := p.oauth2Config(d)
	return cfg.AuthCodeURL(state, opts...), nil
}

// Exchange implements Provider.
func (p *provider) Exchange(ctx context.Context, code, codeVerifier, nonce string) (Identity, error) {
	d, err := p.resolve(ctx)
	if err != nil {
		return Identity{}, err
	}

	cfg := p.oauth2Config(d)
	tok, err := cfg.Exchange(ctx, code, oauth2.VerifierOption(codeVerifier))
	if err != nil {
		return Identity{}, fmt.Errorf("oidc: code exchange failed: %w", err)
	}

	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return Identity{}, fmt.Errorf("%w: token response carried no id_token", ErrTokenInvalid)
	}

	// Verify checks the signature against the provider's JWKS (refetching on
	// an unknown key id, so key rotation is tolerated), the issuer, the
	// audience, and expiry. Every SkipXCheck is left false on purpose.
	idToken, err := d.Verifier(&coreoidc.Config{ClientID: p.cfg.ClientID}).Verify(ctx, raw)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrTokenInvalid, err)
	}

	// Nonce is not the library's job. Reject an empty nonce on either side
	// BEFORE comparing: subtle.ConstantTimeCompare(nil, nil) returns 1, so two
	// empty nonces would compare equal and the only hand-written check in this
	// package would silently pass. The caller's OpenState happens to reject an
	// empty nonce today, but a security check must not rest on an invariant
	// enforced in another file.
	if nonce == "" || idToken.Nonce == "" {
		return Identity{}, fmt.Errorf("%w: nonce missing", ErrTokenInvalid)
	}
	// Constant-time, like any other secret comparison: a byte-by-byte compare
	// leaks how much of a guess is right.
	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(nonce)) != 1 {
		return Identity{}, fmt.Errorf("%w: nonce mismatch", ErrTokenInvalid)
	}

	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("%w: claims unreadable: %w", ErrTokenInvalid, err)
	}

	// An identity with no subject or no username is not a usable login: the
	// subject is what accounts are matched on, and a username claim that reads
	// empty would provision every SSO user with a blank name. Both are almost
	// always a claim-name misconfiguration, so the error says which claim.
	if idToken.Subject == "" {
		return Identity{}, fmt.Errorf("%w: id token has an empty \"sub\" claim", ErrTokenInvalid)
	}
	username := stringClaim(claims, p.cfg.UsernameClaim)
	if username == "" {
		return Identity{}, fmt.Errorf("%w: username claim %q is absent, null, or not a string",
			ErrTokenInvalid, p.cfg.UsernameClaim)
	}

	return Identity{
		Subject:     idToken.Subject,
		Username:    username,
		DisplayName: stringClaim(claims, p.cfg.DisplayNameClaim),
		Groups:      stringsClaim(claims, p.cfg.GroupsClaim),
	}, nil
}

// EndSessionURL implements Provider.
//
// It never emits an id_token_hint. sqi does not store ID tokens: doing so
// would put the first plaintext bearer secret into a schema that otherwise
// holds only hashes, to save a redirect. Providers that require the hint will
// simply re-prompt, which is the correct outcome for a logout.
func (p *provider) EndSessionURL(postLogoutRedirect string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), discoveryTimeout)
	defer cancel()

	if _, err := p.resolve(ctx); err != nil {
		return "", false
	}

	p.mu.Lock()
	endpoint := p.metadata.EndSessionEndpoint
	p.mu.Unlock()
	if endpoint == "" {
		// The provider does not support RP-initiated logout. Report that
		// rather than guessing a URL, so the caller falls back to local logout.
		return "", false
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		return "", false
	}
	q := u.Query()
	q.Set("client_id", p.cfg.ClientID)
	if postLogoutRedirect == "" {
		postLogoutRedirect = p.cfg.PostLogoutRedirectURL
	}
	if postLogoutRedirect != "" {
		q.Set("post_logout_redirect_uri", postLogoutRedirect)
	}
	u.RawQuery = q.Encode()
	return u.String(), true
}

// stringClaim reads a string-valued claim, returning "" when the claim is
// absent, null, or not a string.
func stringClaim(claims map[string]any, name string) string {
	if name == "" {
		return ""
	}
	if s, ok := claims[name].(string); ok {
		return s
	}
	return ""
}

// stringsClaim reads a groups-style claim.
//
// It tolerates a bare string as well as an array, because some providers emit
// a bare string when the user is in exactly one group. Reading only the array
// form would silently yield no groups for those users, and therefore the
// default role — a silent privilege downgrade that looks like correct
// behavior.
func stringsClaim(claims map[string]any, name string) []string {
	if name == "" {
		return nil
	}
	switch v := claims[name].(type) {
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	default:
		return nil
	}
}
