// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Tests for the directory-backed login path: routing by AuthSource,
// just-in-time provisioning, role syncing, collision handling, and the
// invariant that every failure returns one indistinguishable 401.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/auth/ldap"
	"github.com/uberware/sqi/internal/auth/password"
	"github.com/uberware/sqi/internal/auth/session"
	"github.com/uberware/sqi/internal/health"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/product"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// fakeVerifier is a scripted ldap.Verifier. Every login test drives this, so
// none of them needs a directory. The mutex is not decoration: Verify runs on
// the httptest server's goroutine while the assertions run on the test's.
type fakeVerifier struct {
	mu       sync.Mutex
	identity ldap.Identity
	err      error
	calls    int
	lastUser string
	lastPass string
}

func (f *fakeVerifier) Verify(_ context.Context, username, pw string) (ldap.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastUser, f.lastPass = username, pw
	if f.err != nil {
		return ldap.Identity{}, f.err
	}
	return f.identity, nil
}

// callCount reports how many times the directory was consulted.
func (f *fakeVerifier) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func aliceIdentity() ldap.Identity {
	return ldap.Identity{
		DN:          "CN=Alice,DC=example,DC=com",
		Username:    "alice",
		DisplayName: "Alice Anderson",
		Groups:      []string{"CN=Admins,DC=example,DC=com"},
	}
}

func ldapCfg() ldap.Config {
	return ldap.Config{
		RoleSource:  ldap.RoleSourceDirectory,
		DefaultRole: "read-only",
		RoleMap:     []ldap.RoleMapping{{Group: "CN=Admins,DC=example,DC=com", Role: "admin"}},
	}
}

// authRouterLDAP mirrors authRouter with a directory verifier wired in.
// A nil verifier models auth.ldap.enabled=false.
func authRouterLDAP(st store.Store, v ldap.Verifier, lcfg ldap.Config) chi.Router {
	return NewRouter(
		Config{DisableRateLimit: true, AuthEnabled: true},
		Deps{
			Store:        st,
			Products:     product.NewCatalog(st),
			Auth:         session.New(st, "sqi_session", nil),
			SessionTTL:   time.Hour,
			CookieName:   "sqi_session",
			CookieSecure: "false",
			LDAPVerifier: v,
			LDAPConfig:   lcfg,
		},
		newTestLogger(), metrics.New(), health.NewRegistry(),
	)
}

// newLDAPServer starts an auth-enabled test server with LDAP wired to v,
// mirroring newAuthTestServer.
func newLDAPServer(t *testing.T, st store.Store, v ldap.Verifier, lcfg ldap.Config) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(authRouterLDAP(st, v, lcfg))
	t.Cleanup(srv.Close)
	return srv
}

// postLogin posts credentials to /auth/login, which is mounted outside the
// auth middleware and so needs no cookie.
func postLogin(t *testing.T, srv *httptest.Server, username, pw string) (int, string) {
	t.Helper()
	resp := doRequest(t, http.MethodPost, srv.URL+"/api/v1/auth/login",
		map[string]string{"username": username, "password": pw}, nil)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(b)
}

// seedLDAPUser inserts a directory-backed account directly, since
// seedAuthUser only ever creates local ones.
func seedLDAPUser(t *testing.T, st store.Store, u store.User) store.User {
	t.Helper()
	u.ID = uuid.NewString()
	u.AuthSource = store.AuthSourceLDAP
	if u.PasswordHash == "" {
		u.PasswordHash = ldapPlaceholderHash
	}
	out, err := st.CreateUser(t.Context(), u)
	if err != nil {
		t.Fatalf("seedLDAPUser: %v", err)
	}
	return out
}

