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
		{"operator can manage workers", "operator", http.MethodPost, "/api/v1/workers/w1/disable", nil, http.StatusNotFound}, // allowed, so the handler runs and answers 404 (no such worker)
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
	// Auth off: the anonymous superuser passes every gate. A router with
	// auth.Anonymous must let a POST /jobs reach the handler (400 or 201,
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
// The tests above pin a handful of hand-picked cases. A route registered
// directly on the `rest` group (instead of inside one of the permission-gated
// sub-groups in router.go) is reachable by every authenticated role, and only
// walking the actual router finds it. TestAuthz_RouteSweep_* below enumerates
// every route chi serves under /api/v1 and diffs it against expectedRoutes, a
// hardcoded classification table, in both directions: a newly added ungated
// route and a stale or renamed table entry both fail.

// routeExpectation classifies one registered (method, pattern) route as
// either requiring a specific policy.Permission or being permission-free
// (reachable by any authenticated principal, or, for login/openapi.yaml/ws,
// with no principal at all).
type routeExpectation struct {
	method  string
	pattern string
	perm    policy.Permission // zero value when public is true
	public  bool
}

// expectedRoutes classifies every route sqi registers under /api/v1, derived
// from router.go's route groups. A route added there must be classified here
// too, or TestAuthz_RouteSweep_TableMatchesRouter fails.
var expectedRoutes = []routeExpectation{
	// Unauthenticated (no session required at all).
	{method: http.MethodPost, pattern: "/api/v1/auth/login", public: true},
	// Login-page discovery: unauthenticated because it tells the login page
	// which login methods exist. See authproviders.go for what it discloses.
	{method: http.MethodGet, pattern: "/api/v1/auth/providers", public: true},
	{method: http.MethodGet, pattern: "/api/v1/openapi.yaml", public: true},
	// WebSocket upgrade: authenticated by its own handler (wsH.ServeHTTP), not
	// az.require. It is excluded from the request-driving loops because it is
	// an upgrade, not a plain HTTP round trip, but it stays classified here so
	// a change of class is noticed.
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

	// workers.enroll. POST /api/v1/workers/enroll is not listed here: it is
	// unauthenticated (the join token is the credential) and this suite's
	// router does not mount it, because it requires Deps.NATSAuthEnabled,
	// which authRouterWith does not set. workerenroll_test.go covers it.
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
	{method: http.MethodGet, pattern: "/api/v1/capacity-groups", perm: policy.InfraRead},
	{method: http.MethodGet, pattern: "/api/v1/capacity-groups/{id}", perm: policy.InfraRead},
	{method: http.MethodGet, pattern: "/api/v1/capacity-groups/{id}/members", perm: policy.InfraRead},
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
	{method: http.MethodPost, pattern: "/api/v1/capacity-groups", perm: policy.InfraManage},
	{method: http.MethodPut, pattern: "/api/v1/capacity-groups/{id}", perm: policy.InfraManage},
	{method: http.MethodDelete, pattern: "/api/v1/capacity-groups/{id}", perm: policy.InfraManage},
	{method: http.MethodPut, pattern: "/api/v1/capacity-groups/{id}/members/{workerID}", perm: policy.InfraManage},
	{method: http.MethodDelete, pattern: "/api/v1/capacity-groups/{id}/members/{workerID}", perm: policy.InfraManage},
	{method: http.MethodGet, pattern: "/api/v1/capacity-groups/{id}/tokens", perm: policy.InfraManage},
	{method: http.MethodPost, pattern: "/api/v1/capacity-groups/{id}/tokens", perm: policy.InfraManage},
	{method: http.MethodDelete, pattern: "/api/v1/capacity-groups/{id}/tokens/{tokenID}", perm: policy.InfraManage},

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
	{method: http.MethodGet, pattern: "/api/v1/servers", perm: policy.DiagnosticsRead},
}

// routeKey identifies a route by method and chi pattern, matching what
// chi.Walk reports (e.g. {"GET", "/api/v1/users/{id}"}).
type routeKey struct{ method, pattern string }

// authRouterWith builds the same auth-enabled router newAuthTestServer wraps in
// an httptest.Server (auth_test.go: same store, 1-hour session TTL, cookie
// name), but returns the chi.Router directly so callers can both walk its
// route table with chi.Walk and serve it themselves.
//
// mutate, when non-nil, gets the fully populated Deps before the router is
// built; the provider-specific variants (authRouterLDAP, authRouterOIDC) use
// it to add their fields.
//
// Products is a real catalog because the products and presets routes the
// sweep drives dereference the catalog's store, and a nil catalog would panic
// instead of answering an ordinary 404 or 503 for the "zzz" placeholder.
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

