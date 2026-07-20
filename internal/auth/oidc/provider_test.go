// SPDX-License-Identifier: AGPL-3.0-or-later

package oidc

import (
	"errors"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/auth/rolemap"
)

// asProvider reaches through the Provider interface to the implementation, so
// a test can override the failure backoff. Checked rather than a bare
// assertion: forcetypeassert rejects the one-value form.
func asProvider(t *testing.T, p Provider) *provider {
	t.Helper()
	impl, ok := p.(*provider)
	if !ok {
		t.Fatalf("New returned %T, want *provider", p)
	}
	return impl
}

func testConfig(issuer string) Config {
	return Config{
		Issuer:                issuer,
		ClientID:              testClientID,
		ClientSecret:          "test-secret",
		RedirectURL:           "https://sqi.example.com/api/v1/auth/oidc/callback",
		Scopes:                []string{"profile", "email"},
		UsernameClaim:         "preferred_username",
		DisplayNameClaim:      "name",
		GroupsClaim:           "groups",
		RoleSource:            RoleSourceDirectory,
		DefaultRole:           "viewer",
		ReauthMode:            ReauthAfterLogout,
		LogoutMode:            LogoutProvider,
		PostLogoutRedirectURL: "https://sqi.example.com/login",
		ButtonLabel:           "Sign in with SSO",
	}
}

// TestProvider_Exchange is the ID-token validation suite. Each case breaks
// exactly one property, so removing any single check in the implementation
// makes that case — and only that case — fail.
func TestProvider_Exchange(t *testing.T) {
	noop := func(*fakeIDP) {}
	tests := []struct {
		name   string
		mutate func(*fakeIDP)
		// mutateCfg breaks the relying party's configuration rather than the
		// provider's response.
		mutateCfg func(*Config)
		// noCallerNonce makes Exchange be called with an empty nonce.
		noCallerNonce bool
		wantErr       bool
	}{
		{name: "valid", mutate: noop},
		{name: "wrong issuer", mutate: func(f *fakeIDP) { f.issuerOverride = "https://evil.example.com" }, wantErr: true},
		{name: "wrong audience", mutate: func(f *fakeIDP) { f.audienceOverride = "someone-else" }, wantErr: true},
		{name: "expired", mutate: func(f *fakeIDP) { f.expiryOffset = -time.Hour }, wantErr: true},
		{name: "signature from an unpublished key", mutate: func(f *fakeIDP) { f.signWithRogueKey = true }, wantErr: true},
		{name: "nonce mismatch", mutate: func(f *fakeIDP) { f.nonceOverride = "not-the-one-we-sent" }, wantErr: true},
		// The case a constant-time compare alone lets through:
		// subtle.ConstantTimeCompare(nil, nil) returns 1, so two empty nonces
		// compare equal and the check silently no-ops.
		{
			name: "nonce empty on both sides", mutate: func(f *fakeIDP) { f.nonce = "" },
			noCallerNonce: true, wantErr: true,
		},
		{name: "provider omits the nonce claim", mutate: func(f *fakeIDP) { f.nonce = "" }, wantErr: true},
		{name: "caller supplies no nonce", mutate: noop, noCallerNonce: true, wantErr: true},
		// An empty subject is what accounts would be matched on; an empty
		// username would be provisioned verbatim. Both are rejected here so the
		// error names the claim instead of surfacing later as a bad account.
		{name: "empty subject", mutate: func(f *fakeIDP) { f.subject = "" }, wantErr: true},
		{name: "username claim reads empty", mutate: func(f *fakeIDP) { f.username = "" }, wantErr: true},
		{
			name: "username claim misconfigured", mutate: noop,
			mutateCfg: func(c *Config) { c.UsernameClaim = "no_such_claim" }, wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idp := newFakeIDP(t)
			tt.mutate(idp)
			cfg := testConfig(idp.URL())
			if tt.mutateCfg != nil {
				tt.mutateCfg(&cfg)
			}
			p := New(cfg)

			nonce := "nonce-1"
			if tt.noCallerNonce {
				nonce = ""
			}
			id, err := p.Exchange(t.Context(), "code-1", "verifier-1", nonce)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got identity %+v", id)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if id.Subject != "sub-1" {
				t.Fatalf("Subject = %q, want sub-1", id.Subject)
			}
			if id.Username != "alice" {
				t.Fatalf("Username = %q, want alice", id.Username)
			}
			if id.DisplayName != "Alice Example" {
				t.Fatalf("DisplayName = %q, want Alice Example", id.DisplayName)
			}
			if !slices.Equal(id.Groups, []string{"artists"}) {
				t.Fatalf("Groups = %v, want [artists]", id.Groups)
			}
		})
	}
}