// A local account must not touch the directory at all — otherwise every local
// login pays a network round trip and, worse, a wrong local password would
// trigger a bind that can lock the directory account.
func TestLogin_LocalUserNeverConsultsLDAP(t *testing.T) {
	st := fake.New()
	seedAuthUser(t, st, "bob", "localpass", "user")
	v := &fakeVerifier{identity: aliceIdentity()}
	srv := newLDAPServer(t, st, v, ldapCfg())

	if code, body := postLogin(t, srv, "bob", "localpass"); code != http.StatusOK {
		t.Fatalf("login: got %d, want 200: %s", code, body)
	}
	if n := v.callCount(); n != 0 {
		t.Errorf("verifier called %d times for a local account, want 0", n)
	}
	if code, _ := postLogin(t, srv, "bob", "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("wrong password: got %d, want 401", code)
	}
	if n := v.callCount(); n != 0 {
		t.Errorf("verifier called %d times on a failed local login, want 0", n)
	}
}

// First login for an unknown username provisions the local record.
func TestLogin_JITProvisionsUser(t *testing.T) {
	st := fake.New()
	v := &fakeVerifier{identity: aliceIdentity()}
	srv := newLDAPServer(t, st, v, ldapCfg())

	resp := doRequest(t, http.MethodPost, srv.URL+"/api/v1/auth/login",
		map[string]string{"username": "alice", "password": "dirpass"}, nil)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: got %d, want 200: %s", resp.StatusCode, body)
	}

	u, err := st.GetUserByUsername(t.Context(), "alice")
	if err != nil {
		t.Fatalf("user not provisioned: %v", err)
	}
	if u.AuthSource != store.AuthSourceLDAP {
		t.Errorf("AuthSource: got %q, want ldap", u.AuthSource)
	}
	if u.Role != "admin" {
		t.Errorf("Role: got %q, want admin (mapped from CN=Admins)", u.Role)
	}
	if u.DisplayName != "Alice Anderson" {
		t.Errorf("DisplayName: got %q", u.DisplayName)
	}
	// No local password may be usable on a directory account. A malformed
	// placeholder makes Verify return an error rather than false; either way
	// it must not report a match.
	if ok, verr := password.Verify(u.PasswordHash, "dirpass"); ok || verr == nil {
		t.Errorf("directory password must not be stored as a usable local hash (ok=%v err=%v)", ok, verr)
	}
	// A session cookie is set, exactly as for a local login.
	if len(resp.Cookies()) == 0 {
		t.Error("expected a session cookie")
	}
}

// role_source=directory recomputes the role on EVERY login.
func TestLogin_DirectoryModeResyncsRole(t *testing.T) {
	st := fake.New()
	seedLDAPUser(t, st, store.User{Username: "alice", Role: "read-only"})
	v := &fakeVerifier{identity: aliceIdentity()}
	srv := newLDAPServer(t, st, v, ldapCfg())

	if code, body := postLogin(t, srv, "alice", "pw"); code != http.StatusOK {
		t.Fatalf("login: got %d: %s", code, body)
	}
	u, err := st.GetUserByUsername(t.Context(), "alice")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if u.Role != "admin" {
		t.Errorf("Role: got %q, want admin after directory re-sync", u.Role)
	}
	if u.AuthSource != store.AuthSourceLDAP {
		t.Errorf("AuthSource changed by role sync: got %q", u.AuthSource)
	}
}

// role_source=local seeds at JIT-create and never overwrites afterwards.
func TestLogin_LocalModeDoesNotResyncRole(t *testing.T) {
	st := fake.New()
	seedLDAPUser(t, st, store.User{Username: "alice", Role: "operator"})
	cfg := ldapCfg()
	cfg.RoleSource = ldap.RoleSourceLocal
	v := &fakeVerifier{identity: aliceIdentity()}
	srv := newLDAPServer(t, st, v, cfg)

	if code, body := postLogin(t, srv, "alice", "pw"); code != http.StatusOK {
		t.Fatalf("login: got %d: %s", code, body)
	}
	u, err := st.GetUserByUsername(t.Context(), "alice")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if u.Role != "operator" {
		t.Errorf("Role: got %q, want operator preserved in local mode", u.Role)
	}
}