// liveRoutes walks r and returns every (method, pattern) chi registered under
// /api/v1. Routes outside that prefix (health, metrics, the embedded UI
// catch-all) are out of scope for this authorization sweep.
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

// placeholderPath substitutes a dummy value for every chi path parameter in
// pattern, e.g. "/api/v1/jobs/{id}" -> "/api/v1/jobs/zzz". The gate under test
// depends on the route's permission group, not the resolved resource.
func placeholderPath(pattern string) string {
	return strings.NewReplacer("{id}", "zzz", "{name}", "zzz", "{workerID}", "zzz", "{tokenID}", "zzz").Replace(pattern)
}

// requestBodyFor returns a JSON body for method. Handlers may still reject it
// (missing fields, unknown resource, ...); the sweep asserts only 403 versus
// not 403, so any resulting 400, 404, 422 or 500 shows the gate let the
// request through to the handler.
func requestBodyFor(method string) any {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return map[string]any{}
	default:
		return nil
	}
}

// TestAuthz_RouteSweep_TableMatchesRouter fails if the live router and
// expectedRoutes disagree in either direction: a route chi serves with no
// entry in the table (an unclassified, possibly ungated route) or a table
// entry with no matching live route (stale or renamed).
func TestAuthz_RouteSweep_TableMatchesRouter(t *testing.T) {
	live := liveRoutes(t, authRouter(fake.New()))

	expected := make(map[routeKey]bool, len(expectedRoutes))
	for _, e := range expectedRoutes {
		expected[routeKey{e.method, e.pattern}] = true
	}

	for key := range live {
		if !expected[key] {
			t.Errorf("router registers %s %s with no entry in expectedRoutes; classify it as permission-gated or public", key.method, key.pattern)
		}
	}
	for key := range expected {
		if !live[key] {
			t.Errorf("expectedRoutes has %s %s but the router does not register it; update the table", key.method, key.pattern)
		}
	}
}

// assertRouteGate drives one (method, pattern) request as role through srv,
// logging in fresh for this one request, and asserts the response is 403
// exactly when policy.Can denies the route's required permission. It does not
// assert on the handler's own status. Each request logs in fresh because
// expectedRoutes includes POST /auth/logout: with a shared cookie it would
// revoke the session and turn every later route into a 401.
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

// TestAuthz_RouteSweep_EnforcesPermissionsPerRole drives, for every route in
// expectedRoutes, a real request per built-in role and confirms the gate
// matches policy.Can. GET /ws is skipped (WebSocket upgrade, authenticated by
// its own handler).
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
				continue // WebSocket upgrade, not a plain HTTP round trip.
			}
			t.Run(role+" "+e.method+" "+e.pattern, func(t *testing.T) {
				assertRouteGate(t, srv, username, password, role, e)
			})
		}
	}
}

// TestAuthz_RouteSweep_DisabledAuthAllowsAll is the route sweep's auth-off
// case: with a router built with Auth: auth.Anonymous(), every route in
// expectedRoutes (except the WebSocket upgrade) must be reachable with no
// 403, because the anonymous superuser principal bypasses every gate.
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
// TestAuthz_RouteSweep_EnforcesPermissionsPerRole above checks permission
// membership only: it drives placeholder ids ("/api/v1/jobs/zzz") with no
// seeded job and no owner, so it cannot observe per-object ownership. A role
// that holds the route's permission but is owner-scoped (e.g. `user`) gets a
// 404 whether or not the job access middleware ran, and the 403-versus-not
// assertion cannot tell the difference.
// TestAuthz_OwnershipSweep_EnforcesOwnerScoping below seeds a real job (and
// task) per owner and asserts a scoped `user` principal gets a non-403 on its
// own object and a 403 on someone else's, for each of the 11 job and task
// object routes that router.go wraps in requireJobAccessByJobID or
// requireJobAccessByTaskID.

// ownershipGatedRoutes lists every job and task object route that must enforce
// per-owner access through az.requireJobAccessByJobID or
// az.requireJobAccessByTaskID, per router.go's jobs.read and jobs.write
// groups. TestAuthz_OwnershipSweep_TableMatchesRouter diffs it, in both
// directions, against the object-shaped subset of expectedRoutes, which
// TestAuthz_RouteSweep_TableMatchesRouter diffs against the live router.
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

