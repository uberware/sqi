// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

// oidc_test.go — the real-provider regression guard for SSO (Phase 3, C2).
//
// # Why this file exists
//
// The unit tests drive a fake provider (internal/auth/oidc/fakeidp_test.go).
// It is stronger than C1's fake conn in one respect — it signs real tokens, so
// a validation mistake surfaces — but it shares the fake's fundamental blind
// spot: it returns whatever the test tells it to, so it cannot show what a real
// provider OMITS.
//
// The concrete trap: Keycloak does not put group memberships in a token unless
// a protocol mapper is configured for them. Without one, every login succeeds
// carrying zero groups, so every user silently lands on default_role — a
// privilege downgrade with no error anywhere. That is structurally the same bug
// as LDAP's operational-attribute trap, and no fake would think to reproduce it.
//
// It also settles two vendor claims the logout design rests on: whether
// end_session_endpoint is honored with client_id and no id_token_hint, and
// whether prompt=login actually forces re-authentication.
//
// # What it runs against
//
// A pinned, throwaway Keycloak container, seeded via its admin API with a
// realm, groups, users, and the groups protocol mapper. The image tag is
// pinned rather than floating on :latest because the login form is scraped —
// a vendor UI refresh must break loudly here, not silently elsewhere.
//
// Set SQI_TEST_OIDC_ISSUER to point at a realm you already have (one that
// already holds the fixture below) and no container is started; the tests that
// need the admin API to mutate the fixture skip instead. Skips cleanly when
// Docker is unavailable, so it never blocks a developer who has not installed
// it.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/auth/oidc"
	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/server"
)

// ── Provider fixture ──────────────────────────────────────────────────────────

const (
	// keycloakImage is pinned, not floating on :latest, for a reason stronger
	// than the usual reproducibility argument: the browser leg below SCRAPES
	// Keycloak's login form out of its HTML. A vendor UI refresh changes that
	// markup, and pinning means it breaks here — loudly, in the one test whose
	// job is to notice — instead of somewhere downstream at upgrade time.
	keycloakImage = "quay.io/keycloak/keycloak:26.0.7"

	kcAdminUser = "admin"
	kcAdminPass = "admin"

	// kcRealm is the realm name an EXTERNALLY supplied provider is expected to
	// hold the fixture in (SQI_TEST_OIDC_ISSUER). The managed container gets a
	// fresh realm per test instead — see startKeycloak.
	kcRealm = "sqi"

	kcClientID     = "sqi-server"
	kcClientSecret = "test-client-secret"

	kcGroupAdmins  = "farm-admins"
	kcGroupArtists = "artists"

	// Container start can be slow on a cold image pull or a busy CI runner.
	// Keycloak builds its augmented distribution on first boot, so this is
	// considerably longer than the LDAP fixture needs.
	kcReadyTimeout = 180 * time.Second

	// Attempts allowed for the admin token grant, with a linear backoff between
	// them. See adminToken for why one attempt is not enough.
	kcAdminTokenAttempts = 4
)

// keycloak is a seeded realm on a running identity provider under test.
type keycloak struct {
	// BaseURL is the provider's root, e.g. http://127.0.0.1:34567.
	BaseURL string
	// realm is the realm this test's fixture lives in. It is per-test, not a
	// constant — see startKeycloak.
	realm string
	// container is the docker container name, empty when the provider was
	// supplied externally via SQI_TEST_OIDC_ISSUER.
	container string
	// client is a plain HTTP client for the admin API. It deliberately does NOT
	// share the browser's cookie jar: admin calls are bearer-authenticated and
	// must not be able to disturb, or be disturbed by, a login under test.
	client *http.Client
}

// Issuer is the realm's OIDC issuer, which is what sqi is configured with.
func (k *keycloak) Issuer() string {
	return k.BaseURL + "/realms/" + k.realm
}

var (
	sharedKCOnce sync.Once
	// sharedKC is the one container every test in this file shares; nil when
	// the boot failed, with the reason in sharedKCErr.
	sharedKC    *keycloak
	sharedKCErr string
	// kcRealmSeq names each test's realm apart from every other test's.
	kcRealmSeq atomic.Int64
)

// startKeycloak returns a freshly seeded realm to test against.
//
// SQI_TEST_OIDC_ISSUER takes precedence: point it at a realm that already holds
// the fixture below and no container is started. Otherwise the package's single
// shared Keycloak is used, and this test gets a brand new realm on it.
//
// One container, many realms — rather than a container per test — because
// Keycloak takes about thirteen seconds to boot and about a tenth of a second
// to create a realm, and a realm is a full isolation boundary for everything
// these tests touch: users, groups, clients, sessions, and the "sub" values
// they hang identity off. Even TestOIDC_RenameAtProviderKeepsAccount, which
// mutates a user, is contained by it exactly as well as a private container
// would contain it.
//
// The test skips — it does not fail — when Docker is unavailable, so a
// developer without Docker is never blocked.
func startKeycloak(t *testing.T, redirectURI, postLogoutURI string) *keycloak {
	t.Helper()

	if issuer := os.Getenv("SQI_TEST_OIDC_ISSUER"); issuer != "" {
		t.Logf("using externally provided issuer at %s (SQI_TEST_OIDC_ISSUER)", issuer)
		base := strings.TrimSuffix(issuer, "/realms/"+kcRealm)
		return &keycloak{
			BaseURL: base,
			realm:   kcRealm,
			client:  &http.Client{Timeout: 30 * time.Second},
		}
	}

	requireDocker(t, "OIDC", "set SQI_TEST_OIDC_ISSUER to an existing realm")

	// The probe above is per-test, so a skip reaches every test; the boot below
	// happens once and its failure has to be replayed to the tests that did not
	// run it.
	sharedKCOnce.Do(func() {
		k, err := bootKeycloak(freePort(t))
		if err != nil {
			sharedKCErr = err.Error()
			return
		}
		sharedKC = k
		// Not t.Cleanup: the container outlives the test that happened to boot
		// it. TestMain is the only hook that runs after the last one.
		packageCleanups = append(packageCleanups, func() { removeContainer(k.container) })
	})
	if sharedKC == nil {
		t.Skipf("skipping OIDC integration test: cannot start %s: %s", keycloakImage, sharedKCErr)
	}

	k := &keycloak{
		BaseURL:   sharedKC.BaseURL,
		realm:     fmt.Sprintf("sqi-%d", kcRealmSeq.Add(1)),
		container: sharedKC.container,
		client:    sharedKC.client,
	}
	k.seed(t, redirectURI, postLogoutURI)
	return k
}

