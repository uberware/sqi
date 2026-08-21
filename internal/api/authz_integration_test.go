// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// End-to-end authorization tests over the real router. Each case logs in as a
// role and asserts the HTTP status the matrix predicts, or (for the auth-off
// case) exercises the router with auth disabled to confirm the gate is inert.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/auth/policy"
	"github.com/uberware/sqi/internal/auth/session"
	"github.com/uberware/sqi/internal/health"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/product"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

func TestAuthz_MatrixOverRealRouter(t *testing.T) {
	cases := []struct {
		name, role, method, path string
		body                     any
		want                     int
	}{
		{"readonly cannot submit job", "read-only", http.MethodPost, "/api/v1/jobs", map[string]any{}, http.StatusForbidden},
		{"user can reach jobs read", "user", http.MethodGet, "/api/v1/jobs", nil, http.StatusOK},
		{"user cannot manage workers", "user", http.MethodPost, "/api/v1/workers/w1/disable", nil, http.StatusForbidden},
		{"operator can manage workers", "operator", http.MethodPost, "/api/v1/workers/w1/disable", nil, http.StatusNotFound}, // allowed → handler runs → 404 (no such worker)
		{"operator cannot list users", "operator", http.MethodGet, "/api/v1/users", nil, http.StatusForbidden},
		{"admin can list users", "admin", http.MethodGet, "/api/v1/users", nil, http.StatusOK},
		{"readonly cannot read diagnostics", "read-only", http.MethodGet, "/api/v1/diagnostics/logs", nil, http.StatusForbidden},
		{"readonly can read workers", "read-only", http.MethodGet, "/api/v1/workers", nil, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := fake.New()
			seedAuthUser(t, st, "u", "pw-secret-1", tc.role)
			srv := newAuthTestServer(t, st)
			cookie := loginCookie(t, srv, "u", "pw-secret-1")
			resp := doRequest(t, tc.method, srv.URL+tc.path, tc.body, cookie)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("%s %s as %s: status = %d, want %d", tc.method, tc.path, tc.role, resp.StatusCode, tc.want)
			}
		})
	}
}

func TestAuthz_DisabledIsUnchanged(t *testing.T) {
	// Auth off: anonymous superuser passes every gate. A raw router with a nil
	// Auth (auth.Anonymous) must let a POST /jobs reach the handler (400/201,
	// never 403).
	deps := Deps{Store: fake.New(), Auth: auth.Anonymous(), CookieName: "sqi_session"}
	r := NewRouter(Config{DisableRateLimit: true}, deps, newTestLogger(), metrics.New(), health.NewRegistry())
	srv := httptest.NewServer(r)
	defer srv.Close()

	resp := doRequest(t, http.MethodPost, srv.URL+"/api/v1/jobs", map[string]any{}, nil)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		t.Fatal("auth-off POST /jobs returned 403; authorization must be inert when disabled")
	}
}

// ── route sweep ──────────────────────────────────────────────────────────────
//
// The tests above pin a handful of hand-picked cases. Nothing short of
// walking the actual router closes the fail-open gap: a route registered
// directly on the `rest` group (instead of inside one of the permission-gated
// sub-groups in router.go) is reachable by every authenticated role and the
// hand-picked cases above would never notice. TestAuthz_RouteSweep_* below
// enumerates every route chi actually serves under /api/v1 and diffs it
// against expectedRoutes, a hardcoded classification table — in both
// directions, so both a newly added ungated route and a stale/renamed table
// entry fail the build.

// routeExpectation classifies one registered (method, pattern) route as
// either requiring a specific policy.Permission or being permission-free
// (reachable by any authenticated principal — or, for login/openapi.yaml/ws,
// with no principal at all).
type routeExpectation struct {
	method  string
	pattern string
	perm    policy.Permission // zero value when public is true
	public  bool
}