// isJobObjectPattern reports whether pattern addresses a specific job or task
// object, as opposed to a collection route (GET /jobs) or a creation route
// with no prior object (POST /jobs, POST /products/{name}/jobs), by checking
// for an "{id}" segment directly after "/jobs/" or "/tasks/".
func isJobObjectPattern(pattern string) bool {
	return strings.Contains(pattern, "/jobs/{id}") || strings.Contains(pattern, "/tasks/{id}")
}

// TestAuthz_OwnershipSweep_TableMatchesRouter fails if ownershipGatedRoutes
// and the jobs.read/jobs.write object-shaped subset of expectedRoutes
// disagree in either direction, and confirms every table entry is registered
// on the real router.
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
			t.Errorf("ownershipGatedRoutes has %s %s but the router does not register it", k.method, k.pattern)
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
// eligibility guard answer (404/409/204) before the handler reaches h.sched
// where possible. authRouter leaves Deps.Scheduler nil, so a call that reaches
// the scheduler nil-pointer-panics and chi's Recoverer turns it into a 500.
// Two routes reach it regardless of status and answer that recovered 500 on
// their own case: POST /api/v1/jobs/{id}/retry (retryJob calls h.sched.RetryJob
// unconditionally once the job is found) and DELETE /api/v1/jobs/{id}
// (deleteJob calls h.sched.ReconcileDependents unconditionally after the
// delete, though the terminal status chosen here skips its conditional
// CancelJob call). A non-403 there still shows the ownership gate let the
// request through, which is all this sweep checks.
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

// ownershipPath substitutes pattern's "{id}" with the task id when pattern
// addresses a task object and the job id otherwise.
//
// The mapping is derived from the pattern rather than from the router. The
// router states it by mounting requireJobAccessByJobID or
// requireJobAccessByTaskID per route, so a route wired to the wrong middleware
// fails this sweep: the wrong resolver looks up an id of the wrong kind and
// answers 404 where the table expects 200 or 403.
func ownershipPath(pattern, jobID, taskID string) string {
	id := jobID
	if strings.Contains(pattern, "/tasks/{id}") {
		id = taskID
	}
	return strings.Replace(pattern, "{id}", id, 1)
}

// assertOwnershipGate drives e twice as alice, a scoped `user` principal: once
// against a fresh object alice owns (must not be 403) and once against a fresh
// object bob owns (must be 403). idx makes the seeded IDs unique per table row
// so cases sharing a pattern (GET, PATCH and DELETE all use
// "/api/v1/jobs/{id}") do not collide or mutate each other's fixtures.
func assertOwnershipGate(t *testing.T, st *fake.Store, srv *httptest.Server, cookie *http.Cookie, idx int, e routeExpectation) {
	t.Helper()
	f := ownershipFixtureFor(e.method, e.pattern)

	ownJobID, ownTaskID := fmt.Sprintf("ownsweep-job-own-%d", idx), fmt.Sprintf("ownsweep-task-own-%d", idx)
	seedOwnedJobWithTask(t, st, ownJobID, "ownsweep-alice", ownTaskID, f.jobStatus, f.taskStatus)
	ownResp := doRequest(t, e.method, srv.URL+ownershipPath(e.pattern, ownJobID, ownTaskID), requestBodyFor(e.method), cookie)
	defer ownResp.Body.Close()
	if ownResp.StatusCode == http.StatusForbidden {
		t.Errorf("alice on her own object (%s %s): got 403, want non-403", e.method, e.pattern)
	}

	otherJobID, otherTaskID := fmt.Sprintf("ownsweep-job-other-%d", idx), fmt.Sprintf("ownsweep-task-other-%d", idx)
	seedOwnedJobWithTask(t, st, otherJobID, "ownsweep-bob", otherTaskID, f.jobStatus, f.taskStatus)
	otherResp := doRequest(t, e.method, srv.URL+ownershipPath(e.pattern, otherJobID, otherTaskID), requestBodyFor(e.method), cookie)
	defer otherResp.Body.Close()
	if otherResp.StatusCode != http.StatusForbidden {
		t.Errorf("alice on bob's object (%s %s): got %d, want 403", e.method, e.pattern, otherResp.StatusCode)
	}
}

// TestAuthz_OwnershipSweep_EnforcesOwnerScoping checks, for every route in
// ownershipGatedRoutes, that alice (scoped `user`) reaches her own job or task
// (non-403) and is refused bob's (403). Removing the job access middleware
// from one route in router.go fails that route's subtest.
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