// bootKeycloak starts the container and waits for it to answer. It returns an
// error rather than taking a *testing.T because it runs under a sync.Once, on
// whichever test got there first — reporting through that test's T would make
// the outcome invisible to the others.
func bootKeycloak(port int) (*keycloak, error) {
	name := fmt.Sprintf("sqi-oidc-it-%d", port)

	// --rm so an aborted run (SIGINT, panic) does not leak a container; the
	// explicit remove in TestMain then becomes a no-op rather than the only
	// thing standing between this test and a pile of orphans.
	//
	// start-dev rather than start: it skips the production hostname/HTTPS
	// checks that would otherwise refuse to serve on plain HTTP, and leaves the
	// issuer derived from the request's Host header, which is what lets the
	// fixture work on whatever ephemeral port it was given.
	runCtx, runCancel := context.WithTimeout(context.Background(), kcReadyTimeout)
	defer runCancel()
	out, err := exec.CommandContext(
		runCtx, "docker", "run", "-d", "--rm",
		"--name", name,
		"-p", fmt.Sprintf("127.0.0.1:%d:8080", port),
		"-e", "KC_BOOTSTRAP_ADMIN_USERNAME="+kcAdminUser,
		"-e", "KC_BOOTSTRAP_ADMIN_PASSWORD="+kcAdminPass,
		keycloakImage, "start-dev",
	).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%w\n%s", err, out)
	}

	k := &keycloak{
		BaseURL:   fmt.Sprintf("http://127.0.0.1:%d", port),
		container: name,
		client:    &http.Client{Timeout: 30 * time.Second},
	}
	if err := k.waitReady(); err != nil {
		removeContainer(name)
		return nil, err
	}
	return k, nil
}

// waitReady blocks until the provider answers a discovery request for the realm
// it ships with. Polling beats a fixed sleep in both directions: a sleep long
// enough for a cold CI runner is wasted on every local run, and one tuned for
// local is flaky on CI.
func (k *keycloak) waitReady() error {
	deadline := time.Now().Add(kcReadyTimeout)
	var last string
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			k.BaseURL+"/realms/master/.well-known/openid-configuration", nil)
		if err != nil {
			cancel()
			return fmt.Errorf("build discovery probe: %w", err)
		}
		resp, err := k.client.Do(req)
		if err == nil {
			code := resp.StatusCode
			_ = resp.Body.Close()
			cancel()
			if code == http.StatusOK {
				return nil
			}
			last = fmt.Sprintf("discovery answered HTTP %d", code)
		} else {
			cancel()
			last = "discovery not answering yet: " + err.Error()
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("keycloak did not become ready within %s: %s", kcReadyTimeout, last)
}

// ── Admin API ─────────────────────────────────────────────────────────────────

// adminToken fetches an admin access token from the master realm, retrying a
// few times.
//
// waitReady only proves that master-realm *discovery* answers, and the admin
// endpoints can still be warming up behind it — on a loaded CI runner the first
// token request can come back 401 or 503. A single attempt turns that into a
// t.Fatalf that looks like a credential bug.
func (k *keycloak) adminToken(t *testing.T) string {
	t.Helper()
	var last string
	for attempt := range kcAdminTokenAttempts {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
		token, err := k.tryAdminToken()
		if err == nil {
			return token
		}
		last = err.Error()
	}
	t.Fatalf("admin token failed after %d attempts: %s", kcAdminTokenAttempts, last)
	return ""
}

// tryAdminToken is one attempt at the admin token grant.
func (k *keycloak) tryAdminToken() (string, error) {
	form := url.Values{
		"grant_type": {"password"},
		"client_id":  {"admin-cli"},
		"username":   {kcAdminUser},
		"password":   {kcAdminPass},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		k.BaseURL+"/realms/master/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build admin token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := k.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("admin token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read admin token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("admin token: HTTP %d\n%s", resp.StatusCode, body)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode admin token: %w\n%s", err, body)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("admin token response carried no access_token:\n%s", body)
	}
	return out.AccessToken, nil
}