// expectedRoutes is the hardcoded ground truth for every route sqi registers
// under /api/v1, hand-derived from router.go's route groups. Keep it in sync
// with router.go: adding a route there means classifying it here too, or
// TestAuthz_RouteSweep_TableMatchesRouter fails.
var expectedRoutes = []routeExpectation{
	// Unauthenticated (no session required at all).
	{method: http.MethodPost, pattern: "/api/v1/auth/login", public: true},
	// Login-page discovery: unauthenticated by necessity, since it is what
	// tells the login page which login methods exist. Gating it would be
	// circular. See authproviders.go for what it does and does not disclose.
	{method: http.MethodGet, pattern: "/api/v1/auth/providers", public: true},
	{method: http.MethodGet, pattern: "/api/v1/openapi.yaml", public: true},
	// WebSocket upgrade: gated by its own hook (wsH.ServeHTTP), not
	// az.require — deliberately excluded from the request-driving loop
	// (it's an upgrade, not a plain HTTP round trip) but still classified
	// here so it can't silently change class unnoticed.
	{method: http.MethodGet, pattern: "/api/v1/ws", public: true},

	// Permission-free once authenticated (any principal, no role check).
	{method: http.MethodPost, pattern: "/api/v1/auth/logout", public: true},
	{method: http.MethodGet, pattern: "/api/v1/auth/me", public: true},
	{method: http.MethodPatch, pattern: "/api/v1/auth/me", public: true},
	{method: http.MethodPut, pattern: "/api/v1/auth/password", public: true},
	{method: http.MethodGet, pattern: "/api/v1/version", public: true},

	// users.read / users.manage
	{method: http.MethodGet, pattern: "/api/v1/users", perm: policy.UsersRead},
	{method: http.MethodGet, pattern: "/api/v1/users/{id}", perm: policy.UsersRead},
	{method: http.MethodPost, pattern: "/api/v1/users", perm: policy.UsersManage},
	{method: http.MethodPatch, pattern: "/api/v1/users/{id}", perm: policy.UsersManage},
	{method: http.MethodPut, pattern: "/api/v1/users/{id}/password", perm: policy.UsersManage},
	{method: http.MethodDelete, pattern: "/api/v1/users/{id}", perm: policy.UsersManage},

	// apikeys.self
	{method: http.MethodPost, pattern: "/api/v1/api-keys", perm: policy.APIKeysSelf},
	{method: http.MethodGet, pattern: "/api/v1/api-keys", perm: policy.APIKeysSelf},
	{method: http.MethodDelete, pattern: "/api/v1/api-keys/{id}", perm: policy.APIKeysSelf},

	// apikeys.admin
	{method: http.MethodGet, pattern: "/api/v1/users/{id}/api-keys", perm: policy.APIKeysAdmin},
	{method: http.MethodDelete, pattern: "/api/v1/users/{id}/api-keys/{keyId}", perm: policy.APIKeysAdmin},

	// jobs.read
	{method: http.MethodGet, pattern: "/api/v1/jobs", perm: policy.JobsRead},
	{method: http.MethodGet, pattern: "/api/v1/jobs/{id}", perm: policy.JobsRead},
	{method: http.MethodGet, pattern: "/api/v1/jobs/{id}/tasks", perm: policy.JobsRead},
	{method: http.MethodGet, pattern: "/api/v1/tasks/{id}", perm: policy.JobsRead},
	{method: http.MethodGet, pattern: "/api/v1/tasks/{id}/logs", perm: policy.JobsRead},
	{method: http.MethodGet, pattern: "/api/v1/tasks/{id}/attempts", perm: policy.JobsRead},
	// jobs.write
	{method: http.MethodPost, pattern: "/api/v1/jobs", perm: policy.JobsWrite},
	{method: http.MethodPatch, pattern: "/api/v1/jobs/{id}", perm: policy.JobsWrite},
	{method: http.MethodPost, pattern: "/api/v1/jobs/{id}/cancel", perm: policy.JobsWrite},
	{method: http.MethodPost, pattern: "/api/v1/jobs/{id}/retry", perm: policy.JobsWrite},
	{method: http.MethodDelete, pattern: "/api/v1/jobs/{id}", perm: policy.JobsWrite},
	{method: http.MethodPost, pattern: "/api/v1/tasks/{id}/retry", perm: policy.JobsWrite},
	{method: http.MethodPost, pattern: "/api/v1/tasks/{id}/cancel", perm: policy.JobsWrite},
	{method: http.MethodPost, pattern: "/api/v1/products/{name}/jobs", perm: policy.JobsWrite},

	// workers.read / workers.manage
	{method: http.MethodGet, pattern: "/api/v1/workers", perm: policy.WorkersRead},
	{method: http.MethodGet, pattern: "/api/v1/workers/{id}", perm: policy.WorkersRead},
	{method: http.MethodPost, pattern: "/api/v1/workers/{id}/disable", perm: policy.WorkersManage},
	{method: http.MethodPost, pattern: "/api/v1/workers/{id}/enable", perm: policy.WorkersManage},
	{method: http.MethodDelete, pattern: "/api/v1/workers/{id}", perm: policy.WorkersManage},

	// workers.enroll. POST /api/v1/workers/enroll is NOT listed here: it is
	// unauthenticated by design (the join token is the credential) and, in
	// this suite's router, never even mounted — it requires
	// Deps.NATSAuthEnabled, which authRouterWith does not set. See
	// workerenroll_test.go for its own coverage.
	{method: http.MethodPost, pattern: "/api/v1/workers/join-tokens", perm: policy.WorkersEnroll},
	{method: http.MethodDelete, pattern: "/api/v1/workers/{id}/credential", perm: policy.WorkersEnroll},

	// infra.read / infra.manage (farms, queues, storage-locations,
	// compute-locations, usage-pools)
	{method: http.MethodGet, pattern: "/api/v1/farms", perm: policy.InfraRead},
	{method: http.MethodGet, pattern: "/api/v1/farms/{id}", perm: policy.InfraRead},
	{method: http.MethodGet, pattern: "/api/v1/queues", perm: policy.InfraRead},
	{method: http.MethodGet, pattern: "/api/v1/queues/{id}", perm: policy.InfraRead},
	{method: http.MethodGet, pattern: "/api/v1/storage-locations", perm: policy.InfraRead},
	{method: http.MethodGet, pattern: "/api/v1/storage-locations/{id}", perm: policy.InfraRead},
	{method: http.MethodGet, pattern: "/api/v1/compute-locations", perm: policy.InfraRead},
	{method: http.MethodGet, pattern: "/api/v1/compute-locations/{id}", perm: policy.InfraRead},
	{method: http.MethodGet, pattern: "/api/v1/usage-pools", perm: policy.InfraRead},
	{method: http.MethodGet, pattern: "/api/v1/usage-pools/{id}", perm: policy.InfraRead},
	{method: http.MethodPost, pattern: "/api/v1/farms", perm: policy.InfraManage},
	{method: http.MethodPut, pattern: "/api/v1/farms/{id}", perm: policy.InfraManage},
	{method: http.MethodDelete, pattern: "/api/v1/farms/{id}", perm: policy.InfraManage},
	{method: http.MethodPost, pattern: "/api/v1/queues", perm: policy.InfraManage},
	{method: http.MethodPut, pattern: "/api/v1/queues/{id}", perm: policy.InfraManage},
	{method: http.MethodDelete, pattern: "/api/v1/queues/{id}", perm: policy.InfraManage},
	{method: http.MethodPost, pattern: "/api/v1/storage-locations", perm: policy.InfraManage},
	{method: http.MethodPut, pattern: "/api/v1/storage-locations/{id}", perm: policy.InfraManage},
	{method: http.MethodDelete, pattern: "/api/v1/storage-locations/{id}", perm: policy.InfraManage},
	{method: http.MethodPost, pattern: "/api/v1/compute-locations", perm: policy.InfraManage},
	{method: http.MethodPut, pattern: "/api/v1/compute-locations/{id}", perm: policy.InfraManage},
	{method: http.MethodDelete, pattern: "/api/v1/compute-locations/{id}", perm: policy.InfraManage},
	{method: http.MethodPost, pattern: "/api/v1/usage-pools", perm: policy.InfraManage},
	{method: http.MethodPut, pattern: "/api/v1/usage-pools/{id}", perm: policy.InfraManage},
	{method: http.MethodDelete, pattern: "/api/v1/usage-pools/{id}", perm: policy.InfraManage},

	// products.read / products.manage (products, presets)
	{method: http.MethodGet, pattern: "/api/v1/products", perm: policy.ProductsRead},
	{method: http.MethodGet, pattern: "/api/v1/products/{name}", perm: policy.ProductsRead},
	{method: http.MethodGet, pattern: "/api/v1/products/{name}/parameters", perm: policy.ProductsRead},
	{method: http.MethodGet, pattern: "/api/v1/presets", perm: policy.ProductsRead},
	{method: http.MethodGet, pattern: "/api/v1/presets/{name}", perm: policy.ProductsRead},
	{method: http.MethodPost, pattern: "/api/v1/products", perm: policy.ProductsManage},
	{method: http.MethodPut, pattern: "/api/v1/products/{name}", perm: policy.ProductsManage},
	{method: http.MethodDelete, pattern: "/api/v1/products/{name}", perm: policy.ProductsManage},
	{method: http.MethodPost, pattern: "/api/v1/presets/{name}/install", perm: policy.ProductsManage},

	// diagnostics.read
	{method: http.MethodGet, pattern: "/api/v1/diagnostics/logs", perm: policy.DiagnosticsRead},
}

