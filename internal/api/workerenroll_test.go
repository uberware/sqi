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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

func newWorkerEnrollRouter(st store.Store, singleUse bool, ttl time.Duration) chi.Router {
	h := newWorkerEnrollHandler(st, newTestLogger(), singleUse, ttl)
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