// TestProvider_Exchange_GroupsClaimShapes covers the bare-string form some
// providers emit when the user is in exactly one group. Reading only the array
// form would yield no groups and therefore the default role: a silent
// privilege downgrade that looks like normal behavior.
func TestProvider_Exchange_GroupsClaimShapes(t *testing.T) {
	tests := []struct {
		name   string
		groups any
		want   []string
	}{
		{name: "array of strings", groups: []string{"artists", "leads"}, want: []string{"artists", "leads"}},
		{name: "bare string", groups: "artists", want: []string{"artists"}},
		{name: "empty array", groups: []string{}, want: nil},
		{name: "claim absent", groups: nil, want: nil},
		{name: "non-string members are skipped", groups: []any{"artists", 42}, want: []string{"artists"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idp := newFakeIDP(t)
			idp.groups = tt.groups
			p := New(testConfig(idp.URL()))

			id, err := p.Exchange(t.Context(), "code-1", "verifier-1", "nonce-1")
			if err != nil {
				t.Fatalf("Exchange: %v", err)
			}
			if !slices.Equal(id.Groups, tt.want) {
				t.Fatalf("Groups = %#v, want %#v", id.Groups, tt.want)
			}
		})
	}
}

// TestProvider_Exchange_SendsPKCEVerifier proves the verifier reaches the token
// endpoint. Without it, an intercepted authorization code would be redeemable
// by anyone.
func TestProvider_Exchange_SendsPKCEVerifier(t *testing.T) {
	idp := newFakeIDP(t)
	p := New(testConfig(idp.URL()))

	if _, err := p.Exchange(t.Context(), "code-1", "verifier-1", "nonce-1"); err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if got := idp.lastTokenForm.Get("code_verifier"); got != "verifier-1" {
		t.Fatalf("code_verifier = %q, want verifier-1", got)
	}
	if got := idp.lastTokenForm.Get("code"); got != "code-1" {
		t.Fatalf("code = %q, want code-1", got)
	}
}

func TestProvider_AuthCodeURL_ForceReauth(t *testing.T) {
	idp := newFakeIDP(t)
	p := New(testConfig(idp.URL()))

	for _, force := range []bool{false, true} {
		raw, err := p.AuthCodeURL("state-1", "nonce-1", "challenge-1", force)
		if err != nil {
			t.Fatalf("AuthCodeURL: %v", err)
		}
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		q := u.Query()
		if got := q.Get("prompt"); (got == "login") != force {
			t.Fatalf("forceReauth=%v gave prompt=%q", force, got)
		}
		if q.Get("code_challenge") != "challenge-1" {
			t.Fatalf("code_challenge = %q, want challenge-1", q.Get("code_challenge"))
		}
		if q.Get("code_challenge_method") != "S256" {
			t.Fatalf("code_challenge_method = %q, want S256", q.Get("code_challenge_method"))
		}
		if q.Get("nonce") != "nonce-1" || q.Get("state") != "state-1" {
			t.Fatalf("state/nonce not carried: %v", q)
		}
		if q.Get("redirect_uri") != testConfig(idp.URL()).RedirectURL {
			t.Fatalf("redirect_uri = %q", q.Get("redirect_uri"))
		}
		// "openid" must be present or the provider returns no ID token.
		if !slices.Contains(strings.Fields(q.Get("scope")), "openid") {
			t.Fatalf("scope = %q, want it to include openid", q.Get("scope"))
		}
	}
}

