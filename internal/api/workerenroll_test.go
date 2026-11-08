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
	"github.com/uberware/sqi/internal/auth/jointoken"
	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/health"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/store/storetest"
)

// ── router helper ────────────────────────────────────────────────────────────

// storeRevoker adapts a store.Store to [WorkerRevoker] by writing the
// credential row and nothing else, with no broker disconnect. The disconnect
// belongs to *server.Server, which this package cannot import because
// internal/server imports internal/api, and test/integration's broker-auth
// suite covers it. newWorkerEnrollRouter uses this revoker.
type storeRevoker struct{ store store.WorkerCredentialStore }

func (r storeRevoker) RevokeWorker(ctx context.Context, workerID string) error {
	return r.store.RevokeWorkerCredential(ctx, workerID, time.Now().UTC())
}

func newWorkerEnrollRouter(st store.Store, singleUse bool, ttl time.Duration) chi.Router {
	return newWorkerEnrollRouterWith(st, storeRevoker{store: st}, singleUse, ttl)
}

func newWorkerEnrollRouterWithRevoker(st store.Store, revoker WorkerRevoker, singleUse bool, ttl time.Duration) chi.Router {
	return newWorkerEnrollRouterWith(st, revoker, singleUse, ttl)
}

func newWorkerEnrollRouterWith(st store.Store, revoker WorkerRevoker, singleUse bool, ttl time.Duration) chi.Router {
	h := newWorkerEnrollHandler(st, revoker, newTestLogger(), singleUse, ttl)
	r := chi.NewRouter()
	r.Post("/workers/enroll", h.enroll)
	r.Post("/workers/join-tokens", h.createJoinToken)
	r.Delete("/workers/{id}/credential", h.revokeCredential)
	return r
}

// ── seed helpers ─────────────────────────────────────────────────────────────

