// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Unit tests for the worker broker-credential REST handlers.
//
// Route coverage:
//
//	POST   /api/v1/workers/enroll          — enroll
//	POST   /api/v1/workers/join-tokens     — createJoinToken
//	DELETE /api/v1/workers/{id}/credential — revokeCredential

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/health"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// ── router helper ────────────────────────────────────────────────────────────

// storeRevoker adapts a store.Store directly to [WorkerRevoker] by writing
// the credential row and nothing else — no broker reload. The real
// broker-reload behavior belongs to *server.Server (internal/server cannot
// be imported here: internal/server imports internal/api, not the other way
// around) and is covered by test/integration's broker-auth suite. Every test
// in this file that does not care about broker semantics gets this by
// default via newWorkerEnrollRouter.
type storeRevoker struct{ store store.WorkerCredentialStore }

func (r storeRevoker) RevokeWorker(ctx context.Context, workerID string) error {
	return r.store.RevokeWorkerCredential(ctx, workerID, time.Now().UTC())
}

// noopReloader is a [BrokerCredentialReloader] that does nothing and
// returns nil — the enroll-side default for tests that don't care about
// broker-reload semantics, matching storeRevoker's role on the revoke side.
type noopReloader struct{}

func (noopReloader) ReloadBrokerCredentials(context.Context) error { return nil }

// recordingReloader is a [BrokerCredentialReloader] stub that counts calls
// and returns a configurable error, for tests verifying enroll's delegation
// to the reloader and its log-and-continue failure handling.
type recordingReloader struct {
	calls int
	err   error
}

func (r *recordingReloader) ReloadBrokerCredentials(context.Context) error {
	r.calls++
	return r.err
}

func newWorkerEnrollRouter(st store.Store, singleUse bool, ttl time.Duration) chi.Router {
	return newWorkerEnrollRouterWith(st, storeRevoker{store: st}, noopReloader{}, singleUse, ttl)
}

func newWorkerEnrollRouterWithRevoker(st store.Store, revoker WorkerRevoker, singleUse bool, ttl time.Duration) chi.Router {
	return newWorkerEnrollRouterWith(st, revoker, noopReloader{}, singleUse, ttl)
}

func newWorkerEnrollRouterWith(st store.Store, revoker WorkerRevoker, reloader BrokerCredentialReloader, singleUse bool, ttl time.Duration) chi.Router {
	h := newWorkerEnrollHandler(st, revoker, reloader, newTestLogger(), singleUse, ttl)
	r := chi.NewRouter()
	r.Post("/workers/enroll", h.enroll)
	r.Post("/workers/join-tokens", h.createJoinToken)
	r.Delete("/workers/{id}/credential", h.revokeCredential)
	return r
}

// ── seed helpers ─────────────────────────────────────────────────────────────

// seedJoinToken creates a join token directly in the store (bypassing the
// mint handler) and returns the raw token alongside the stored record.
// mutate, when non-nil, runs after the default fields are set so a test can
// override ExpiresAt or UsedAt to exercise the reject paths.
func seedJoinToken(t *testing.T, st store.Store, mutate func(*store.WorkerJoinToken)) (raw string, tok store.WorkerJoinToken) {
	t.Helper()
	raw, hash, prefix, err := brokerauth.GenerateJoinToken()
	if err != nil {
		t.Fatalf("GenerateJoinToken: %v", err)
	}
	rec := store.WorkerJoinToken{
		ID:        uuid.NewString(),
		TokenHash: hash,
		Prefix:    prefix,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
		CreatedAt: time.Now().UTC(),
	}
	if mutate != nil {
		mutate(&rec)
	}
	created, err := st.CreateWorkerJoinToken(t.Context(), rec)
	if err != nil {
		t.Fatalf("CreateWorkerJoinToken: %v", err)
	}
	return raw, created
}

// genPublicKey returns a fresh, valid nkey user public key.
func genPublicKey(t *testing.T) string {
	t.Helper()
	_, pub, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed: %v", err)
	}
	return pub
}

// ── POST /workers/enroll ─────────────────────────────────────────────────────