// routeKey identifies a route by method and chi pattern, matching what
// chi.Walk reports (e.g. {"GET", "/api/v1/users/{id}"}).
type routeKey struct{ method, pattern string }

// authRouterWith builds the same auth-enabled router newAuthTestServer wraps in
// an httptest.Server (auth_test.go: same store, 1-hour session TTL, cookie
// name), but returns the chi.Router directly so callers can both walk its
// route table with chi.Walk and serve it themselves.
//
// mutate, when non-nil, gets the fully-populated Deps before the router is
// built, which is how the provider-specific variants (authRouterLDAP,
// authRouterOIDC) add their two or three fields. Keeping the base literal in
// one place means a newly required Deps field is added once rather than
// silently omitted from two of three near-identical copies.
//
// Products is wired to a real catalog (not left nil) so the products/presets
// routes the sweep drives — GetByName/Delete/etc. all deref the catalog's
// store field unconditionally — return an ordinary 404/503 for the "zzz"
// placeholder instead of panicking on a nil receiver.
func authRouterWith(st store.Store, mutate func(*Deps)) chi.Router {
	deps := Deps{
		Store:         st,
		Products:      product.NewCatalog(st),
		Auth:          session.New(st, "sqi_session", nil),
		SessionTTL:    time.Hour,
		CookieName:    "sqi_session",
		CookieSecure:  "false",
		WorkerRevoker: storeRevoker{store: st},
	}
	if mutate != nil {
		mutate(&deps)
	}
	return NewRouter(
		Config{DisableRateLimit: true, AuthEnabled: true},
		deps,
		newTestLogger(), metrics.New(), health.NewRegistry(),
	)
}