// seedJoinToken creates a join token directly in the store, bypassing the
// mint handler, and returns the raw token alongside the stored record.
// mutate, when non-nil, runs after the default fields are set so a test can
// override ExpiresAt or UsedAt to exercise the reject paths.
func seedJoinToken(t *testing.T, st store.Store, mutate func(*store.WorkerJoinToken)) (raw string, tok store.WorkerJoinToken) {
	t.Helper()
	raw, hash, prefix, err := jointoken.Generate()
	if err != nil {
		t.Fatalf("jointoken.Generate: %v", err)
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

	cred, err := storetest.ActiveWorkerCredential(t.Context(), st, "w1")
	if err != nil {
		t.Fatalf("ActiveWorkerCredential: %v", err)
	}
	if cred.PublicKey != pub {
		t.Errorf("stored credential public key = %q, want %q", cred.PublicKey, pub)
	}

	// The token must never be echoed back in the response.
	if strings.Contains(rr.Body.String(), raw) {
		t.Error("response body echoes the raw join token")
	}
}

// failingRevoker is a [WorkerRevoker] that fails the test if it is ever called.
type failingRevoker struct{ t *testing.T }

func (r failingRevoker) RevokeWorker(context.Context, string) error {
	r.t.Helper()
	r.t.Error("enrollment called the revoker; it must not touch the broker")
	return nil
}

// TestEnroll_DoesNotCallBroker checks that enrollment completes with no broker
// call: the handler gets a revoker that must stay unused, and the credential
// is created and durable. The broker reads the credential from the store when
// the worker connects; test/integration's
// TestEnrollment_ConnectsToRunningBrokerWithoutRestart checks that against a
// live broker.
func TestEnroll_DoesNotCallBroker(t *testing.T) {
	st := fake.New()
	r := newWorkerEnrollRouterWith(st, failingRevoker{t: t}, true, time.Hour)
	raw, _ := seedJoinToken(t, st, nil)

	req := newReq(t, http.MethodPost, "/workers/enroll", jsonBody(t, workerEnrollRequest{
		JoinToken: raw, WorkerID: "w1", PublicKey: genPublicKey(t),
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — body: %s", rr.Code, rr.Body)
	}
	if _, err := storetest.ActiveWorkerCredential(t.Context(), st, "w1"); err != nil {
		t.Errorf("credential was not durably created: %v", err)
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
	r := newWorkerEnrollRouter(st, false, time.Hour) // single-use off
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

// ── unknown, expired, used and store-failure tokens are indistinguishable ──
//
// enroll is unauthenticated and may be internet-reachable, so every way a
// join token can fail to authorize a call must answer with the identical
// status and body. TestWorkerEnroll_UnknownToken_Unauthorized and the tests
// beside it check that each case returns 401; the test below checks that
// nothing distinguishes the cases from each other, which those per-case tests
// cannot.

// joinTokenLookupErrStore forces the join-token claim to fail with a
// non-ErrNotFound error, simulating a store outage while the token is being
// redeemed: the fourth way enroll can fail to authorize a request, alongside
// unknown, expired, and already-used. Both the single-use claim and the
// reusable-token lookup are overridden so the case holds on either path.
type joinTokenLookupErrStore struct {
	store.Store
}

func (joinTokenLookupErrStore) GetWorkerJoinTokenByHash(context.Context, string) (store.WorkerJoinToken, error) {
	return store.WorkerJoinToken{}, errors.New("simulated store outage")
}

func (joinTokenLookupErrStore) RedeemWorkerJoinToken(context.Context, string, time.Time, store.WorkerCredential) (store.WorkerCredential, error) {
	return store.WorkerCredential{}, errors.New("simulated store outage")
}

func TestWorkerEnroll_TokenFailureModesAreIndistinguishable(t *testing.T) {
	pub := genPublicKey(t)
	usedAt := time.Now().UTC().Add(-time.Minute)

	// dispatch builds a fresh router over st (single-use always on, so the
	// "already-used" case rejects) and returns the recorded response to one
	// enroll attempt with joinToken.
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
			t.Errorf("%s: status = %d, want %d (same as %q); a caller could distinguish "+
				"join-token failure modes by status code, which the endpoint must never allow",
				g.name, g.code, want.code, want.name)
		}
		if g.body != want.body {
			t.Errorf("%s: body = %q, want %q (same as %q); a caller could distinguish "+
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
// shape, so when enroll is not mounted, a request for it is routed as
// /workers/{id} with id="enroll" and chi answers 405 (the path matches a
// registered pattern, just not for POST), not 404. Walking the route table
// avoids that collision and asserts whether the route is registered.

func TestWorkerEnroll_EndpointAbsentWhenEnrollmentEndpointDisabled(t *testing.T) {
	deps := Deps{
		Store:           fake.New(),
		Auth:            auth.Anonymous(),
		NATSAuthEnabled: true,
		// NATSAuthEnrollmentEndpointEnabled is left false.
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

	stored, err := st.GetWorkerJoinTokenByHash(t.Context(), jointoken.Hash(resp.Token))
	if err != nil {
		t.Fatalf("GetWorkerJoinTokenByHash: %v", err)
	}
	if stored.ID != resp.ID {
		t.Errorf("stored token id = %q, want %q", stored.ID, resp.ID)
	}
	if stored.TokenHash == resp.Token {
		t.Error("stored TokenHash equals the raw token; the raw value must never be persisted")
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

	if _, err := storetest.ActiveWorkerCredential(t.Context(), st, "w1"); err == nil {
		t.Error("credential still active after revoke")
	} else if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("ActiveWorkerCredential after revoke: %v, want store.ErrNotFound", err)
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
	// The response must not claim the worker does not exist: the store cannot
	// distinguish that case from "already revoked".
	if strings.Contains(rr.Body.String(), "does not exist") {
		t.Error("response claims the worker does not exist, which the store cannot actually distinguish from already-revoked")
	}
}

// ── revoke delegates to the injected WorkerRevoker ──────────────────────────

// recordingRevoker is a [WorkerRevoker] stub that records the workerID it
// was called with and returns a configurable error, so a test can check that
// the handler calls through the injected interface rather than the store, and
// that it maps a non-[store.ErrNotFound] failure to 500 without leaking the
// underlying error text (in production, a broker-internal detail).
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
		t.Errorf("revoker called with %q, want %q; the handler must delegate to the injected WorkerRevoker, not write the store directly",
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
	rev := &recordingRevoker{err: errors.New("broker disconnect failed")}
	r := newWorkerEnrollRouterWithRevoker(st, rev, true, time.Hour)

	req := newReq(t, http.MethodDelete, "/workers/w1/credential", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 — body: %s", rr.Code, rr.Body)
	}
	if strings.Contains(rr.Body.String(), "broker disconnect failed") {
		t.Error("response leaks the underlying revoker error; expected the generic message")
	}
}

// ── single-use redemption is atomic ─────────────────────────────────────────

// TestWorkerEnroll_ConcurrentRedemptionsOfOneSingleUseToken checks that
// redemption is a single claim, not a check followed by a claim. Reading the
// token, inspecting UsedAt and marking it used as separate steps would let two
// simultaneous enrollments with one single-use token both observe UsedAt as
// nil and both succeed. store.RedeemWorkerJoinToken makes check and claim one
// statement, inside one transaction with the credential creation, so exactly
// one enrollment wins.
func TestWorkerEnroll_ConcurrentRedemptionsOfOneSingleUseToken(t *testing.T) {
	// Several rounds, because a single burst can serialize even under -race
	// and then says nothing about a check-then-act implementation. Every
	// round must yield exactly one 201.
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

// TestWorkerEnroll_MalformedPublicKey_TokenNotSpent checks that a request
// rejected before the token is claimed leaves the token redeemable: a
// malformed public key is caught by validation, ahead of any store call.
func TestWorkerEnroll_MalformedPublicKey_TokenNotSpent(t *testing.T) {
	st := fake.New()
	r := newWorkerEnrollRouter(st, true, time.Hour)

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
}

// TestWorkerEnroll_ConflictingEnrollment_TokenSurvives checks that a
// single-use token presented in a request that conflicts (here, a worker ID
// that already has an active credential) is not spent. The claim and the
// credential creation are one transaction (store.RedeemWorkerJoinToken), so a
// conflict rolls the claim back along with the credential write. The test
// redeems the same token afterwards, for a different worker ID.
func TestWorkerEnroll_ConflictingEnrollment_TokenSurvives(t *testing.T) {
	st := fake.New()
	r := newWorkerEnrollRouter(st, true, time.Hour)
	raw, _ := seedJoinToken(t, st, nil)

	// The first enrollment for w1 is the legitimate use of this token.
	req := newReq(t, http.MethodPost, "/workers/enroll", jsonBody(t, workerEnrollRequest{
		JoinToken: raw, WorkerID: "w1", PublicKey: genPublicKey(t),
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("first enrollment: status = %d, want 201 — body: %s", rr.Code, rr.Body)
	}

	// A single-use token is spent by its first successful redemption, so a
	// second attempt with the same token, even one that would also conflict
	// on worker ID w1, answers the generic invalid-token 401 rather than a
	// 409. The case under test is a credential write that fails for a reason
	// other than the token being spent (a worker ID bound to an active
	// credential from a different token's redemption), so seed a second
	// token for that attempt.
	raw2, _ := seedJoinToken(t, st, nil)
	req = newReq(t, http.MethodPost, "/workers/enroll", jsonBody(t, workerEnrollRequest{
		JoinToken: raw2, WorkerID: "w1", PublicKey: genPublicKey(t),
	}))
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("conflicting worker id: status = %d, want 409 — body: %s", rr.Code, rr.Body)
	}

	// raw2 must still be redeemable: the conflict rolled its claim back.
	// Redeem it for a different worker ID.
	req = newReq(t, http.MethodPost, "/workers/enroll", jsonBody(t, workerEnrollRequest{
		JoinToken: raw2, WorkerID: "w3", PublicKey: genPublicKey(t),
	}))
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("re-redemption after a conflict: status = %d, want 201 — body: %s", rr.Code, rr.Body)
	}
}

// ── worker_id must be a single NATS subject token ───────────────────────────

// TestWorkerEnroll_InvalidWorkerID_BadRequest checks the enrollment
// boundary's worker_id validation. The stored worker_id goes verbatim into
// brokerauth.WorkerPermissions, and those grants are NATS subject patterns. A
// worker_id of "*" would mint a credential granted "task.status.*.*",
// "worker.deregister.*", "work.lease.*.*" and the rest: one credential that
// may publish concrete subjects belonging to any worker, forging status and
// logs, deregistering the farm, and leasing work as another worker. The
// scheduler's provenance checks cannot catch it, because the subject NATS
// vouches for names the victim.
//
// ">" yields the malformed "task.status.>.*" subject, which the broker can
// reject, breaking credential changes such as revocation.
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

			// Nothing was enrolled under it.
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

// ── capacity-group join tokens ───────────────────────────────────────────────

// seedGroupToken creates a capacity group and a join token bound to it.
func seedGroupToken(t *testing.T, st store.Store, mutate func(*store.WorkerJoinToken)) (raw string, group store.CapacityGroup, tok store.WorkerJoinToken) {
	t.Helper()
	farm, err := st.CreateFarm(t.Context(), store.Farm{ID: uuid.NewString(), Name: "farm-" + uuid.NewString()})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	group, err = st.CreateCapacityGroup(t.Context(), store.CapacityGroup{
		ID: uuid.NewString(), FarmID: farm.ID, Name: "group-" + uuid.NewString(),
		Kind: store.CapacityGroupNamed, Provider: store.CapacityProviderExternal,
		Enabled: true, MaxMachines: 10, MaxStepUp: 1,
		IdleTimeout: time.Hour, BootTimeout: time.Hour, RetireAction: store.CapacityRetireSleep,
	})
	if err != nil {
		t.Fatalf("CreateCapacityGroup: %v", err)
	}
	raw, tok = seedJoinToken(t, st, func(tok *store.WorkerJoinToken) {
		tok.CapacityGroupID = group.ID
		if mutate != nil {
			mutate(tok)
		}
	})
	return raw, group, tok
}

func postEnroll(t *testing.T, r http.Handler, raw, workerID string) *httptest.ResponseRecorder {
	t.Helper()
	req := newReq(t, http.MethodPost, "/workers/enroll", jsonBody(t, workerEnrollRequest{
		JoinToken: raw, WorkerID: workerID, PublicKey: genPublicKey(t),
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func TestEnroll_GroupTokenIsReusableAndTagsCredential(t *testing.T) {
	for _, singleUse := range []bool{true, false} {
		t.Run(fmt.Sprintf("singleUse=%v", singleUse), func(t *testing.T) {
			st := fake.New()
			r := newWorkerEnrollRouter(st, singleUse, time.Hour)
			raw, group, _ := seedGroupToken(t, st, nil)

			for _, id := range []string{"w1", "w2"} {
				if rr := postEnroll(t, r, raw, id); rr.Code != http.StatusCreated {
					t.Fatalf("enroll %s: status = %d, want 201 — body: %s", id, rr.Code, rr.Body)
				}
				cred, err := storetest.ActiveWorkerCredential(t.Context(), st, id)
				if err != nil {
					t.Fatalf("ActiveWorkerCredential %s: %v", id, err)
				}
				if cred.CapacityGroupID != group.ID {
					t.Errorf("credential %s group = %q, want %q", id, cred.CapacityGroupID, group.ID)
				}
			}
		})
	}
}

func TestEnroll_UngroupedTokenCredentialHasNoGroup(t *testing.T) {
	st := fake.New()
	r := newWorkerEnrollRouter(st, true, time.Hour)
	raw, _ := seedJoinToken(t, st, nil)
	if rr := postEnroll(t, r, raw, "w1"); rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — body: %s", rr.Code, rr.Body)
	}
	cred, err := storetest.ActiveWorkerCredential(t.Context(), st, "w1")
	if err != nil || cred.CapacityGroupID != "" {
		t.Fatalf("credential = (%+v, %v), want no group", cred, err)
	}
}

func TestEnroll_RevokedGroupTokenRefused(t *testing.T) {
	for _, singleUse := range []bool{true, false} {
		t.Run(fmt.Sprintf("singleUse=%v", singleUse), func(t *testing.T) {
			st := fake.New()
			r := newWorkerEnrollRouter(st, singleUse, time.Hour)
			raw, _, tok := seedGroupToken(t, st, nil)

			if rr := postEnroll(t, r, raw, "w1"); rr.Code != http.StatusCreated {
				t.Fatalf("enroll before revoke: status = %d, want 201 — body: %s", rr.Code, rr.Body)
			}
			if err := st.RevokeCapacityGroupToken(t.Context(), tok.CapacityGroupID, tok.ID, time.Now().UTC()); err != nil {
				t.Fatalf("RevokeCapacityGroupToken: %v", err)
			}
			rr := postEnroll(t, r, raw, "w2")
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("enroll after revoke: status = %d, want 401 — body: %s", rr.Code, rr.Body)
			}
			if _, err := storetest.ActiveWorkerCredential(t.Context(), st, "w2"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("a credential exists for the refused enrollment: err = %v", err)
			}

			// The refusal is the one an expired token gets.
			expRaw, _, _ := seedGroupToken(t, st, func(tok *store.WorkerJoinToken) {
				tok.ExpiresAt = time.Now().UTC().Add(-time.Minute)
			})
			exp := postEnroll(t, r, expRaw, "w3")
			if exp.Code != rr.Code || exp.Body.String() != rr.Body.String() {
				t.Errorf("expired = %d %s, revoked = %d %s: want identical", exp.Code, exp.Body, rr.Code, rr.Body)
			}
		})
	}
}

func TestEnroll_RevokedUngroupedTokenRefused(t *testing.T) {
	for _, singleUse := range []bool{true, false} {
		t.Run(fmt.Sprintf("singleUse=%v", singleUse), func(t *testing.T) {
			st := fake.New()
			r := newWorkerEnrollRouter(st, singleUse, time.Hour)
			revokedAt := time.Now().UTC().Add(-time.Minute)
			raw, _ := seedJoinToken(t, st, func(tok *store.WorkerJoinToken) { tok.RevokedAt = &revokedAt })
			if rr := postEnroll(t, r, raw, "w1"); rr.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 — body: %s", rr.Code, rr.Body)
			}
		})
	}
}

func TestEnroll_GroupTokenConflictKeepsToken(t *testing.T) {
	st := fake.New()
	r := newWorkerEnrollRouter(st, true, time.Hour)
	raw, _, _ := seedGroupToken(t, st, nil)
	if rr := postEnroll(t, r, raw, "w1"); rr.Code != http.StatusCreated {
		t.Fatalf("first enroll: status = %d — body: %s", rr.Code, rr.Body)
	}
	if rr := postEnroll(t, r, raw, "w1"); rr.Code != http.StatusConflict {
		t.Fatalf("same worker again: status = %d, want 409 — body: %s", rr.Code, rr.Body)
	}
	if rr := postEnroll(t, r, raw, "w2"); rr.Code != http.StatusCreated {
		t.Fatalf("another worker: status = %d, want 201 — body: %s", rr.Code, rr.Body)
	}
}

// A group token's hash stays Go-side: nothing the enrollment or minting
// handlers return carries it.
func TestEnroll_ResponsesNeverCarryTokenHash(t *testing.T) {
	st := fake.New()
	r := newWorkerEnrollRouter(st, true, time.Hour)
	raw, _, tok := seedGroupToken(t, st, nil)
	rr := postEnroll(t, r, raw, "w1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d — body: %s", rr.Code, rr.Body)
	}
	if strings.Contains(rr.Body.String(), tok.TokenHash) {
		t.Error("enroll response carries the token hash")
	}
}