// The display name is seeded once and never re-synced, so a self-service
// PATCH /auth/me edit survives the next login.
//
// The seeded role is deliberately NOT the role the directory maps to. If it
// matched, resolveLDAPUser would short-circuit before UpdateUser and this test
// would pass without ever reaching the write it exists to guard. The role
// assertion below is what proves the update path actually ran.
func TestLogin_DisplayNameNotResynced(t *testing.T) {
	st := fake.New()
	seedLDAPUser(t, st, store.User{Username: "alice", Role: "read-only", DisplayName: "Ali"})
	v := &fakeVerifier{identity: aliceIdentity()} // directory says "Alice Anderson"
	srv := newLDAPServer(t, st, v, ldapCfg())

	if code, body := postLogin(t, srv, "alice", "pw"); code != http.StatusOK {
		t.Fatalf("login: got %d: %s", code, body)
	}
	u, err := st.GetUserByUsername(t.Context(), "alice")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if u.Role != "admin" {
		t.Fatalf("Role: got %q, want admin — the update path did not run, so the "+
			"display-name assertion below would be vacuous", u.Role)
	}
	if u.DisplayName != "Ali" {
		t.Errorf("DisplayName: got %q, want the self-service value preserved", u.DisplayName)
	}
}

// A directory account colliding with an existing LOCAL account must be
// rejected, never adopted: adopting would let anyone who can create a
// directory account named "admin" take over the local admin.
func TestLogin_CollisionWithLocalAccountRejected(t *testing.T) {
	st := fake.New()
	seedAuthUser(t, st, "alice", "localpass", "admin")
	v := &fakeVerifier{identity: aliceIdentity()}
	srv := newLDAPServer(t, st, v, ldapCfg())

	// The directory password must not authenticate the local account.
	if code, body := postLogin(t, srv, "alice", "dirpass"); code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401: %s", code, body)
	}
	// And the directory must not even have been consulted: the local record
	// shadows the directory entry outright.
	if n := v.callCount(); n != 0 {
		t.Errorf("verifier called %d times for a shadowed local account, want 0", n)
	}
	u, err := st.GetUserByUsername(t.Context(), "alice")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if u.AuthSource != store.AuthSourceLocal {
		t.Errorf("local account was adopted: AuthSource=%q", u.AuthSource)
	}
	if u.Role != "admin" {
		t.Errorf("local account role was modified: %q", u.Role)
	}
}

// The same collision reached the other way: the local record is created
// between the lookup and the JIT insert, so provisioning itself must reject
// on store.ErrConflict rather than adopt.
func TestLogin_ProvisioningConflictRejected(t *testing.T) {
	st := fake.New()
	v := &fakeVerifier{identity: aliceIdentity()}
	h := newAuthHandler(st, newTestLogger(), time.Hour, "sqi_session", "false", v, ldapCfg())

	seedAuthUser(t, st, "alice", "localpass", "admin")

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		"http://example.test/api/v1/auth/login", bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if _, err := h.provisionLDAPUser(req, aliceIdentity(), "admin"); err == nil {
		t.Fatal("provisionLDAPUser adopted an existing local account, want an error")
	}
	u, err := st.GetUserByUsername(t.Context(), "alice")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if u.AuthSource != store.AuthSourceLocal {
		t.Errorf("local account was adopted: AuthSource=%q", u.AuthSource)
	}
	if u.Role != "admin" {
		t.Errorf("local account role was modified: %q", u.Role)
	}
}