// admin performs an authenticated admin-API call and asserts the status is one
// of wantStatus. It returns the response body, which is empty for the 204s most
// mutations answer with.
func (k *keycloak) admin(t *testing.T, method, path string, payload any, wantStatus ...int) []byte {
	t.Helper()
	if k.container == "" {
		t.Skip("skipping: fixture mutation requires the managed container (unset SQI_TEST_OIDC_ISSUER)")
	}

	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal admin payload: %v", err)
		}
		body = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, k.BaseURL+"/admin/realms"+path, body)
	if err != nil {
		t.Fatalf("build admin request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+k.adminToken(t))
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := k.client.Do(req)
	if err != nil {
		t.Fatalf("admin %s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read admin response: %v", err)
	}
	if !slices.Contains(wantStatus, resp.StatusCode) {
		t.Fatalf("admin %s %s: HTTP %d, want one of %v\n%s", method, path, resp.StatusCode, wantStatus, out)
	}
	return out
}

// seed builds the fixture realm.
//
// alice → farm-admins (maps to admin)
// bob   → artists     (maps to user)
// carol → no group    (falls through to default_role)
//
// The groups protocol mapper on the client is the point of the whole file.
// Keycloak emits NO group claim without one: the login still succeeds, the
// token still validates, and every user silently lands on default_role. Delete
// the mapper below and TestOIDC_AuthCodeRoundTripMapsGroups must go red — if it
// does not, that test is not testing what it claims to.
func (k *keycloak) seed(t *testing.T, redirectURI, postLogoutURI string) {
	t.Helper()

	k.admin(t, http.MethodPost, "", map[string]any{
		"realm":   k.realm,
		"enabled": true,
		// Keycloak treats username as read-only unless a realm opts into
		// renames, rejecting the change with error-user-attribute-read-only.
		// TestOIDC_RenameAtProviderKeepsAccount needs a rename to be possible at
		// all, and a realm that permits them is exactly the deployment whose
		// users can outgrow their login name — the case that test is about.
		"editUsernameAllowed": true,
	}, http.StatusCreated)

	for _, g := range []string{kcGroupAdmins, kcGroupArtists} {
		k.admin(t, http.MethodPost, "/"+k.realm+"/groups", map[string]any{"name": g}, http.StatusCreated)
	}

	k.admin(t, http.MethodPost, "/"+k.realm+"/clients", map[string]any{
		"clientId":                  kcClientID,
		"enabled":                   true,
		"protocol":                  "openid-connect",
		"publicClient":              false,
		"secret":                    kcClientSecret,
		"standardFlowEnabled":       true,
		"directAccessGrantsEnabled": false,
		"redirectUris":              []string{redirectURI},
		"webOrigins":                []string{"+"},
		// Keycloak refuses a post_logout_redirect_uri it has not been told
		// about, so registering it is part of configuring RP-initiated logout —
		// not an incidental fixture detail.
		"attributes": map[string]string{"post.logout.redirect.uris": postLogoutURI},
		"protocolMappers": []map[string]any{{
			"name":           "groups",
			"protocol":       "openid-connect",
			"protocolMapper": "oidc-group-membership-mapper",
			"config": map[string]string{
				// full.path=false emits bare names ("farm-admins") rather than
				// paths ("/farm-admins"). Either is fine as long as the role_map
				// agrees; the two disagreeing is the other way a deployment ends
				// up matching nothing and defaulting everyone.
				"full.path":            "false",
				"id.token.claim":       "true",
				"access.token.claim":   "true",
				"userinfo.token.claim": "true",
				"claim.name":           "groups",
			},
		}},
	}, http.StatusCreated)

	k.createUser(t, "alice", "alicepass", "Alice Anderson", kcGroupAdmins)
	k.createUser(t, "bob", "bobpass", "Bob Brown", kcGroupArtists)
	k.createUser(t, "carol", "carolpass", "Carol Clark", "")
}

// createUser adds an enabled user with a permanent password, optionally in a
// group.
func (k *keycloak) createUser(t *testing.T, username, pass, displayName, group string) {
	t.Helper()
	first, last, _ := strings.Cut(displayName, " ")
	payload := map[string]any{
		"username":      username,
		"enabled":       true,
		"emailVerified": true,
		"firstName":     first,
		"lastName":      last,
		"email":         username + "@example.com",
		// temporary=false matters: a temporary password makes Keycloak
		// interpose an "update password" screen, and every login below would
		// hang on a form this test does not know how to fill in.
		"credentials": []map[string]any{{"type": "password", "value": pass, "temporary": false}},
	}
	if group != "" {
		payload["groups"] = []string{"/" + group}
	}
	k.admin(t, http.MethodPost, "/"+k.realm+"/users", payload, http.StatusCreated)
}

// userID looks up a user's Keycloak id, which is also the "sub" claim.
func (k *keycloak) userID(t *testing.T, username string) string {
	t.Helper()
	raw := k.admin(t, http.MethodGet,
		"/"+k.realm+"/users?exact=true&username="+url.QueryEscape(username), nil, http.StatusOK)
	var users []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &users); err != nil {
		t.Fatalf("decode user lookup: %v\n%s", err, raw)
	}
	if len(users) != 1 {
		t.Fatalf("user lookup for %q returned %d users, want 1\n%s", username, len(users), raw)
	}
	return users[0].ID
}

// ── Server fixture ────────────────────────────────────────────────────────────

// oidcFixture is a Keycloak and an sqi-server wired to each other.
type oidcFixture struct {
	kc      *keycloak
	ts      *testServer
	sqiAddr string
}

// baseOIDCConfig is the shared half of every test's configuration: the two
// group→role rules and a default role. Each test mutates what it is about.
func baseOIDCConfig(issuer, sqiAddr string) config.OIDCConfig {
	return config.OIDCConfig{
		Enabled:      true,
		Issuer:       issuer,
		ClientID:     kcClientID,
		ClientSecret: kcClientSecret,
		RedirectURL:  "http://" + sqiAddr + "/api/v1/auth/oidc/callback",
		Scopes:       []string{"openid", "profile", "email"},
		// Keycloak's login name lives in preferred_username; "name" is the
		// assembled human-facing one. Both are the shipped defaults.
		UsernameClaim:    "preferred_username",
		DisplayNameClaim: "name",
		GroupsClaim:      "groups",
		RoleSource:       oidc.RoleSourceDirectory,
		DefaultRole:      "read-only",
		RoleMap: []config.RoleMappingConfig{
			{Group: kcGroupAdmins, Role: "admin"},
			{Group: kcGroupArtists, Role: "user"},
		},
		ReauthMode: oidc.ReauthAfterLogout,
		LogoutMode: oidc.LogoutLocal,
		// Set unconditionally even though only logout_mode=provider reads it,
		// so it cannot drift from the URI registered with the client at seeding
		// time — Keycloak rejects a post_logout_redirect_uri it does not know.
		PostLogoutRedirectURL: "http://" + sqiAddr + "/",
		ButtonLabel:           "Sign in with SSO",
	}
}