// authRouter is authRouterWith and no provider: local accounts only.
func authRouter(st store.Store) chi.Router { return authRouterWith(st, nil) }

// liveRoutes walks r and returns every (method, pattern) chi actually
// registered under /api/v1. Routes outside that prefix (health, metrics, the
// embedded UI catch-all) are out of scope for this authorization sweep.
func liveRoutes(t *testing.T, r chi.Router) map[routeKey]bool {
	t.Helper()
	live := map[routeKey]bool{}
	err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.HasPrefix(route, "/api/v1") {
			live[routeKey{method, route}] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	return live
}

// placeholderPath substitutes a concrete dummy value for every chi path
// parameter in pattern, e.g. "/api/v1/jobs/{id}" -> "/api/v1/jobs/zzz". The
// gate under test only cares about the permission group a route belongs to,
// never the resolved resource, so any placeholder is representative.
func placeholderPath(pattern string) string {
	return strings.NewReplacer("{id}", "zzz", "{name}", "zzz").Replace(pattern)
}

// requestBodyFor returns a plausible JSON body for method, matching the
// style of the hand-picked cases in TestAuthz_MatrixOverRealRouter above.
// Handlers may still reject it (missing fields, unknown resource, ...) — the
// sweep asserts only on 403-vs-not-403, so any resulting 400/404/422/500 is
// fine; it proves the gate let the request through to the handler.
func requestBodyFor(method string) any {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return map[string]any{}
	default:
		return nil
	}
}