func TestWorkerEnroll_ValidTokenAndKey_Created(t *testing.T) {
	st := fake.New()
	r := newWorkerEnrollRouter(st, true, time.Hour)
	raw, _ := seedJoinToken(t, st, nil)
	pub := genPublicKey(t)

	req := newReq(t, http.MethodPost, "/workers/enroll", jsonBody(t, workerEnrollRequest{
		JoinToken: raw, WorkerID: "w1", PublicKey: pub, Name: "render-01",
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — body: %s", rr.Code, rr.Body)
	}
	var resp workerCredentialResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.WorkerID != "w1" || resp.PublicKey != pub || resp.Name != "render-01" {
		t.Errorf("response = %+v, want worker_id=w1 public_key=%s name=render-01", resp, pub)
	}

	cred, err := st.GetActiveWorkerCredentialByWorkerID(t.Context(), "w1")
	if err != nil {
		t.Fatalf("GetActiveWorkerCredentialByWorkerID: %v", err)
	}
	if cred.PublicKey != pub {
		t.Errorf("stored credential public key = %q, want %q", cred.PublicKey, pub)
	}

	// The token must never be echoed back in the response.
	if strings.Contains(rr.Body.String(), raw) {
		t.Error("response body echoes the raw join token")
	}
}

// TestWorkerEnroll_ReloadsBrokerCredentialsAfterSuccess proves enroll
// delegates to the injected [BrokerCredentialReloader] — not just the store
// — after a successful credential creation. This is the property that makes
// a freshly-enrolled worker able to connect to a RUNNING broker without a
// restart; test/integration's broker-auth suite proves the real
// *server.Server implementation end to end against a live broker.
func TestWorkerEnroll_ReloadsBrokerCredentialsAfterSuccess(t *testing.T) {
	st := fake.New()
	reloader := &recordingReloader{}
	r := newWorkerEnrollRouterWith(st, storeRevoker{store: st}, reloader, true, time.Hour)
	raw, _ := seedJoinToken(t, st, nil)

	req := newReq(t, http.MethodPost, "/workers/enroll", jsonBody(t, workerEnrollRequest{
		JoinToken: raw, WorkerID: "w1", PublicKey: genPublicKey(t),
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — body: %s", rr.Code, rr.Body)
	}
	if reloader.calls != 1 {
		t.Errorf("reloader.calls = %d, want 1 — enroll must reload the broker's authorized-key set after creating the credential", reloader.calls)
	}
}

// TestWorkerEnroll_ReloadFailure_StillCreated pins the deliberate asymmetry
// with revoke: the credential is genuinely created and durable regardless of
// whether the broker reload succeeds, so a reload failure here is logged and
// swallowed, not turned into an error response — telling the caller
// enrollment failed would be false, and the worker can simply retry
// connecting once a later reload or restart picks up the row that already
// exists in the store.
func TestWorkerEnroll_ReloadFailure_StillCreated(t *testing.T) {
	st := fake.New()
	reloader := &recordingReloader{err: errors.New("broker not started")}
	r := newWorkerEnrollRouterWith(st, storeRevoker{store: st}, reloader, true, time.Hour)
	raw, _ := seedJoinToken(t, st, nil)

	req := newReq(t, http.MethodPost, "/workers/enroll", jsonBody(t, workerEnrollRequest{
		JoinToken: raw, WorkerID: "w1", PublicKey: genPublicKey(t),
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 even though the reload failed — body: %s", rr.Code, rr.Body)
	}
	if reloader.calls != 1 {
		t.Errorf("reloader.calls = %d, want 1", reloader.calls)
	}
	if _, err := st.GetActiveWorkerCredentialByWorkerID(t.Context(), "w1"); err != nil {
		t.Errorf("credential was not durably created despite the reload failure: %v", err)
	}
	if strings.Contains(rr.Body.String(), "broker not started") {
		t.Error("response leaks the underlying reload error; it must not appear given the 201 above")
	}
}

func TestWorkerEnroll_UnknownToken_Unauthorized(t *testing.T) {
	st := fake.New()
	r := newWorkerEnrollRouter(st, true, time.Hour)

	req := newReq(t, http.MethodPost, "/workers/enroll", jsonBody(t, workerEnrollRequest{
		JoinToken: "sqiw_does-not-exist", WorkerID: "w1", PublicKey: genPublicKey(t),
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — body: %s", rr.Code, rr.Body)
	}
}

func TestWorkerEnroll_ExpiredToken_Unauthorized(t *testing.T) {
	st := fake.New()
	r := newWorkerEnrollRouter(st, true, time.Hour)
	raw, _ := seedJoinToken(t, st, func(tok *store.WorkerJoinToken) {
		tok.ExpiresAt = time.Now().UTC().Add(-time.Minute)
	})

	req := newReq(t, http.MethodPost, "/workers/enroll", jsonBody(t, workerEnrollRequest{
		JoinToken: raw, WorkerID: "w1", PublicKey: genPublicKey(t),
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — body: %s", rr.Code, rr.Body)
	}
}

func TestWorkerEnroll_UsedSingleUseToken_Unauthorized(t *testing.T) {
	st := fake.New()
	r := newWorkerEnrollRouter(st, true, time.Hour)
	usedAt := time.Now().UTC().Add(-time.Minute)
	raw, _ := seedJoinToken(t, st, func(tok *store.WorkerJoinToken) {
		tok.UsedAt = &usedAt
	})

	req := newReq(t, http.MethodPost, "/workers/enroll", jsonBody(t, workerEnrollRequest{
		JoinToken: raw, WorkerID: "w1", PublicKey: genPublicKey(t),
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — body: %s", rr.Code, rr.Body)
	}
}

func TestWorkerEnroll_UsedTokenAllowedWhenNotSingleUse(t *testing.T) {
	st := fake.New()
	r := newWorkerEnrollRouter(st, false, time.Hour) // single-use OFF
	usedAt := time.Now().UTC().Add(-time.Minute)
	raw, _ := seedJoinToken(t, st, func(tok *store.WorkerJoinToken) {
		tok.UsedAt = &usedAt
	})

	req := newReq(t, http.MethodPost, "/workers/enroll", jsonBody(t, workerEnrollRequest{
		JoinToken: raw, WorkerID: "w1", PublicKey: genPublicKey(t),
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (single-use disabled, a used token is still accepted) — body: %s", rr.Code, rr.Body)
	}
}

func TestWorkerEnroll_WorkerIDAlreadyBoundToDifferentKey_Conflict(t *testing.T) {
	st := fake.New()
	r := newWorkerEnrollRouter(st, true, time.Hour)

	raw1, _ := seedJoinToken(t, st, nil)
	req1 := newReq(t, http.MethodPost, "/workers/enroll", jsonBody(t, workerEnrollRequest{
		JoinToken: raw1, WorkerID: "w1", PublicKey: genPublicKey(t),
	}))
	rr1 := httptest.NewRecorder()
	r.ServeHTTP(rr1, req1)
	if rr1.Code != http.StatusCreated {
		t.Fatalf("first enroll: status = %d, want 201 — body: %s", rr1.Code, rr1.Body)
	}

	raw2, _ := seedJoinToken(t, st, nil)
	req2 := newReq(t, http.MethodPost, "/workers/enroll", jsonBody(t, workerEnrollRequest{
		JoinToken: raw2, WorkerID: "w1", PublicKey: genPublicKey(t), // same worker, different key
	}))
	rr2 := httptest.NewRecorder()
	r.ServeHTTP(rr2, req2)

	if rr2.Code != http.StatusConflict {
		t.Fatalf("second enroll: status = %d, want 409 — body: %s", rr2.Code, rr2.Body)
	}
}

func TestWorkerEnroll_MalformedPublicKey_BadRequest(t *testing.T) {
	st := fake.New()
	r := newWorkerEnrollRouter(st, true, time.Hour)
	raw, _ := seedJoinToken(t, st, nil)

	req := newReq(t, http.MethodPost, "/workers/enroll", jsonBody(t, workerEnrollRequest{
		JoinToken: raw, WorkerID: "w1", PublicKey: "not-a-valid-key",
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — body: %s", rr.Code, rr.Body)
	}
}

func TestWorkerEnroll_MalformedJSON_BadRequest(t *testing.T) {
	st := fake.New()
	r := newWorkerEnrollRouter(st, true, time.Hour)

	req := newReq(t, http.MethodPost, "/workers/enroll", badJSON())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — body: %s", rr.Code, rr.Body)
	}
}

func TestWorkerEnroll_MissingFields_BadRequest(t *testing.T) {
	st := fake.New()
	r := newWorkerEnrollRouter(st, true, time.Hour)

	req := newReq(t, http.MethodPost, "/workers/enroll", jsonBody(t, workerEnrollRequest{WorkerID: "w1"}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — body: %s", rr.Code, rr.Body)
	}
}

// ── the non-negotiable: unknown/expired/used/store-failure are indistinguishable ──
//
// enroll is unauthenticated and may be internet-reachable, so every way a
// join token can fail to authorize a call must answer with the identical
// status AND body — never just the same status. TestWorkerEnroll_UnknownToken_
// Unauthorized and friends above already prove each individual case returns
// 401; this test is what actually pins the property those don't: that
// nothing distinguishes them from each other. A later change that makes any
// one path "more helpful" (e.g. naming which case it was) would pass every
// existing per-case test and only fail here.

// joinTokenLookupErrStore forces the join-token claim to fail with a
// non-ErrNotFound error, simulating a store outage while the token is being
// redeemed — the fourth way enroll can fail to authorize a request,
// alongside unknown, expired, and already-used. Both the single-use claim
// and the reusable-token lookup are overridden so the case holds whichever
// path a test exercises.
type joinTokenLookupErrStore struct {
	store.Store
}

func (joinTokenLookupErrStore) GetWorkerJoinTokenByHash(context.Context, string) (store.WorkerJoinToken, error) {
	return store.WorkerJoinToken{}, errors.New("simulated store outage")
}

func (joinTokenLookupErrStore) ConsumeWorkerJoinToken(context.Context, string, time.Time) (store.WorkerJoinToken, error) {
	return store.WorkerJoinToken{}, errors.New("simulated store outage")
}

func TestWorkerEnroll_TokenFailureModesAreIndistinguishable(t *testing.T) {
	pub := genPublicKey(t)
	usedAt := time.Now().UTC().Add(-time.Minute)

	// dispatch builds a fresh router over st (singleUse always on, so the
	// "already-used" case actually rejects) and returns the recorded
	// response to one enroll attempt with joinToken.
	dispatch := func(st store.Store, joinToken string) *httptest.ResponseRecorder {
		r := newWorkerEnrollRouter(st, true, time.Hour)
		req := newReq(t, http.MethodPost, "/workers/enroll", jsonBody(t, workerEnrollRequest{
			JoinToken: joinToken, WorkerID: "w1", PublicKey: pub,
		}))
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		return rr
	}

	cases := []struct {
		name string
		run  func() *httptest.ResponseRecorder
	}{
		{
			name: "unknown token",
			run: func() *httptest.ResponseRecorder {
				return dispatch(fake.New(), "sqiw_does-not-exist")
			},
		},
		{
			name: "expired token",
			run: func() *httptest.ResponseRecorder {
				st := fake.New()
				raw, _ := seedJoinToken(t, st, func(tok *store.WorkerJoinToken) {
					tok.ExpiresAt = time.Now().UTC().Add(-time.Minute)
				})
				return dispatch(st, raw)
			},
		},
		{
			name: "already-used single-use token",
			run: func() *httptest.ResponseRecorder {
				st := fake.New()
				raw, _ := seedJoinToken(t, st, func(tok *store.WorkerJoinToken) {
					tok.UsedAt = &usedAt
				})
				return dispatch(st, raw)
			},
		},
		{
			name: "store failure during lookup",
			run: func() *httptest.ResponseRecorder {
				return dispatch(joinTokenLookupErrStore{Store: fake.New()}, "sqiw_irrelevant-lookup-always-fails")
			},
		},
	}

	type outcome struct {
		name string
		code int
		body string
	}
	var got []outcome
	for _, tc := range cases {
		rr := tc.run()
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", tc.name, rr.Code)
		}
		got = append(got, outcome{name: tc.name, code: rr.Code, body: rr.Body.String()})
	}

	want := got[0]
	for _, g := range got[1:] {
		if g.code != want.code {
			t.Errorf("%s: status = %d, want %d (same as %q) — a caller could distinguish "+
				"join-token failure modes by status code, which the endpoint must never allow",
				g.name, g.code, want.code, want.name)
		}
		if g.body != want.body {
			t.Errorf("%s: body = %q, want %q (same as %q) — a caller could distinguish "+
				"join-token failure modes by response body, which the endpoint must never allow",
				g.name, g.body, want.body, want.name)
		}
	}
}

// ── router-level mounting gate ───────────────────────────────────────────────
//
// These use chi.Walk (via the liveRoutes/routeKey helpers from
// authz_integration_test.go) rather than firing an HTTP request: POST
// /workers/enroll and GET/DELETE /workers/{id} share the same two-segment
// shape, so when enroll is NOT mounted, a request for it is routed as
// /workers/{id} with id="enroll" — chi answers that with 405 (the path
// matches a registered pattern, just not for POST), not 404. Walking the
// route table sidesteps that collision and asserts what actually matters:
// whether the route is registered at all.

func TestWorkerEnroll_EndpointAbsentWhenEnrollmentEndpointDisabled(t *testing.T) {
	deps := Deps{
		Store:           fake.New(),
		Auth:            auth.Anonymous(),
		NATSAuthEnabled: true,
		// NATSAuthEnrollmentEndpointEnabled left false.
	}
	r := NewRouter(Config{DisableRateLimit: true}, deps, newTestLogger(), metrics.New(), health.NewRegistry())
	if liveRoutes(t, r)[routeKey{http.MethodPost, "/api/v1/workers/enroll"}] {
		t.Error("POST /api/v1/workers/enroll is registered even though the enrollment endpoint is disabled")
	}
}

func TestWorkerEnroll_EndpointAbsentWhenNATSAuthDisabled(t *testing.T) {
	deps := Deps{
		Store:                             fake.New(),
		Auth:                              auth.Anonymous(),
		NATSAuthEnabled:                   false,
		NATSAuthEnrollmentEndpointEnabled: true,
	}
	r := NewRouter(Config{DisableRateLimit: true}, deps, newTestLogger(), metrics.New(), health.NewRegistry())
	if liveRoutes(t, r)[routeKey{http.MethodPost, "/api/v1/workers/enroll"}] {
		t.Error("POST /api/v1/workers/enroll is registered even though broker (nats.auth) authentication is disabled")
	}
}

func TestWorkerEnroll_EndpointMountedWhenBothEnabled(t *testing.T) {
	deps := Deps{
		Store:                             fake.New(),
		Auth:                              auth.Anonymous(),
		NATSAuthEnabled:                   true,
		NATSAuthEnrollmentEndpointEnabled: true,
	}
	r := NewRouter(Config{DisableRateLimit: true}, deps, newTestLogger(), metrics.New(), health.NewRegistry())
	if !liveRoutes(t, r)[routeKey{http.MethodPost, "/api/v1/workers/enroll"}] {
		t.Error("POST /api/v1/workers/enroll is not registered even though both gating flags are true")
	}
}

// ── POST /workers/join-tokens ────────────────────────────────────────────────

func TestWorkerJoinTokenCreate_Created(t *testing.T) {
	st := fake.New()
	r := newWorkerEnrollRouter(st, true, time.Hour)

	req := newReq(t, http.MethodPost, "/workers/join-tokens", jsonBody(t, workerJoinTokenCreateRequest{Name: "batch-1"}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — body: %s", rr.Code, rr.Body)
	}
	var resp workerJoinTokenCreatedResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Token == "" {
		t.Fatal("response did not carry the raw token")
	}
	if resp.Name != "batch-1" {
		t.Errorf("name = %q, want batch-1", resp.Name)
	}

	stored, err := st.GetWorkerJoinTokenByHash(t.Context(), brokerauth.HashJoinToken(resp.Token))
	if err != nil {
		t.Fatalf("GetWorkerJoinTokenByHash: %v", err)
	}
	if stored.ID != resp.ID {
		t.Errorf("stored token id = %q, want %q", stored.ID, resp.ID)
	}
	if stored.TokenHash == resp.Token {
		t.Error("stored TokenHash equals the raw token — the raw value must never be persisted")
	}
}

func TestWorkerJoinTokenCreate_EmptyBody_Created(t *testing.T) {
	st := fake.New()
	r := newWorkerEnrollRouter(st, true, time.Hour)

	req := newReq(t, http.MethodPost, "/workers/join-tokens", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — body: %s", rr.Code, rr.Body)
	}
}

// ── DELETE /workers/{id}/credential ─────────────────────────────────────────

func TestWorkerCredentialRevoke_EnrolledWorker_NoContent(t *testing.T) {
	st := fake.New()
	r := newWorkerEnrollRouter(st, true, time.Hour)
	if _, err := st.CreateWorkerCredential(t.Context(), store.WorkerCredential{
		ID: uuid.NewString(), WorkerID: "w1", PublicKey: genPublicKey(t), EnrolledAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed CreateWorkerCredential: %v", err)
	}

	req := newReq(t, http.MethodDelete, "/workers/w1/credential", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 — body: %s", rr.Code, rr.Body)
	}

	if _, err := st.GetActiveWorkerCredentialByWorkerID(t.Context(), "w1"); err == nil {
		t.Error("credential still active after revoke")
	} else if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetActiveWorkerCredentialByWorkerID after revoke: %v, want store.ErrNotFound", err)
	}
}

func TestWorkerCredentialRevoke_UnknownWorker_NotFound(t *testing.T) {
	st := fake.New()
	r := newWorkerEnrollRouter(st, true, time.Hour)

	req := newReq(t, http.MethodDelete, "/workers/does-not-exist/credential", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — body: %s", rr.Code, rr.Body)
	}
	// The response must not claim the worker doesn't exist — that case is
	// indistinguishable from "already revoked" at the store layer.
	if strings.Contains(rr.Body.String(), "does not exist") {
		t.Error("response claims the worker does not exist, which the store cannot actually distinguish from already-revoked")
	}
}

// ── revoke delegates to the injected WorkerRevoker ──────────────────────────

// recordingRevoker is a [WorkerRevoker] stub that records the workerID it
// was called with and returns a configurable error, so a test can prove the
// handler actually calls through the injected interface — not the store
// directly — and that it maps a non-[store.ErrNotFound] failure to 500
// rather than leaking the underlying error text (which, in production, would
// be a broker-internal detail like a reload failure).
type recordingRevoker struct {
	calledWith string
	err        error
}

func (r *recordingRevoker) RevokeWorker(_ context.Context, workerID string) error {
	r.calledWith = workerID
	return r.err
}

func TestWorkerCredentialRevoke_DelegatesToInjectedRevoker(t *testing.T) {
	st := fake.New()
	rev := &recordingRevoker{}
	r := newWorkerEnrollRouterWithRevoker(st, rev, true, time.Hour)

	req := newReq(t, http.MethodDelete, "/workers/w1/credential", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 — body: %s", rr.Code, rr.Body)
	}
	if rev.calledWith != "w1" {
		t.Errorf("revoker called with %q, want %q — the handler must delegate to the injected WorkerRevoker, not write the store directly",
			rev.calledWith, "w1")
	}
}

func TestWorkerCredentialRevoke_RevokerErrorNotFound_NotFound(t *testing.T) {
	st := fake.New()
	rev := &recordingRevoker{err: store.ErrNotFound}
	r := newWorkerEnrollRouterWithRevoker(st, rev, true, time.Hour)

	req := newReq(t, http.MethodDelete, "/workers/does-not-exist/credential", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — body: %s", rr.Code, rr.Body)
	}
}

func TestWorkerCredentialRevoke_RevokerOtherError_InternalServerError(t *testing.T) {
	st := fake.New()
	rev := &recordingRevoker{err: errors.New("broker reload failed")}
	r := newWorkerEnrollRouterWithRevoker(st, rev, true, time.Hour)

	req := newReq(t, http.MethodDelete, "/workers/w1/credential", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 — body: %s", rr.Code, rr.Body)
	}
	if strings.Contains(rr.Body.String(), "broker reload failed") {
		t.Error("response leaks the underlying revoker error; expected the generic message")
	}
}

// ── single-use redemption is atomic ─────────────────────────────────────────

// TestWorkerEnroll_ConcurrentRedemptionsOfOneSingleUseToken proves the
// redemption is a claim, not a check followed by a claim. Reading the token,
// inspecting UsedAt and marking it used as separate steps lets two
// simultaneous enrollments with one single-use token BOTH observe UsedAt as
// nil and both succeed — the token's whole purpose defeated by timing
// alone. store.ConsumeWorkerJoinToken makes check and claim one statement,
// so exactly one of these can win.
func TestWorkerEnroll_ConcurrentRedemptionsOfOneSingleUseToken(t *testing.T) {
	// Repeated rounds, not one: the handler is fast enough that a single
	// burst can happen to serialize even under -race, so one round proves
	// nothing about a check-then-act implementation. Every round must yield
	// exactly one 201.
	const rounds = 50
	const attempts = 8

	for round := range rounds {
		st := fake.New()
		r := newWorkerEnrollRouter(st, true, time.Hour)
		raw, _ := seedJoinToken(t, st, nil)

		// Build every request up front so the goroutines do nothing but
		// serve once released.
		reqs := make([]*http.Request, attempts)
		for i := range reqs {
			reqs[i] = newReq(t, http.MethodPost, "/workers/enroll", jsonBody(t, workerEnrollRequest{
				JoinToken: raw,
				WorkerID:  fmt.Sprintf("w%d", i),
				PublicKey: genPublicKey(t),
			}))
		}

		codes := make([]int, attempts)
		var ready, done sync.WaitGroup
		ready.Add(attempts)
		done.Add(attempts)
		start := make(chan struct{})
		for i := range attempts {
			go func() {
				defer done.Done()
				rr := httptest.NewRecorder()
				ready.Done()
				<-start
				r.ServeHTTP(rr, reqs[i])
				codes[i] = rr.Code
			}()
		}
		ready.Wait()
		close(start)
		done.Wait()

		created := 0
		for i, code := range codes {
			switch code {
			case http.StatusCreated:
				created++
			case http.StatusUnauthorized:
			default:
				t.Fatalf("round %d attempt %d: status = %d, want 201 or 401", round, i, code)
			}
		}
		if created != 1 {
			t.Fatalf("round %d: %d of %d concurrent enrollments succeeded with one single-use token, want exactly 1",
				round, created, attempts)
		}
	}
}

// TestWorkerEnroll_SingleUseTokenIsSpentBeforeTheCredentialIsCreated pins the
// deliberate cost of that atomicity: the claim has to precede the credential
// write, so an enrollment rejected for a conflicting worker ID has already
// spent the token and the operator issues a new one. A malformed public key
// costs nothing, because it is rejected before the claim.
func TestWorkerEnroll_SingleUseTokenIsSpentBeforeTheCredentialIsCreated(t *testing.T) {
	st := fake.New()
	r := newWorkerEnrollRouter(st, true, time.Hour)

	// Seed an existing active credential so the enrollment below conflicts.
	if _, err := st.CreateWorkerCredential(t.Context(), store.WorkerCredential{
		ID: uuid.NewString(), WorkerID: "w1", PublicKey: genPublicKey(t), EnrolledAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed CreateWorkerCredential: %v", err)
	}

	rawBadKey, badKeyTok := seedJoinToken(t, st, nil)
	req := newReq(t, http.MethodPost, "/workers/enroll", jsonBody(t, workerEnrollRequest{
		JoinToken: rawBadKey, WorkerID: "w2", PublicKey: "not-a-valid-key",
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed key: status = %d, want 400 — body: %s", rr.Code, rr.Body)
	}
	stored, err := st.GetWorkerJoinTokenByHash(t.Context(), badKeyTok.TokenHash)
	if err != nil {
		t.Fatalf("GetWorkerJoinTokenByHash: %v", err)
	}
	if stored.UsedAt != nil {
		t.Error("a malformed public key spent the join token; it must be rejected before the token is claimed")
	}

	rawConflict, conflictTok := seedJoinToken(t, st, nil)
	req = newReq(t, http.MethodPost, "/workers/enroll", jsonBody(t, workerEnrollRequest{
		JoinToken: rawConflict, WorkerID: "w1", PublicKey: genPublicKey(t),
	}))
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("conflicting worker id: status = %d, want 409 — body: %s", rr.Code, rr.Body)
	}
	stored, err = st.GetWorkerJoinTokenByHash(t.Context(), conflictTok.TokenHash)
	if err != nil {
		t.Fatalf("GetWorkerJoinTokenByHash: %v", err)
	}
	if stored.UsedAt == nil {
		t.Error("the join token was not spent by a conflicting enrollment — the claim must be atomic, " +
			"which means it precedes the credential write and cannot be undone by its failure")
	}
}

// ── worker_id must be a single NATS subject token ───────────────────────────

// TestWorkerEnroll_InvalidWorkerID_BadRequest is the enrollment boundary's
// half of the branch's whole premise. The stored worker_id flows verbatim
// into brokerauth.WorkerPermissions when the broker builds its key set, and
// those grants are NATS subject PATTERNS. A worker_id of "*" therefore mints
// a credential granted "task.status.*.*", "worker.deregister.*",
// "work.lease.*.*" and the rest — one credential that may publish concrete
// subjects belonging to ANY worker: forge status and logs, deregister the
// farm, and lease work as another worker, receiving that worker's assignment
// batch in its own inbox. The scheduler's provenance checks cannot catch it,
// because the subject NATS vouches for genuinely names the victim.
//
// ">" is worse-shaped still: it yields the malformed "task.status.>.*"
// inside Options.Nkeys, which can make every later ReloadOptions fail —
// revocation permanently 500ing — or wedge the broker at boot.
func TestWorkerEnroll_InvalidWorkerID_BadRequest(t *testing.T) {
	cases := []struct {
		name     string
		workerID string
	}{
		{"single-token wildcard", "*"},
		{"multi-token wildcard", ">"},
		{"contains a dot", "render.01"},
		{"contains whitespace", "render 01"},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := fake.New()
			r := newWorkerEnrollRouter(st, true, time.Hour)
			raw, tok := seedJoinToken(t, st, nil)

			req := newReq(t, http.MethodPost, "/workers/enroll", jsonBody(t, workerEnrollRequest{
				JoinToken: raw, WorkerID: tc.workerID, PublicKey: genPublicKey(t),
			}))
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("worker_id %q: status = %d, want 400 — body: %s", tc.workerID, rr.Code, rr.Body)
			}

			// Rejected before the claim, so the operator's token survives.
			stored, err := st.GetWorkerJoinTokenByHash(t.Context(), tok.TokenHash)
			if err != nil {
				t.Fatalf("GetWorkerJoinTokenByHash: %v", err)
			}
			if stored.UsedAt != nil {
				t.Error("an invalid worker_id spent the join token; it must be rejected before the token is claimed")
			}

			// And nothing was enrolled under it.
			creds, err := st.ListActiveWorkerCredentials(t.Context())
			if err != nil {
				t.Fatalf("ListActiveWorkerCredentials: %v", err)
			}
			if len(creds) != 0 {
				t.Errorf("credential created for invalid worker_id %q: %+v", tc.workerID, creds)
			}
		})
	}
}