func TestProvider_DiscoveryIsLazyAndRetried(t *testing.T) {
	idp := newFakeIDP(t)
	idp.discoveryFails = true
	p := New(testConfig(idp.URL()))
	// Zero the herd-collapsing backoff: this test is about recoverability, and
	// the real window would hand back the recorded error instead of retrying.
	asProvider(t, p).failureBackoff = 0

	// New must not have dialed: construction happens at boot, and an
	// unreachable provider must not stop the server from starting.
	if _, err := p.AuthCodeURL("s", "n", "c", false); err == nil {
		t.Fatal("want error while discovery is failing")
	}
	idp.discoveryFails = false
	if _, err := p.AuthCodeURL("s", "n", "c", false); err != nil {
		t.Fatalf("discovery must be retried after a failure, got %v", err)
	}
}

// TestProvider_DiscoveryFailureBackoff proves the failure is remembered only
// briefly: within the window a caller gets the recorded error without a second
// network attempt, and once the window lapses discovery is retried.
func TestProvider_DiscoveryFailureBackoff(t *testing.T) {
	idp := newFakeIDP(t)
	idp.discoveryFails = true
	p := New(testConfig(idp.URL()))
	asProvider(t, p).failureBackoff = time.Hour

	if _, err := p.AuthCodeURL("s", "n", "c", false); !errors.Is(err, ErrDiscovery) {
		t.Fatalf("first attempt: want ErrDiscovery, got %v", err)
	}
	before := idp.discoveryHits()

	idp.discoveryFails = false
	if _, err := p.AuthCodeURL("s", "n", "c", false); !errors.Is(err, ErrDiscovery) {
		t.Fatalf("within the backoff window: want the recorded ErrDiscovery, got %v", err)
	}
	if got := idp.discoveryHits(); got != before {
		t.Fatalf("backoff window made %d extra discovery requests, want 0", got-before)
	}

	// Lapse the window; the very next login must retry for real.
	asProvider(t, p).failureBackoff = 0
	if _, err := p.AuthCodeURL("s", "n", "c", false); err != nil {
		t.Fatalf("after the window: discovery must be retried, got %v", err)
	}
}

// TestProvider_ConcurrentDiscovery pins both halves of the resolve contract: a
// herd of first-use callers fetches discovery exactly once, and they do not
// serialize behind one another. The earlier implementation held the mutex
// across the network call, so N callers cost N × the provider's latency — and
// against a dead provider, N × discoveryTimeout, each pinning a handler
// goroutine.
func TestProvider_ConcurrentDiscovery(t *testing.T) {
	const (
		goroutines = 50
		latency    = 200 * time.Millisecond
	)
	idp := newFakeIDP(t)
	idp.discoveryDelay = latency
	p := New(testConfig(idp.URL()))

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for range goroutines {
		wg.Go(func() {
			<-start
			_, err := p.AuthCodeURL("s", "n", "c", false)
			errs <- err
		})
	}

	began := time.Now()
	close(start)
	wg.Wait()
	elapsed := time.Since(began)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("AuthCodeURL: %v", err)
		}
	}

	if got := idp.discoveryHits(); got != 1 {
		t.Fatalf("discovery fetched %d times, want exactly 1", got)
	}
	// Serial execution would need goroutines × latency; allow generous slack
	// for scheduling while still failing on serialization.
	if limit := 4 * latency; elapsed > limit {
		t.Fatalf("concurrent first use took %v, want under %v (callers are serializing)", elapsed, limit)
	}
}

// TestProvider_ConcurrentDiscoveryFailure is the dead-provider half: a herd
// must cost one timed-out attempt in total, not one per caller.
func TestProvider_ConcurrentDiscoveryFailure(t *testing.T) {
	const (
		goroutines = 5
		latency    = 200 * time.Millisecond
	)
	idp := newFakeIDP(t)
	idp.discoveryDelay = latency
	idp.discoveryFails = true
	p := New(testConfig(idp.URL()))

	start := make(chan struct{})
	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			<-start
			if _, err := p.AuthCodeURL("s", "n", "c", false); !errors.Is(err, ErrDiscovery) {
				t.Errorf("want ErrDiscovery, got %v", err)
			}
		})
	}

	began := time.Now()
	close(start)
	wg.Wait()
	elapsed := time.Since(began)

	if got := idp.discoveryHits(); got != 1 {
		t.Fatalf("failing discovery fetched %d times, want exactly 1", got)
	}
	if limit := 4 * latency; elapsed > limit {
		t.Fatalf("concurrent failing logins took %v, want under %v", elapsed, limit)
	}
}