// TestAuthz_RouteSweep_TableMatchesRouter is finding (1)(a)+(b): it fails if
// the live router and expectedRoutes disagree in either direction — a route
// chi serves with no entry in the table (an unclassified, possibly ungated,
// new route) or a table entry with no matching live route (stale/renamed).
func TestAuthz_RouteSweep_TableMatchesRouter(t *testing.T) {
	live := liveRoutes(t, authRouter(fake.New()))

	expected := make(map[routeKey]bool, len(expectedRoutes))
	for _, e := range expectedRoutes {
		expected[routeKey{e.method, e.pattern}] = true
	}

	for key := range live {
		if !expected[key] {
			t.Errorf("router registers %s %s with no entry in expectedRoutes — classify it as permission-gated or public", key.method, key.pattern)
		}
	}
	for key := range expected {
		if !live[key] {
			t.Errorf("expectedRoutes has %s %s but the router no longer registers it — update the table", key.method, key.pattern)
		}
	}
}

// assertRouteGate drives one (method, pattern) request as role through srv,
// logging in fresh for this one request, and asserts the response is 403
// exactly when policy.Can denies the route's required permission — never
// asserting on the handler's own status. A fresh login per request (rather
// than one cookie shared across a role's whole route list) matters because
// expectedRoutes includes POST /auth/logout itself: driving it with a shared
// cookie would revoke the session server-side and turn every subsequent
// route in the list into a spurious 401 instead of the 403 (or non-403) the
// gate should actually produce.
func assertRouteGate(t *testing.T, srv *httptest.Server, username, password, role string, e routeExpectation) {
	t.Helper()
	cookie := loginCookie(t, srv, username, password)
	path := placeholderPath(e.pattern)
	resp := doRequest(t, e.method, srv.URL+path, requestBodyFor(e.method), cookie)
	defer resp.Body.Close()

	wantForbidden := !e.public && !policy.Can(auth.Principal{Roles: []string{role}}, e.perm)
	gotForbidden := resp.StatusCode == http.StatusForbidden
	if gotForbidden != wantForbidden {
		t.Errorf("%s %s as %s: forbidden = %v, want %v (status %d)",
			e.method, path, role, gotForbidden, wantForbidden, resp.StatusCode)
	}
}

// TestAuthz_RouteSweep_EnforcesPermissionsPerRole is finding (1)(c): for
// every non-public route in expectedRoutes, drive a real request per
// built-in role and confirm the gate matches policy.Can exactly. GET /ws is
// skipped (WebSocket upgrade, gated by its own hook — finding (1)(d)).
func TestAuthz_RouteSweep_EnforcesPermissionsPerRole(t *testing.T) {
	st := fake.New()
	srv := httptest.NewServer(authRouter(st))
	t.Cleanup(srv.Close)

	const password = "pw-secret-1"
	for _, role := range []string{"read-only", "user", "operator", "admin"} {
		username := "sweep-" + role
		seedAuthUser(t, st, username, password, role)

		for _, e := range expectedRoutes {
			if e.pattern == "/api/v1/ws" {
				continue // WebSocket upgrade — not a plain HTTP round trip.
			}
			t.Run(role+" "+e.method+" "+e.pattern, func(t *testing.T) {
				assertRouteGate(t, srv, username, password, role, e)
			})
		}
	}
}