// A directory account that logs in under an alias — user_filter matches
// userPrincipalName while username_attr is sAMAccountName, so the caller types
// "alice@example.com" but the row is "alice" — must be recognized on every
// login, not just the first. The lookup in login misses, provisioning collides,
// and the ErrConflict branch has to resolve it to the existing directory row
// rather than a permanent 401.
func TestLogin_AliasLoginRecognizesExistingLDAPAccount(t *testing.T) {
	st := fake.New()
	seeded := seedLDAPUser(t, st, store.User{Username: "alice", Role: "read-only"})
	v := &fakeVerifier{identity: aliceIdentity()} // directory returns Username "alice"
	srv := newLDAPServer(t, st, v, ldapCfg())

	// Twice: a one-shot success would not prove the path is stable.
	for i := range 2 {
		if code, body := postLogin(t, srv, "alice@example.com", "pw"); code != http.StatusOK {
			t.Fatalf("alias login %d: got %d, want 200: %s", i, code, body)
		}
	}

	users, err := st.ListUsers(t.Context())
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("got %d users, want 1 — the alias must not create a second row: %+v", len(users), users)
	}
	u := users[0]
	if u.ID != seeded.ID {
		t.Errorf("ID: got %q, want the existing row %q", u.ID, seeded.ID)
	}
	if u.Username != "alice" {
		t.Errorf("Username: got %q, want the directory spelling preserved", u.Username)
	}
	// Recognizing the row still runs the normal role sync.
	if u.Role != "admin" {
		t.Errorf("Role: got %q, want admin re-synced from the directory", u.Role)
	}
}

// The same ErrConflict branch must still refuse a LOCAL row. Recognizing an
// account the directory itself provisioned is fine; taking over a local one is
// the privilege-escalation this defense exists to stop.
func TestLogin_AliasLoginDoesNotAdoptLocalAccount(t *testing.T) {
	st := fake.New()
	seedAuthUser(t, st, "alice", "localpass", "admin")
	v := &fakeVerifier{identity: aliceIdentity()} // directory returns Username "alice"
	srv := newLDAPServer(t, st, v, ldapCfg())

	if code, body := postLogin(t, srv, "alice@example.com", "dirpass"); code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401: %s", code, body)
	}
	users, err := st.ListUsers(t.Context())
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("got %d users, want 1: %+v", len(users), users)
	}
	u := users[0]
	if u.AuthSource != store.AuthSourceLocal {
		t.Errorf("local account was adopted: AuthSource=%q", u.AuthSource)
	}
	if u.Role != "admin" {
		t.Errorf("local account role was modified: %q", u.Role)
	}
	if u.Username != "alice" {
		t.Errorf("Username: got %q", u.Username)
	}
}

// A DC outage must fail closed and must NOT fall through to any other check.
func TestLogin_DirectoryUnavailableFailsClosed(t *testing.T) {
	st := fake.New()
	v := &fakeVerifier{err: ldap.ErrUnavailable}
	srv := newLDAPServer(t, st, v, ldapCfg())

	if code, body := postLogin(t, srv, "alice", "pw"); code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401: %s", code, body)
	}
	if _, err := st.GetUserByUsername(t.Context(), "alice"); err == nil {
		t.Error("no user may be provisioned when the directory is unavailable")
	}
}

// An account disabled locally stays out, whatever the directory says — the
// local flag is the override an operator can always reach.
func TestLogin_LocallyDisabledDirectoryUserBlocked(t *testing.T) {
	st := fake.New()
	seedLDAPUser(t, st, store.User{Username: "alice", Role: "admin", Disabled: true})
	v := &fakeVerifier{identity: aliceIdentity()}
	srv := newLDAPServer(t, st, v, ldapCfg())

	if code, body := postLogin(t, srv, "alice", "pw"); code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401: %s", code, body)
	}
	// The disabled flag must short-circuit before the bind, so a disabled
	// account cannot be used to hammer the directory either.
	if n := v.callCount(); n != 0 {
		t.Errorf("verifier called %d times for a disabled account, want 0", n)
	}
}

// A directory account with no verifier configured (LDAP switched off after
// provisioning) has no satisfiable credential and must not fall back to the
// placeholder password hash.
func TestLogin_DirectoryUserWithoutVerifierRejected(t *testing.T) {
	st := fake.New()
	seedLDAPUser(t, st, store.User{Username: "alice", Role: "admin"})
	srv := newLDAPServer(t, st, nil, ldap.Config{})

	for _, pw := range []string{"pw", ldapPlaceholderHash, ""} {
		if code, body := postLogin(t, srv, "alice", pw); code != http.StatusUnauthorized {
			t.Fatalf("password %q: got %d, want 401: %s", pw, code, body)
		}
	}
}