// newOIDCFixture starts a Keycloak and an sqi-server bound to each other.
//
// The sqi port is chosen FIRST, because the redirect URI and the post-logout
// redirect URI have to be registered with the client at seeding time. Keycloak
// rejects both if they were not registered, so "pick the port later" is not an
// option — and registering a wildcard instead would switch off the very
// validation a real deployment relies on.
func newOIDCFixture(t *testing.T, mutate func(*config.OIDCConfig)) *oidcFixture {
	t.Helper()

	sqiAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	kc := startKeycloak(
		t,
		"http://"+sqiAddr+"/api/v1/auth/oidc/callback",
		"http://"+sqiAddr+"/",
	)

	cfg := baseOIDCConfig(kc.Issuer(), sqiAddr)
	if mutate != nil {
		mutate(&cfg)
	}
	return &oidcFixture{kc: kc, ts: startOIDCServer(t, sqiAddr, cfg), sqiAddr: sqiAddr}
}

// sqiURL builds an absolute URL onto the server under test.
func (f *oidcFixture) sqiURL(path string) string { return "http://" + f.sqiAddr + path }

// startOIDCServer boots a full sqi-server with auth and the given OIDC config.
func startOIDCServer(t *testing.T, httpAddr string, oidcCfg config.OIDCConfig) *testServer {
	t.Helper()
	return startAuthServer(t, httpAddr, "sqi-oidc.db", func(c *server.Config) { c.AuthOIDC = oidcCfg })
}

// ── Browser ───────────────────────────────────────────────────────────────────

// browserMaxHops bounds a redirect chain. A full login is about six hops; well
// under this, and a runaway loop fails with the chain printed rather than
// hanging until the test timeout.
const browserMaxHops = 20

// browser is a headless stand-in for the user agent the auth-code flow needs.
// It holds a cookie jar (both sqi's and the provider's cookies matter) and
// follows redirects by hand so a test can inspect, or interrupt, the chain.
type browser struct {
	t     *testing.T
	c     *http.Client
	chain []string
}

func newBrowser(t *testing.T) *browser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("new cookie jar: %v", err)
	}
	// A stock cookiejar is enough even though this fixture speaks plain HTTP and
	// Keycloak marks AUTH_SESSION_ID / KEYCLOAK_IDENTITY / KEYCLOAK_SESSION /
	// KC_RESTART Secure unconditionally (they are SameSite=None, which obliges
	// it; sslRequired=none does not turn it off). Go's jar treats a loopback
	// host as a secure origin and sends Secure cookies to it anyway — see
	// entry.secureMatch in $GOROOT/src/net/http/cookiejar/jar.go — and the
	// container is published on 127.0.0.1. If SQI_TEST_OIDC_ISSUER ever pointed
	// at a NON-loopback provider over plain HTTP, that exemption would not
	// apply, the provider's session would be invisible to this client, and the
	// control step in TestOIDC_PromptLoginForcesReauth is what would catch it.
	return &browser{t: t, c: &http.Client{
		Jar: jar,
		// Redirects are followed manually below, so every hop is recorded and
		// a test can stop the walk before a particular request is made.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       60 * time.Second,
	}}
}

// page is where a redirect chain came to rest.
type page struct {
	URL    *url.URL
	Status int
	Body   string
	// Chain is every URL visited on the way here, for failure messages — a
	// login that ends somewhere unexpected is almost unreadable without it.
	Chain []string
}

// hasLoginForm reports whether Keycloak is asking for credentials.
func (p page) hasLoginForm() bool { return strings.Contains(p.Body, `id="kc-form-login"`) }

// visit issues a request and follows the whole redirect chain.
func (b *browser) visit(method, raw string, form url.Values) page {
	b.t.Helper()
	return b.visitUntil(method, raw, form, nil)
}

// visitUntil is visit, stopping before it requests the first URL that stop
// accepts. That hop is returned unrequested, which is how a test gets its hands
// on the callback URL before the server has seen it.
func (b *browser) visitUntil(method, raw string, form url.Values, stop func(*url.URL) bool) page {
	b.t.Helper()
	b.chain = nil

	for range browserMaxHops {
		u, err := url.Parse(raw)
		if err != nil {
			b.t.Fatalf("parse %q: %v (chain: %v)", raw, err, b.chain)
		}
		if stop != nil && stop(u) {
			return page{URL: u, Chain: slices.Clone(b.chain)}
		}
		b.chain = append(b.chain, raw)

		status, loc, body := b.request(method, raw, form)
		if loc != "" && status >= http.StatusMovedPermanently && status <= http.StatusPermanentRedirect {
			next, perr := u.Parse(loc)
			if perr != nil {
				b.t.Fatalf("parse Location %q: %v (chain: %v)", loc, perr, b.chain)
			}
			// Every redirect in this flow is a GET, including the 302 that
			// answers the credential POST.
			raw, method, form = next.String(), http.MethodGet, nil
			continue
		}
		return page{URL: u, Status: status, Body: body, Chain: slices.Clone(b.chain)}
	}
	b.t.Fatalf("redirect chain did not settle within %d hops: %v", browserMaxHops, b.chain)
	return page{}
}

// request performs a single HTTP request, returning its status, Location, and
// body.
func (b *browser) request(method, raw string, form url.Values) (int, string, string) {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, raw, body)
	if err != nil {
		b.t.Fatalf("build %s %s: %v", method, raw, err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatalf("%s %s: %v (chain: %v)", method, raw, err, b.chain)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		b.t.Fatalf("read %s %s: %v", method, raw, err)
	}
	return resp.StatusCode, resp.Header.Get("Location"), string(out)
}