// TestAuthz_RouteSweep_DisabledAuthAllowsAll is finding (1)'s auth-off case:
// with a router built with Auth: auth.Anonymous(), every route in
// expectedRoutes (bar the WebSocket upgrade) must be reachable with zero
// 403s — the anonymous superuser principal bypasses every gate.
func TestAuthz_RouteSweep_DisabledAuthAllowsAll(t *testing.T) {
	st := fake.New()
	r := NewRouter(
		Config{DisableRateLimit: true},
		Deps{Store: st, Products: product.NewCatalog(st), Auth: auth.Anonymous(), CookieName: "sqi_session", WorkerRevoker: storeRevoker{store: st}},
		newTestLogger(), metrics.New(), health.NewRegistry(),
	)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	for _, e := range expectedRoutes {
		if e.pattern == "/api/v1/ws" {
			continue
		}
		t.Run(e.method+" "+e.pattern, func(t *testing.T) {
			path := placeholderPath(e.pattern)
			resp := doRequest(t, e.method, srv.URL+path, requestBodyFor(e.method), nil)
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusForbidden {
				t.Errorf("%s %s: got 403 with auth disabled; anonymous superuser must bypass every gate", e.method, path)
			}
		})
	}
}

// ── ownership sweep ──────────────────────────────────────────────────────────
//
// TestAuthz_RouteSweep_EnforcesPermissionsPerRole above proves permission
// membership only: it drives placeholder ids ("/api/v1/jobs/zzz") with no
// seeded job and no owner, so it cannot observe per-object ownership — a
// role that HOLDS the route's permission but is owner-scoped (e.g. `user`)
// hits a 404 either way, before or after requireJobAccess runs, and the
// assertion (403-vs-not) is blind to the difference. A reviewer proved this
// by sabotage: deleting az.requireJobAccess() from DELETE /jobs/{id} — the
// exact vulnerability Task 4 closes — left the entire internal/api suite
// green. TestAuthz_OwnershipSweep_EnforcesOwnerScoping below closes that gap
// by seeding a REAL job (and task) per owner and asserting a scoped `user`
// principal gets a non-403 on its own object and a 403 on someone else's,
// for every one of the 11 job/task object routes requireJobAccess is wired
// onto in router.go.

// ownershipGatedRoutes is the hardcoded ground truth for every job/task
// object route that must enforce per-owner access via
// az.requireJobAccess(), per router.go's jobs.read/jobs.write groups.
// TestAuthz_OwnershipSweep_TableMatchesRouter diffs it, in both directions,
// against the object-shaped subset of expectedRoutes — itself already
// diffed against the live router by TestAuthz_RouteSweep_TableMatchesRouter
// — so a route rename or a forgotten table update goes loud, not silent.
var ownershipGatedRoutes = []routeExpectation{
	// jobs.read
	{method: http.MethodGet, pattern: "/api/v1/jobs/{id}", perm: policy.JobsRead},
	{method: http.MethodGet, pattern: "/api/v1/jobs/{id}/tasks", perm: policy.JobsRead},
	{method: http.MethodGet, pattern: "/api/v1/tasks/{id}", perm: policy.JobsRead},
	{method: http.MethodGet, pattern: "/api/v1/tasks/{id}/logs", perm: policy.JobsRead},
	{method: http.MethodGet, pattern: "/api/v1/tasks/{id}/attempts", perm: policy.JobsRead},
	// jobs.write
	{method: http.MethodPatch, pattern: "/api/v1/jobs/{id}", perm: policy.JobsWrite},
	{method: http.MethodPost, pattern: "/api/v1/jobs/{id}/cancel", perm: policy.JobsWrite},
	{method: http.MethodPost, pattern: "/api/v1/jobs/{id}/retry", perm: policy.JobsWrite},
	{method: http.MethodDelete, pattern: "/api/v1/jobs/{id}", perm: policy.JobsWrite},
	{method: http.MethodPost, pattern: "/api/v1/tasks/{id}/retry", perm: policy.JobsWrite},
	{method: http.MethodPost, pattern: "/api/v1/tasks/{id}/cancel", perm: policy.JobsWrite},
}