// default_role="" means group membership is required to sign in at all.
func TestLogin_NoRoleMatchWithEmptyDefaultRejects(t *testing.T) {
	st := fake.New()
	cfg := ldapCfg()
	cfg.DefaultRole = ""
	id := aliceIdentity()
	id.Groups = []string{"CN=Nobody,DC=example,DC=com"}
	v := &fakeVerifier{identity: id}
	srv := newLDAPServer(t, st, v, cfg)

	if code, body := postLogin(t, srv, "alice", "pw"); code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401: %s", code, body)
	}
	if _, err := st.GetUserByUsername(t.Context(), "alice"); err == nil {
		t.Error("no user may be provisioned when no role matched")
	}
}

// With LDAP off, an unknown username behaves exactly as before C1.
func TestLogin_LDAPDisabledUnknownUserStill401(t *testing.T) {
	st := fake.New()
	srv := newLDAPServer(t, st, nil, ldap.Config{}) // nil verifier = LDAP off
	if code, body := postLogin(t, srv, "ghost", "pw"); code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401: %s", code, body)
	}
	if _, err := st.GetUserByUsername(t.Context(), "ghost"); err == nil {
		t.Error("no user may be provisioned with LDAP off")
	}
}

// An auth_source the binary does not recognize must fail closed rather than
// fall through to the local-password check. Nothing validates the column in
// Go, so a hand-edited or future-version row is reachable here.
func TestLogin_UnknownAuthSourceFailsClosed(t *testing.T) {
	st := fake.New()
	hash, err := password.Hash("localpass")
	if err != nil {
		t.Fatalf("password.Hash: %v", err)
	}
	if _, err := st.CreateUser(t.Context(), store.User{
		ID: uuid.NewString(), Username: "mallory", PasswordHash: hash,
		Role: "admin", AuthSource: "saml",
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	v := &fakeVerifier{identity: aliceIdentity()}
	srv := newLDAPServer(t, st, v, ldapCfg())

	if code, body := postLogin(t, srv, "mallory", "localpass"); code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401: %s", code, body)
	}
	if n := v.callCount(); n != 0 {
		t.Errorf("verifier called %d times for an unknown auth_source, want 0", n)
	}
}

// postLoginFixedID posts a login with a caller-supplied X-Request-ID so the
// problem document's "instance" field — a per-request correlation ID, the one
// part of the body that legitimately varies — is pinned. That makes a
// byte-for-byte body comparison across requests meaningful.
func postLoginFixedID(t *testing.T, srv *httptest.Server, reqID, username, pw string) (int, string) {
	t.Helper()
	b, err := json.Marshal(map[string]string{"username": username, "password": pw})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		srv.URL+"/api/v1/auth/login", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", reqID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST login: %v", err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(out)
}

// The response body must be byte-identical across every failure mode:
// unknown account, local account with a bad password, directory account with
// a bad password, directory down, no role matched, a collision with a local
// account, a locally disabled directory account, an unrecognized auth_source,
// and a directory account whose verifier has been switched off. Any difference
// is a user-enumeration oracle.
func TestLogin_FailureBodiesAreIdentical(t *testing.T) {
	const reqID = "fixed-request-id-for-body-comparison"

	// Each case gets its own store and verifier so the failure modes cannot
	// interfere; the bodies are then compared across all of them.
	cases := []struct {
		name  string
		setup func(t *testing.T, st store.Store) *fakeVerifier
		user  string
		pw    string
		// ldapOff models auth.ldap.enabled=false: the router gets a nil
		// ldap.Verifier interface, which a typed-nil *fakeVerifier would not
		// produce.
		ldapOff bool
	}{
		{
			name:  "unknown account, ldap rejects",
			setup: func(*testing.T, store.Store) *fakeVerifier { return &fakeVerifier{err: ldap.ErrInvalidCredentials} },
			user:  "ghost", pw: "pw",
		},
		{
			name: "local account, wrong password",
			setup: func(t *testing.T, st store.Store) *fakeVerifier {
				t.Helper()
				seedAuthUser(t, st, "bob", "localpass", "user")
				return &fakeVerifier{err: ldap.ErrInvalidCredentials}
			},
			user: "bob", pw: "wrong",
		},
		{
			name: "directory account, wrong password",
			setup: func(t *testing.T, st store.Store) *fakeVerifier {
				t.Helper()
				seedLDAPUser(t, st, store.User{Username: "alice", Role: "admin"})
				return &fakeVerifier{err: ldap.ErrInvalidCredentials}
			},
			user: "alice", pw: "wrong",
		},
		{
			name:  "directory unavailable",
			setup: func(*testing.T, store.Store) *fakeVerifier { return &fakeVerifier{err: ldap.ErrUnavailable} },
			user:  "alice", pw: "pw",
		},
		{
			name: "no role matched",
			setup: func(*testing.T, store.Store) *fakeVerifier {
				id := aliceIdentity()
				id.Groups = []string{"CN=Nobody,DC=example,DC=com"}
				return &fakeVerifier{identity: id}
			},
			user: "alice", pw: "pw",
		},
		{
			name: "collides with a local account",
			setup: func(t *testing.T, st store.Store) *fakeVerifier {
				t.Helper()
				seedAuthUser(t, st, "alice", "localpass", "admin")
				return &fakeVerifier{identity: aliceIdentity()}
			},
			user: "alice", pw: "dirpass",
		},
		{
			name: "locally disabled directory account",
			setup: func(t *testing.T, st store.Store) *fakeVerifier {
				t.Helper()
				seedLDAPUser(t, st, store.User{Username: "alice", Role: "admin", Disabled: true})
				return &fakeVerifier{identity: aliceIdentity()}
			},
			user: "alice", pw: "pw",
		},
		{
			// Fails closed in login's default branch, before any of the LDAP
			// code runs — a different call site, so it must be pinned here too.
			name: "unrecognized auth_source",
			setup: func(t *testing.T, st store.Store) *fakeVerifier {
				t.Helper()
				hash, err := password.Hash("localpass")
				if err != nil {
					t.Fatalf("password.Hash: %v", err)
				}
				if _, err := st.CreateUser(t.Context(), store.User{
					ID: uuid.NewString(), Username: "mallory", PasswordHash: hash,
					Role: "admin", AuthSource: "saml",
				}); err != nil {
					t.Fatalf("CreateUser: %v", err)
				}
				return &fakeVerifier{identity: aliceIdentity()}
			},
			user: "mallory", pw: "localpass",
		},
		{
			// A directory account left behind after LDAP was switched off:
			// rejected by loginLDAP's nil-verifier guard, another distinct exit.
			name: "directory account, verifier switched off",
			setup: func(t *testing.T, st store.Store) *fakeVerifier {
				t.Helper()
				seedLDAPUser(t, st, store.User{Username: "alice", Role: "admin"})
				return nil
			},
			user: "alice", pw: "pw", ldapOff: true,
		},
	}

	bodies := make([]string, 0, len(cases))
	for _, c := range cases {
		st := fake.New()
		cfg := ldapCfg()
		if c.name == "no role matched" {
			cfg.DefaultRole = ""
		}
		var verifier ldap.Verifier
		if v := c.setup(t, st); !c.ldapOff {
			verifier = v
		}
		srv := newLDAPServer(t, st, verifier, cfg)

		code, body := postLoginFixedID(t, srv, reqID, c.user, c.pw)
		if code != http.StatusUnauthorized {
			t.Fatalf("%s: got %d, want 401: %s", c.name, code, body)
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(body), &doc); err != nil {
			t.Fatalf("%s: bad JSON: %v", c.name, err)
		}
		bodies = append(bodies, body)
	}
	for i := 1; i < len(bodies); i++ {
		if bodies[i] != bodies[0] {
			t.Errorf("failure bodies differ (%s), enabling account enumeration:\n%q\nvs\n%q",
				cases[i].name, bodies[0], bodies[i])
		}
	}
}