// cookie returns a cookie the jar holds for the given URL, or "".
func (b *browser) cookie(rawURL, name string) string {
	b.t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		b.t.Fatalf("parse %q: %v", rawURL, err)
	}
	for _, c := range b.c.Jar.Cookies(u) {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// ── Form scraping ─────────────────────────────────────────────────────────────

// These are the coupling to Keycloak's markup that keycloakImage is pinned for.
var (
	kcLoginFormRe = regexp.MustCompile(`(?s)<form[^>]*id="kc-form-login"[^>]*?action="([^"]*)"[^>]*>(.*?)</form>`)
	// The logout-confirm form itself carries no id — in logout-confirm.ftl it is
	// a bare <form class="form-actions"> inside <div id="kc-logout-confirm">. So
	// anchor on the wrapper and take the first form within it, rather than the
	// first form on the page: anything Keycloak renders above the content area
	// (a locale selector, say) would otherwise be posted instead.
	kcLogoutConfirmRe = regexp.MustCompile(`(?s)<div[^>]*id="kc-logout-confirm".*?<form[^>]*action="([^"]*)"[^>]*>(.*?)</form>`)
	kcHiddenRe        = regexp.MustCompile(`(?s)<input[^>]*type="hidden"[^>]*>`)
	kcNameAttrRe      = regexp.MustCompile(`name="([^"]*)"`)
	kcValueAttrRe     = regexp.MustCompile(`value="([^"]*)"`)
)

// scrapeForm pulls a form's absolute action URL and its hidden inputs out of a
// page. Keycloak carries the flow's continuation entirely in those two things —
// session_code, execution and tab_id in the action, credentialId in a hidden
// input — so dropping either produces an opaque HTTP 400 rather than a login.
func scrapeForm(t *testing.T, p page, re *regexp.Regexp, what string) (string, url.Values) {
	t.Helper()
	m := re.FindStringSubmatch(p.Body)
	if m == nil {
		t.Fatalf("no %s on %s (HTTP %d).\nThis is usually one of: the provider did not ask for "+
			"credentials at all, or %s changed its markup — the pin on the image is what makes the "+
			"second show up here.\nchain: %v\nbody:\n%s",
			what, p.URL, p.Status, keycloakImage, p.Chain, truncate(p.Body, 2000))
	}
	action, err := p.URL.Parse(html.UnescapeString(m[1]))
	if err != nil {
		t.Fatalf("parse %s action %q: %v", what, m[1], err)
	}

	values := url.Values{}
	for _, input := range kcHiddenRe.FindAllString(m[2], -1) {
		name := kcNameAttrRe.FindStringSubmatch(input)
		if name == nil {
			continue
		}
		// A hidden input with no value attribute is legitimate (Keycloak's
		// credentialId is exactly that) and must still be submitted, empty.
		value := ""
		if v := kcValueAttrRe.FindStringSubmatch(input); v != nil {
			value = html.UnescapeString(v[1])
		}
		values.Set(html.UnescapeString(name[1]), value)
	}
	return action.String(), values
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n… (truncated)"
}

// ── Login helpers ─────────────────────────────────────────────────────────────

// beginLogin navigates to sqi's SSO entry point and follows wherever it leads —
// which is either the provider's credential form or, when the provider still
// considers this browser signed in, straight back through sqi's callback.
func (f *oidcFixture) beginLogin(b *browser) page {
	b.t.Helper()
	return b.visit(http.MethodGet, f.sqiURL("/api/v1/auth/oidc/login"), nil)
}

// submitCredentials fills in and posts the credential form on p.
func (*oidcFixture) submitCredentials(b *browser, p page, username, pass string) page {
	b.t.Helper()
	action, form := scrapeForm(b.t, p, kcLoginFormRe, "credential form")
	form.Set("username", username)
	form.Set("password", pass)
	return b.visit(http.MethodPost, action, form)
}

// signIn drives a complete interactive login and asserts it produced a session.
func (f *oidcFixture) signIn(b *browser, username, pass string) {
	b.t.Helper()
	p := f.beginLogin(b)
	if !p.hasLoginForm() {
		b.t.Fatalf("expected the provider to ask %q for credentials, landed on %s (HTTP %d)\nchain: %v",
			username, p.URL, p.Status, p.Chain)
	}
	done := f.submitCredentials(b, p, username, pass)
	if strings.Contains(done.URL.String(), "sso_error") {
		b.t.Fatalf("login for %q was refused by sqi's callback (%s). The reason is only in the "+
			"server log, by design — run with the server at debug level to see it.\nchain: %v",
			username, done.URL, done.Chain)
	}
	if b.cookie(f.sqiURL("/"), "sqi_session") == "" {
		b.t.Fatalf("login for %q set no session cookie; landed on %s (HTTP %d)\nchain: %v",
			username, done.URL, done.Status, done.Chain)
	}
}

// principal is the subset of GET /auth/me these tests assert on. Note that
// Subject is the sqi ACCOUNT id, not the provider's "sub" — the distinction
// matters in the rename test, which asserts a stable account across a changed
// username while the provider subject stays put.
type principal struct {
	Subject     string   `json:"subject"`
	Username    string   `json:"username"`
	DisplayName string   `json:"display_name"`
	Roles       []string `json:"roles"`
	Kind        string   `json:"kind"`
}

// role is the single role the account resolved to, or "" if it has none.
// sqi assigns exactly one role per account; the wire shape is a list because
// principals in general (API keys) may carry several.
func (p principal) role() string {
	if len(p.Roles) == 0 {
		return ""
	}
	return p.Roles[0]
}

// me reads the signed-in principal back from the server, which is where the
// role that actually took effect can be observed.
func (f *oidcFixture) me(b *browser) principal {
	b.t.Helper()
	status, _, body := b.request(http.MethodGet, f.sqiURL("/api/v1/auth/me"), nil)
	if status != http.StatusOK {
		b.t.Fatalf("GET /auth/me: HTTP %d\n%s", status, body)
	}
	var out principal
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		b.t.Fatalf("decode /auth/me: %v\n%s", err, body)
	}
	return out
}

// storedAccount is the subset of a GET /users/{id} row these tests assert on.
type storedAccount struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	Role       string `json:"role"`
	AuthSource string `json:"auth_source"`
}

// account reads a stored user row. /auth/me describes the authenticated
// principal and deliberately says nothing about which backend vouched for it,
// so auth_source can only be observed here. Requires an admin browser.
func (f *oidcFixture) account(b *browser, id string) storedAccount {
	b.t.Helper()
	var out storedAccount
	status, _, body := b.request(http.MethodGet, f.sqiURL("/api/v1/users/"+id), nil)
	if status != http.StatusOK {
		b.t.Fatalf("GET /users/%s: HTTP %d\n%s", id, status, body)
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		b.t.Fatalf("decode /users/%s: %v\n%s", id, err, body)
	}
	return out
}