// isJobObjectPattern reports whether pattern addresses a specific job or
// task object — as opposed to a collection route (GET /jobs) or a creation
// route with no prior object (POST /jobs, POST /products/{name}/jobs) — by
// checking for an "{id}" segment directly after "/jobs/" or "/tasks/".
func isJobObjectPattern(pattern string) bool {
	return strings.Contains(pattern, "/jobs/{id}") || strings.Contains(pattern, "/tasks/{id}")
}

// TestAuthz_OwnershipSweep_TableMatchesRouter fails if ownershipGatedRoutes
// and the jobs.read/jobs.write object-shaped subset of expectedRoutes
// disagree in either direction, and independently confirms every table
// entry is still live on the real router — the same two-directional-plus-live
// pattern TestAuthz_RouteSweep_TableMatchesRouter uses for expectedRoutes
// itself, so this table cannot silently drift from router.go.
func TestAuthz_OwnershipSweep_TableMatchesRouter(t *testing.T) {
	derived := map[routeKey]bool{}
	for _, e := range expectedRoutes {
		if (e.perm == policy.JobsRead || e.perm == policy.JobsWrite) && isJobObjectPattern(e.pattern) {
			derived[routeKey{e.method, e.pattern}] = true
		}
	}

	table := map[routeKey]bool{}
	for _, e := range ownershipGatedRoutes {
		table[routeKey{e.method, e.pattern}] = true
	}

	for k := range derived {
		if !table[k] {
			t.Errorf("expectedRoutes has job/task object route %s %s with no entry in ownershipGatedRoutes", k.method, k.pattern)
		}
	}
	for k := range table {
		if !derived[k] {
			t.Errorf("ownershipGatedRoutes has %s %s but it is not a jobs.read/jobs.write object route in expectedRoutes", k.method, k.pattern)
		}
	}

	live := liveRoutes(t, authRouter(fake.New()))
	for k := range table {
		if !live[k] {
			t.Errorf("ownershipGatedRoutes has %s %s but the router no longer registers it", k.method, k.pattern)
		}
	}
}

// ownershipFixture is the job/task status pair seeded for one ownership
// sweep case.
type ownershipFixture struct {
	jobStatus  store.JobStatus
	taskStatus store.TaskStatus
}

// ownershipFixtureFor picks a job/task status that lets the route's own
// eligibility guard answer (404/409/204) before the handler ever reaches
// h.sched where possible — this test's router (authRouter) leaves
// Deps.Scheduler nil, same as every other sweep in this file, so a call that
// actually reaches the scheduler nil-pointer-panics (recovered by chi's
// Recoverer into a 500). Two routes reach it regardless of status and hit
// that recovered 500 on their "own" case: POST /api/v1/jobs/{id}/retry
// (jobs.go retryJob calls h.sched.RetryJob unconditionally once the job is
// found — no status guard exists) and DELETE /api/v1/jobs/{id} (deleteJob
// calls h.sched.ReconcileDependents unconditionally after the delete,
// though the terminal status chosen here does still skip its conditional
// CancelJob call). Both are tolerated below: a non-403 there still proves
// the ownership gate let the request through, which is what this sweep
// checks — it is not asserting the downstream action fully succeeds.
func ownershipFixtureFor(method, pattern string) ownershipFixture {
	f := ownershipFixture{jobStatus: store.JobStatusPending, taskStatus: store.TaskStatusReady}
	switch {
	case method == http.MethodPost && pattern == "/api/v1/jobs/{id}/cancel":
		f.jobStatus = store.JobStatusCanceled // idempotent fast-path: no scheduler call
	case method == http.MethodDelete && pattern == "/api/v1/jobs/{id}":
		f.jobStatus = store.JobStatusCanceled // terminal: skips the conditional CancelJob call
	case method == http.MethodPost && pattern == "/api/v1/tasks/{id}/cancel":
		f.taskStatus = store.TaskStatusSucceeded // terminal: 409 before CancelTask
	}
	return f
}