// TestNew_DoesNoNetworkIO points New at a dead address: if it dialed, this
// would hang or fail rather than return promptly.
func TestNew_DoesNoNetworkIO(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:1/unreachable")
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = New(cfg)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("New blocked; it must perform no network I/O")
	}
}

func TestProvider_EndSessionURL(t *testing.T) {
	tests := []struct {
		name         string
		mutate       func(*fakeIDP)
		redirect     string
		wantOK       bool
		wantRedirect string
	}{
		{name: "advertised", mutate: func(*fakeIDP) {}, redirect: "https://sqi.example.com/bye", wantOK: true, wantRedirect: "https://sqi.example.com/bye"},
		{
			name: "falls back to the configured post-logout redirect", mutate: func(*fakeIDP) {},
			redirect: "", wantOK: true, wantRedirect: "https://sqi.example.com/login",
		},
		{name: "not advertised", mutate: func(f *fakeIDP) { f.noEndSessionSupport = true }, wantOK: false},
		{name: "discovery unreachable", mutate: func(f *fakeIDP) { f.discoveryFails = true }, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idp := newFakeIDP(t)
			tt.mutate(idp)
			p := New(testConfig(idp.URL()))

			raw, ok := p.EndSessionURL(tt.redirect)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (url %q)", ok, tt.wantOK, raw)
			}
			if !tt.wantOK {
				if raw != "" {
					t.Fatalf("url = %q, want empty when unsupported", raw)
				}
				return
			}
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			q := u.Query()
			if q.Get("client_id") != testClientID {
				t.Fatalf("client_id = %q", q.Get("client_id"))
			}
			if q.Get("post_logout_redirect_uri") != tt.wantRedirect {
				t.Fatalf("post_logout_redirect_uri = %q, want %q", q.Get("post_logout_redirect_uri"), tt.wantRedirect)
			}
			// sqi stores no ID tokens, so it can never emit this hint —
			// storing one would be the first plaintext bearer secret in a
			// schema that otherwise holds only hashes.
			if q.Has("id_token_hint") {
				t.Fatal("end-session URL must never carry an id_token_hint")
			}
		})
	}
}

func TestConfig_MapRole_DelegatesToRolemap(t *testing.T) {
	cfg := Config{
		RoleMap: []rolemap.Mapping{
			{Group: "farm-admins", Role: "admin"},
			{Group: "artists", Role: "operator"},
		},
		DefaultRole: "viewer",
	}
	tests := []struct {
		name     string
		groups   []string
		wantRole string
		wantOK   bool
	}{
		{name: "first match wins over later ones", groups: []string{"artists", "farm-admins"}, wantRole: "admin", wantOK: true},
		{name: "case-insensitive", groups: []string{"ARTISTS"}, wantRole: "operator", wantOK: true},
		{name: "no match falls back to the default", groups: []string{"nobody"}, wantRole: "viewer", wantOK: true},
		{name: "no groups falls back to the default", groups: nil, wantRole: "viewer", wantOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := cfg.MapRole(tt.groups)
			if got != tt.wantRole || ok != tt.wantOK {
				t.Fatalf("MapRole(%v) = %q,%v; want %q,%v", tt.groups, got, ok, tt.wantRole, tt.wantOK)
			}
		})
	}

	// An empty default role is how a deployment requires group membership to
	// sign in at all: no match must reject the login, not admit a viewer.
	strict := cfg
	strict.DefaultRole = ""
	if role, ok := strict.MapRole([]string{"nobody"}); ok {
		t.Fatalf("MapRole with no default returned %q, want rejection", role)
	}
}