// logout posts to sqi's logout route and returns the provider redirect URL it
// hands back, which is empty under logout_mode=local.
func (f *oidcFixture) logout(b *browser) string {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.sqiURL("/api/v1/auth/logout"), nil)
	if err != nil {
		b.t.Fatalf("build logout request: %v", err)
	}
	// middleware.CSRF guards cookie-authenticated mutations and rejects one
	// carrying no Origin at all, which is what a bare Go client sends.
	req.Header.Set("Origin", "http://"+f.sqiAddr)
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatalf("POST /auth/logout: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		b.t.Fatalf("read logout response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		b.t.Fatalf("POST /auth/logout: HTTP %d\n%s", resp.StatusCode, body)
	}
	var out struct {
		RedirectURL string `json:"redirect_url"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		b.t.Fatalf("decode logout response: %v\n%s", err, body)
	}
	return out.RedirectURL
}

// sqiLoginIsSilent reports whether a login started at sqi completes without a
// credential prompt. It answers a question about SQI's behavior — whether it
// asked the provider to re-prompt — not purely about the provider's session.
//
// Note the side effect: when the login is silent it runs to completion, minting
// a fresh sqi session. That is unavoidable — "would this be challenged?" cannot
// be asked without asking it — and harmless to callers that do not go on to
// assert on session state.
func (f *oidcFixture) sqiLoginIsSilent(b *browser) bool {
	b.t.Helper()
	return !f.beginLogin(b).hasLoginForm()
}

// providerSessionLive asks the PROVIDER directly whether this browser still has
// a session, with prompt=none — the standard "answer without interacting"
// probe, which returns either a code or error=login_required.
//
// It deliberately bypasses sqi's login route. Going through sqi cannot answer
// this question after a logout: sqi sets prompt=login under
// reauth_mode=after_logout, so the provider presents a credential form whether
// or not its own session survived, and a probe built on that would read every
// forced re-prompt as a terminated session. That confusion is exactly what made
// the end-session experiment below report the wrong result at first.
func (f *oidcFixture) providerSessionLive(b *browser) bool {
	b.t.Helper()
	q := url.Values{
		"client_id": {kcClientID},
		// Must be a registered URI even though the walk stops before reaching
		// it — the provider validates it before deciding anything else.
		"redirect_uri":  {"http://" + f.sqiAddr + "/api/v1/auth/oidc/callback"},
		"response_type": {"code"},
		"scope":         {"openid"},
		"state":         {"session-probe"},
		"prompt":        {"none"},
	}
	authURL := f.kc.Issuer() + "/protocol/openid-connect/auth?" + q.Encode()
	// Stop before sqi sees it: this probe must not mint or destroy an sqi
	// session, and an unsolicited code arriving at the callback would do both.
	p := b.visitUntil(http.MethodGet, authURL, nil, func(u *url.URL) bool { return u.Host == f.sqiAddr })
	if p.URL == nil || p.URL.Host != f.sqiAddr {
		b.t.Fatalf("prompt=none probe did not come back to the redirect URI; landed on %v (chain: %v)",
			p.URL, p.Chain)
	}
	q = p.URL.Query()
	switch {
	case q.Get("code") != "":
		return true
	case q.Get("error") == "login_required":
		return false
	default:
		b.t.Fatalf("prompt=none probe returned neither a code nor login_required: %s", p.URL)
		return false
	}
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestOIDC_AuthCodeRoundTripMapsGroups is the mapper guard: it asserts the user
// lands on the MAPPED role, not on default_role. Asserting only "login
// succeeded" would pass with zero groups and prove nothing — which is exactly
// what a Keycloak with no groups protocol mapper produces.
func TestOIDC_AuthCodeRoundTripMapsGroups(t *testing.T) {
	f := newOIDCFixture(t, nil)

	t.Run("group maps to admin", func(t *testing.T) {
		b := newBrowser(t)
		f.signIn(b, "alice", "alicepass")
		got := f.me(b)
		if got.role() == "read-only" {
			t.Fatal("alice landed on default_role. Her token carried no usable groups claim, " +
				"so the group→role mapping matched nothing and she was silently downgraded. " +
				"The usual cause is a provider with no groups protocol mapper, or one emitting " +
				"the claim under a different name than groups_claim. This is the silent-downgrade " +
				"regression this whole file exists to catch.")
		}
		if got.role() != "admin" {
			t.Errorf("role = %q, want admin (group→role mapping over a real provider)", got.role())
		}
		// alice is an admin, so this browser may read the stored row — the only
		// place the provisioning backend is visible.
		if src := f.account(b, got.Subject).AuthSource; src != "oidc" {
			t.Errorf("auth_source = %q, want oidc", src)
		}
		if got.Username != "alice" {
			t.Errorf("username = %q, want alice (from the preferred_username claim)", got.Username)
		}
		if got.DisplayName != "Alice Anderson" {
			t.Errorf("display_name = %q, want %q (from the name claim)", got.DisplayName, "Alice Anderson")
		}
	})

	t.Run("group maps to user", func(t *testing.T) {
		b := newBrowser(t)
		f.signIn(b, "bob", "bobpass")
		if got := f.me(b).role(); got != "user" {
			t.Errorf("role = %q, want user", got)
		}
	})

	t.Run("no group falls through to default_role", func(t *testing.T) {
		// Also what makes the two cases above meaningful: if the groups claim
		// were never arriving, alice and bob would land here too, and a test
		// that only checked carol would call that correct.
		b := newBrowser(t)
		f.signIn(b, "carol", "carolpass")
		if got := f.me(b).role(); got != "read-only" {
			t.Errorf("role = %q, want read-only (no group, so default_role)", got)
		}
	})

	t.Run("wrong password is refused", func(t *testing.T) {
		b := newBrowser(t)
		p := f.beginLogin(b)
		after := f.submitCredentials(b, p, "alice", "wrongpass")
		if !after.hasLoginForm() {
			t.Errorf("a wrong password did not return the credential form; landed on %s", after.URL)
		}
		if b.cookie(f.sqiURL("/"), "sqi_session") != "" {
			t.Error("a wrong password minted an sqi session")
		}
	})
}

// TestOIDC_RenameAtProviderKeepsAccount changes the user's username in Keycloak,
// leaving "sub" untouched, and asserts the second login lands on the same
// account ID.
//
// The rename is what makes the assertion meaningful: afterwards the username no
// longer matches the stored row, so ONLY subject matching can reach the original
// account. Match on the username instead and this test provisions a second row —
// one account per rename, each holding a slice of the person's jobs.
func TestOIDC_RenameAtProviderKeepsAccount(t *testing.T) {
	f := newOIDCFixture(t, nil)

	first := newBrowser(t)
	f.signIn(first, "alice", "alicepass")
	before := f.me(first)
	if before.Subject == "" {
		t.Fatal("first login carried no account id; the comparison below would be vacuous")
	}

	// Rename at the provider. Keycloak's "sub" is the user's immutable id, not
	// the username, so this is the same identity wearing a new name.
	sub := f.kc.userID(t, "alice")
	f.kc.admin(t, http.MethodPut, "/"+f.kc.realm+"/users/"+sub,
		map[string]any{"username": "alice.smith"}, http.StatusNoContent)
	if got := f.kc.userID(t, "alice.smith"); got != sub {
		t.Fatalf("the rename changed the subject (%s -> %s); the fixture no longer tests what it claims to", sub, got)
	}

	// A fresh browser, so the second login genuinely re-authenticates rather
	// than riding the first one's provider session.
	second := newBrowser(t)
	f.signIn(second, "alice.smith", "alicepass")
	after := f.me(second)

	if after.Subject != before.Subject {
		t.Fatalf("the rename created a NEW account (%s -> %s): the provider reports the same "+
			"person with the same \"sub\", so accounts are being matched on the username instead "+
			"of the subject. Every rename would fork the account, splitting a user's job "+
			"ownership across two rows.", before.Subject, after.Subject)
	}
	if after.role() != "admin" {
		t.Errorf("role after rename = %q, want admin (group membership is unchanged)", after.role())
	}
}

// TestOIDC_StateMismatchRejected replays the callback with a tampered state
// parameter and asserts no session cookie is set.
//
// The state cookie is the ONLY CSRF defense the callback has — it is a public
// GET that mints a session, and middleware.CSRF cannot guard it. A callback
// that accepted a mismatched state would let an attacker complete a login of
// their choosing in someone else's browser.
func TestOIDC_StateMismatchRejected(t *testing.T) {
	f := newOIDCFixture(t, nil)
	b := newBrowser(t)

	// Walk a real login right up to the callback, then stop — so the code and
	// state below are genuine, and the only thing wrong with the request is the
	// state value.
	isCallback := func(u *url.URL) bool {
		return u.Host == f.sqiAddr && u.Path == "/api/v1/auth/oidc/callback"
	}
	p := f.beginLogin(b)
	if !p.hasLoginForm() {
		t.Fatalf("expected a credential form, landed on %s", p.URL)
	}
	action, form := scrapeForm(t, p, kcLoginFormRe, "credential form")
	form.Set("username", "alice")
	form.Set("password", "alicepass")
	held := b.visitUntil(http.MethodPost, action, form, isCallback)

	if held.URL == nil || !isCallback(held.URL) {
		t.Fatalf("login never reached sqi's callback; got %v (chain: %v)", held.URL, held.Chain)
	}
	q := held.URL.Query()
	if q.Get("code") == "" {
		t.Fatalf("callback carried no authorization code: %s", held.URL)
	}
	if q.Get("state") == "" {
		t.Fatalf("callback carried no state parameter: %s", held.URL)
	}

	// Same code, same state cookie, one flipped state parameter.
	q.Set("state", q.Get("state")+"-tampered")
	tampered := *held.URL
	tampered.RawQuery = q.Encode()
	got := b.visit(http.MethodGet, tampered.String(), nil)

	if c := b.cookie(f.sqiURL("/"), "sqi_session"); c != "" {
		t.Fatalf("a callback whose state did not match the state cookie still minted a session " +
			"(cookie present). The state comparison is the callback's only CSRF defense.")
	}
	if !strings.Contains(got.URL.String(), "sso_error") {
		t.Errorf("expected a redirect to the generic SSO error marker, landed on %s (chain: %v)",
			got.URL, got.Chain)
	}
}

// TestOIDC_EndSessionAcceptsClientIDWithoutTokenHint tests the vendor claim the
// logout design rests on.
//
// sqi deliberately does not store ID tokens: doing so would put the first
// plaintext bearer secret into a schema that otherwise holds only hashes, and
// the realistic leak path is an accidental log line or a future session-listing
// endpoint. The accepted cost was that provider logout uses client_id and
// post_logout_redirect_uri with NO id_token_hint.
//
// # What this test actually found
//
// The belief being checked was "that works on Keycloak". Observed on
// keycloakImage, it is only half true, and the half that is false matters:
//
//   - Keycloak ACCEPTS the request. No error, no rejection — the client_id and
//     the registered post_logout_redirect_uri are honored.
//   - But it does NOT complete the logout. It answers HTTP 200 with an
//     interactive logout-CONFIRMATION page, and the provider session stays live
//     until a human clicks through it. Only after the confirmation is posted
//     does Keycloak end the session and redirect to post_logout_redirect_uri.
//
// So logout_mode=provider on Keycloak is a confirmation prompt, not a silent
// provider logout. The assertions below pin that observed behavior rather than
// the belief, so a future Keycloak that changes either half breaks here. Fixing
// it by storing the ID token would silently reverse the design decision above
// and is out of scope for this test — see the report accompanying it.
func TestOIDC_EndSessionAcceptsClientIDWithoutTokenHint(t *testing.T) {
	f := newOIDCFixture(t, func(c *config.OIDCConfig) { c.LogoutMode = oidc.LogoutProvider })
	b := newBrowser(t)
	f.signIn(b, "alice", "alicepass")

	redirect := f.logout(b)
	if redirect == "" {
		t.Fatal("logout_mode=provider produced no redirect_url: sqi degraded to a local logout, " +
			"either because discovery failed or because the provider advertised no " +
			"end_session_endpoint. Nothing below would be testing the provider.")
	}

	u, err := url.Parse(redirect)
	if err != nil {
		t.Fatalf("parse end-session URL %q: %v", redirect, err)
	}
	q := u.Query()
	if got := q.Get("id_token_hint"); got != "" {
		t.Fatalf("the end-session URL carried an id_token_hint (%q). sqi does not store ID tokens "+
			"by design; if that changed, this test and docs/auth.md both need revisiting.", got)
	}
	if got := q.Get("client_id"); got != kcClientID {
		t.Errorf("client_id = %q, want %q", got, kcClientID)
	}
	if q.Get("post_logout_redirect_uri") == "" {
		t.Error("the end-session URL carried no post_logout_redirect_uri")
	}

	// Claim under test, part one: the provider accepts these parameters.
	got := b.visit(http.MethodGet, redirect, nil)
	if got.Status != http.StatusOK {
		t.Fatalf("the provider refused an end-session request carrying client_id and no "+
			"id_token_hint: HTTP %d at %s\nchain: %v\nbody:\n%s",
			got.Status, got.URL, got.Chain, truncate(got.Body, 1500))
	}
	if strings.Contains(got.Body, "we are sorry") || strings.Contains(got.Body, "Invalid parameter") {
		t.Fatalf("the provider answered an end-session error page:\n%s", truncate(got.Body, 1500))
	}

	// Claim under test, part two — the half that turned out to be false. The
	// redirect alone does NOT end the session; Keycloak interposes a
	// confirmation page and keeps the session live behind it.
	if !strings.Contains(got.Body, "logout-confirm") {
		t.Fatalf("expected an interactive logout-confirmation page from %s.\n"+
			"If this provider now logs out without confirmation, that is GOOD news and the "+
			"limitation recorded in docs/auth.md is stale — but it must be re-verified and "+
			"rewritten deliberately, not by deleting this assertion.\nbody:\n%s",
			keycloakImage, truncate(got.Body, 1500))
	}
	if !f.providerSessionLive(b) {
		t.Error("the provider session ended without the confirmation being posted; the observed " +
			"behavior recorded above has changed and docs/auth.md should be re-checked")
	}

	// And the session does end once the confirmation is posted, landing on the
	// registered post-logout URI. Without this the test would show that the
	// logout is blocked but not that it is reachable at all.
	//
	// A fresh end-session request rather than reusing got: the confirmation
	// page carries a single-use session_code, and the probe above has already
	// spent the one it came with.
	fresh := b.visit(http.MethodGet, redirect, nil)
	action, form := scrapeForm(t, fresh, kcLogoutConfirmRe, "logout confirmation form")
	done := b.visit(http.MethodPost, action, form)
	if !strings.HasPrefix(done.URL.String(), "http://"+f.sqiAddr) {
		t.Errorf("after confirming logout, expected to land on the registered post-logout URI "+
			"(http://%s/), landed on %s\nchain: %v", f.sqiAddr, done.URL, done.Chain)
	}
	if f.providerSessionLive(b) {
		t.Error("the provider session survived a CONFIRMED logout; RP-initiated logout is not " +
			"working at all, which is worse than the confirmation-prompt limitation above")
	}
}

// TestOIDC_PromptLoginForcesReauth logs in, logs out, then starts a second login
// and asserts Keycloak presents the credential form again rather than silently
// redirecting back with a code.
//
// The control in the middle is what makes it a test rather than a tautology: if
// this browser's provider session were not live to begin with, every login would
// show a credential form and the final assertion would pass having proved
// nothing.
func TestOIDC_PromptLoginForcesReauth(t *testing.T) {
	f := newOIDCFixture(t, func(c *config.OIDCConfig) { c.ReauthMode = oidc.ReauthAfterLogout })
	b := newBrowser(t)

	// 1. A first login must challenge: nothing is signed in yet.
	f.signIn(b, "alice", "alicepass")

	// 2. CONTROL: a second login with no logout in between must be silent. This
	//    proves the provider session exists and that sqi is not sending
	//    prompt=login unconditionally — without it, step 3 proves nothing.
	if !f.sqiLoginIsSilent(b) {
		t.Fatal("a re-login with no logout in between was challenged for credentials. The " +
			"provider session is not live in this browser, so the assertion below cannot " +
			"distinguish prompt=login working from it never having been needed. Most likely " +
			"the browser is not retaining Keycloak's session cookies: it marks them Secure, " +
			"and the stock cookiejar only sends those over plain HTTP because the host is " +
			"loopback. Check that the provider is reachable on 127.0.0.1 (a non-loopback " +
			"SQI_TEST_OIDC_ISSUER over http:// would lose them). Failing that, sqi is sending " +
			"prompt=login unconditionally instead of only after a logout.")
	}

	// 3. After an explicit logout, reauth_mode=after_logout must force the
	//    provider to ask again — even though its own session is still live.
	f.logout(b)
	p := f.beginLogin(b)
	if !p.hasLoginForm() {
		t.Fatalf("after an explicit logout, the next SSO login was still silent (landed on %s). "+
			"prompt=login is not reaching the provider, or the re-auth marker cookie is not "+
			"surviving the logout — either way, the next person at a shared workstation is "+
			"signed in as the last one.\nchain: %v", p.URL, p.Chain)
	}

	// And the forced re-authentication genuinely completes — a prompt that
	// cannot be satisfied would be its own kind of broken.
	//
	// Re-authenticating as the SAME user, not a different one: Keycloak treats
	// prompt=login against a live session as "prove you are still alice", and
	// answering it with bob's credentials simply re-renders the form. Switching
	// users is a logout-then-login, which is a different flow from this one.
	f.submitCredentials(b, p, "alice", "alicepass")
	if got := f.me(b).Username; got != "alice" {
		t.Errorf("after forced re-authentication, signed in as %q, want alice", got)
	}
}