// seedOwnershipObject creates a job (and a task under it) owned by owner,
// with IDs unique to idSuffix so each sweep case mutates only its own
// scratch objects.
func seedOwnershipObject(t *testing.T, st *fake.Store, idSuffix, owner string, f ownershipFixture) (jobID, taskID string) {
	t.Helper()
	jobID = "ownsweep-job-" + idSuffix
	taskID = "ownsweep-task-" + idSuffix
	if _, err := st.CreateJob(t.Context(), store.Job{
		ID: jobID, Name: jobID, Owner: owner, Status: f.jobStatus, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateJob(%s): %v", jobID, err)
	}
	if _, err := st.CreateTask(t.Context(), store.Task{
		ID: taskID, JobID: jobID, Status: f.taskStatus,
	}); err != nil {
		t.Fatalf("CreateTask(%s): %v", taskID, err)
	}
	return jobID, taskID
}

// ownershipPath substitutes pattern's "{id}" with the task id when pattern
// addresses a task object and the job id otherwise.
//
// This is the TEST's own idea of what each route's {id} means, deliberately
// derived from the pattern rather than from the router. The router now states
// that mapping explicitly by mounting requireJobAccessByJobID or
// requireJobAccessByTaskID per route, so if the two ever disagree — a route
// wired to the wrong middleware — this sweep fails: the wrong resolver looks
// up an id of the wrong kind and answers 404 where the table expects 200/403.
func ownershipPath(pattern, jobID, taskID string) string {
	id := jobID
	if strings.Contains(pattern, "/tasks/{id}") {
		id = taskID
	}
	return strings.Replace(pattern, "{id}", id, 1)
}

// assertOwnershipGate drives e twice as alice — a scoped `user` principal —
// once against a fresh object alice owns (must NOT be 403) and once against
// a fresh object bob owns (MUST be 403). idx makes the seeded IDs unique per
// table row so cases sharing a pattern (GET/PATCH/DELETE all use
// "/api/v1/jobs/{id}") don't collide or mutate each other's fixtures.
func assertOwnershipGate(t *testing.T, st *fake.Store, srv *httptest.Server, cookie *http.Cookie, idx int, e routeExpectation) {
	t.Helper()
	f := ownershipFixtureFor(e.method, e.pattern)

	ownJobID, ownTaskID := seedOwnershipObject(t, st, fmt.Sprintf("own-%d", idx), "ownsweep-alice", f)
	ownResp := doRequest(t, e.method, srv.URL+ownershipPath(e.pattern, ownJobID, ownTaskID), requestBodyFor(e.method), cookie)
	defer ownResp.Body.Close()
	if ownResp.StatusCode == http.StatusForbidden {
		t.Errorf("alice on her own object (%s %s): got 403, want non-403", e.method, e.pattern)
	}

	otherJobID, otherTaskID := seedOwnershipObject(t, st, fmt.Sprintf("other-%d", idx), "ownsweep-bob", f)
	otherResp := doRequest(t, e.method, srv.URL+ownershipPath(e.pattern, otherJobID, otherTaskID), requestBodyFor(e.method), cookie)
	defer otherResp.Body.Close()
	if otherResp.StatusCode != http.StatusForbidden {
		t.Errorf("alice on bob's object (%s %s): got %d, want 403", e.method, e.pattern, otherResp.StatusCode)
	}
}

// TestAuthz_OwnershipSweep_EnforcesOwnerScoping is the sabotage-proof sweep:
// for every route in ownershipGatedRoutes, alice (scoped `user`) must reach
// her own job/task (non-403) and be refused bob's (403). Removing
// az.requireJobAccess() from any one route in router.go must fail exactly
// that route's subtest.
func TestAuthz_OwnershipSweep_EnforcesOwnerScoping(t *testing.T) {
	const password = "pw-secret-1"
	st := fake.New()
	seedAuthUser(t, st, "ownsweep-alice", password, "user")
	srv := httptest.NewServer(authRouter(st))
	t.Cleanup(srv.Close)
	cookie := loginCookie(t, srv, "ownsweep-alice", password)

	for i, e := range ownershipGatedRoutes {
		t.Run(e.method+" "+e.pattern, func(t *testing.T) {
			assertOwnershipGate(t, st, srv, cookie, i, e)
		})
	}
}
