// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Unit tests for queue REST handlers.
//
// Coverage targets:
//   POST   /api/v1/queues      — createQueue (validation, conflict, store error)
//   GET    /api/v1/queues      — listQueues  (success, store error, paused filter)
//   GET    /api/v1/queues/{id} — getQueue    (found, not found, store error)
//   PUT    /api/v1/queues/{id} — updateQueue (success, validation, not found, conflict)
//   DELETE /api/v1/queues/{id} — deleteQueue (success, not found, store error)

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// ── router helper ─────────────────────────────────────────────────────────────

func newQueueRouter(st store.Store) chi.Router {
	h := newQueueHandler(st, newTestLogger())
	r := chi.NewRouter()
	r.Post("/api/v1/queues", h.createQueue)
	r.Get("/api/v1/queues", h.listQueues)
	r.Get("/api/v1/queues/{id}", h.getQueue)
	r.Put("/api/v1/queues/{id}", h.updateQueue)
	r.Delete("/api/v1/queues/{id}", h.deleteQueue)
	return r
}

// seedFarmAndQueue creates a farm and a queue in st, returning both.
func seedFarmAndQueue(t *testing.T, st *fake.Store) (store.Farm, store.Queue) {
	t.Helper()
	ctx := t.Context()
	now := time.Now()

	farm := store.Farm{ID: uuid.NewString(), Name: "farm-q-" + uuid.NewString(), CreatedAt: now, UpdatedAt: now}
	f, err := st.CreateFarm(ctx, farm)
	if err != nil {
		t.Fatalf("seedFarmAndQueue: CreateFarm: %v", err)
	}

	q := store.Queue{
		ID: uuid.NewString(), FarmID: f.ID, Name: "queue-" + uuid.NewString(),
		Priority: 10, CreatedAt: now, UpdatedAt: now,
	}
	created, err := st.CreateQueue(ctx, q)
	if err != nil {
		t.Fatalf("seedFarmAndQueue: CreateQueue: %v", err)
	}
	return f, created
}

// ── createQueue ───────────────────────────────────────────────────────────────

func TestCreateQueue_MissingFarmID(t *testing.T) {
	st := fake.New()
	defer st.Close()
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPost, "/api/v1/queues", jsonBody(t, map[string]any{
		"name": "q1",
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestCreateQueue_MissingName(t *testing.T) {
	st := fake.New()
	defer st.Close()
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPost, "/api/v1/queues", jsonBody(t, map[string]any{
		"farm_id": "some-id",
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestCreateQueue_FarmNotFound(t *testing.T) {
	st := fake.New()
	defer st.Close()
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPost, "/api/v1/queues", jsonBody(t, map[string]any{
		"farm_id": "nonexistent",
		"name":    "q1",
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestCreateQueue_Conflict(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm, q := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPost, "/api/v1/queues", jsonBody(t, map[string]any{
		"farm_id": farm.ID,
		"name":    q.Name, // duplicate name in same farm
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", rr.Code)
	}
}

func TestCreateQueue_InvalidJSON(t *testing.T) {
	st := fake.New()
	defer st.Close()
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPost, "/api/v1/queues", badJSON())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestCreateQueue_Success(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm, _ := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPost, "/api/v1/queues", jsonBody(t, map[string]any{
		"farm_id": farm.ID,
		"name":    "brand-new-queue",
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestCreateQueue_PersistsRetryPolicy verifies that max_attempts,
// retry_delay_seconds, and failure_limit are threaded through to the
// persisted queue and echoed in the create response.
func TestCreateQueue_PersistsRetryPolicy(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm, _ := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPost, "/api/v1/queues", jsonBody(t, map[string]any{
		"farm_id":             farm.ID,
		"name":                "retry-policy-queue",
		"max_attempts":        4,
		"retry_delay_seconds": 15,
		"failure_limit":       10,
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["max_attempts"] != float64(4) {
		t.Errorf("max_attempts = %v, want 4", resp["max_attempts"])
	}
	if resp["retry_delay_seconds"] != float64(15) {
		t.Errorf("retry_delay_seconds = %v, want 15", resp["retry_delay_seconds"])
	}
	if resp["failure_limit"] != float64(10) {
		t.Errorf("failure_limit = %v, want 10", resp["failure_limit"])
	}

	id, ok := resp["id"].(string)
	if !ok {
		t.Fatalf("id not a string: %v", resp["id"])
	}
	stored, err := st.GetQueue(t.Context(), id)
	if err != nil {
		t.Fatalf("GetQueue: %v", err)
	}
	if stored.MaxAttempts == nil || *stored.MaxAttempts != 4 {
		t.Errorf("stored max_attempts = %v, want 4", stored.MaxAttempts)
	}
	if stored.RetryDelaySeconds == nil || *stored.RetryDelaySeconds != 15 {
		t.Errorf("stored retry_delay_seconds = %v, want 15", stored.RetryDelaySeconds)
	}
	if stored.FailureLimit == nil || *stored.FailureLimit != 10 {
		t.Errorf("stored failure_limit = %v, want 10", stored.FailureLimit)
	}
}

// TestUpdateQueue_PersistsRetryPolicy verifies that PUT updates the retry
// policy overrides and echoes them in the response.
func TestUpdateQueue_PersistsRetryPolicy(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm, q := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPut, "/api/v1/queues/"+q.ID, jsonBody(t, map[string]any{
		"farm_id":             farm.ID,
		"name":                q.Name,
		"max_attempts":        7,
		"retry_delay_seconds": 30,
		"failure_limit":       40,
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["max_attempts"] != float64(7) {
		t.Errorf("max_attempts = %v, want 7", resp["max_attempts"])
	}
	if resp["retry_delay_seconds"] != float64(30) {
		t.Errorf("retry_delay_seconds = %v, want 30", resp["retry_delay_seconds"])
	}
	if resp["failure_limit"] != float64(40) {
		t.Errorf("failure_limit = %v, want 40", resp["failure_limit"])
	}

	stored, err := st.GetQueue(t.Context(), q.ID)
	if err != nil {
		t.Fatalf("GetQueue: %v", err)
	}
	if stored.MaxAttempts == nil || *stored.MaxAttempts != 7 {
		t.Errorf("stored max_attempts = %v, want 7", stored.MaxAttempts)
	}
	if stored.RetryDelaySeconds == nil || *stored.RetryDelaySeconds != 30 {
		t.Errorf("stored retry_delay_seconds = %v, want 30", stored.RetryDelaySeconds)
	}
	if stored.FailureLimit == nil || *stored.FailureLimit != 40 {
		t.Errorf("stored failure_limit = %v, want 40", stored.FailureLimit)
	}
}

// ── run_as_user / run_as_group field-level authorization ─────────────────────
//
// Setting a queue's OS identity is gated separately from ordinary queue
// management (policy.InfraManage, which operator holds): it requires
// policy.IsolationManage (admin only). The check is field-level — a request
// that does not touch run_as_user/run_as_group must succeed for an operator
// exactly as it does today.

// withPrincipal attaches p to req's context, as the real auth middleware
// would for an authenticated request.
func withPrincipal(req *http.Request, p auth.Principal) *http.Request {
	return req.WithContext(auth.NewContext(req.Context(), p))
}

func TestCreateQueue_RunAsUserRequiresIsolationManage(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm, _ := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPost, "/api/v1/queues", jsonBody(t, map[string]any{
		"farm_id":     farm.ID,
		"name":        "isolated-queue",
		"run_as_user": "render-svc",
	}))
	req = withPrincipal(req, auth.Principal{Username: "op", Roles: []string{"operator"}})
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — body: %s", rr.Code, rr.Body)
	}
}

func TestCreateQueue_RunAsGroupRequiresIsolationManage(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm, _ := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPost, "/api/v1/queues", jsonBody(t, map[string]any{
		"farm_id":      farm.ID,
		"name":         "isolated-queue-group",
		"run_as_group": "render",
	}))
	req = withPrincipal(req, auth.Principal{Username: "op", Roles: []string{"operator"}})
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — body: %s", rr.Code, rr.Body)
	}
}

func TestCreateQueue_WithoutRunAsUserAllowedForOperator(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm, _ := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPost, "/api/v1/queues", jsonBody(t, map[string]any{
		"farm_id": farm.ID,
		"name":    "ordinary-queue",
	}))
	req = withPrincipal(req, auth.Principal{Username: "op", Roles: []string{"operator"}})
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (no isolation field, ordinary queue write) — body: %s", rr.Code, rr.Body)
	}
}

func TestCreateQueue_RunAsUserAllowedForAdmin(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm, _ := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPost, "/api/v1/queues", jsonBody(t, map[string]any{
		"farm_id":      farm.ID,
		"name":         "isolated-queue-admin",
		"run_as_user":  "render-svc",
		"run_as_group": "render",
	}))
	req = withPrincipal(req, auth.Principal{Username: "root", Roles: []string{"admin"}})
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — body: %s", rr.Code, rr.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["run_as_user"] != "render-svc" {
		t.Errorf("run_as_user = %v, want render-svc", got["run_as_user"])
	}
	if got["run_as_group"] != "render" {
		t.Errorf("run_as_group = %v, want render", got["run_as_group"])
	}

	id, ok := got["id"].(string)
	if !ok {
		t.Fatalf("id not a string: %v", got["id"])
	}
	stored, err := st.GetQueue(t.Context(), id)
	if err != nil {
		t.Fatalf("GetQueue: %v", err)
	}
	if stored.RunAsUser == nil || *stored.RunAsUser != "render-svc" {
		t.Errorf("stored run_as_user = %v, want render-svc", stored.RunAsUser)
	}
	if stored.RunAsGroup == nil || *stored.RunAsGroup != "render" {
		t.Errorf("stored run_as_group = %v, want render", stored.RunAsGroup)
	}
}

// TestCreateQueue_RunAsUserAllowedWhenAuthDisabled documents the auth-off
// path: the anonymous superuser principal (injected when auth is disabled)
// holds every permission, so setting run_as_user must still succeed —
// auth-off behavior must stay byte-for-byte unchanged.
func TestCreateQueue_RunAsUserAllowedWhenAuthDisabled(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm, _ := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPost, "/api/v1/queues", jsonBody(t, map[string]any{
		"farm_id":     farm.ID,
		"name":        "isolated-queue-anon",
		"run_as_user": "render-svc",
	}))
	req = withPrincipal(req, auth.Principal{Superuser: true, Kind: auth.KindAnonymous})
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — body: %s", rr.Code, rr.Body)
	}
}

func TestUpdateQueue_RunAsUserRequiresIsolationManage(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm, q := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	// An operator adding run_as_user to an EXISTING ordinary queue via PUT
	// must be rejected — an update-only hole here would let InfraManage
	// alone add isolation after the fact, defeating the create-side check.
	req := newReq(t, http.MethodPut, "/api/v1/queues/"+q.ID, jsonBody(t, map[string]any{
		"farm_id":     farm.ID,
		"name":        q.Name,
		"run_as_user": "render-svc",
	}))
	req = withPrincipal(req, auth.Principal{Username: "op", Roles: []string{"operator"}})
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — body: %s", rr.Code, rr.Body)
	}

	// Confirm the store was not touched.
	stored, err := st.GetQueue(t.Context(), q.ID)
	if err != nil {
		t.Fatalf("GetQueue: %v", err)
	}
	if stored.RunAsUser != nil {
		t.Errorf("stored run_as_user = %v, want nil (write must not have happened)", *stored.RunAsUser)
	}
}

func TestUpdateQueue_WithoutRunAsUserAllowedForOperator(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm, q := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPut, "/api/v1/queues/"+q.ID, jsonBody(t, map[string]any{
		"farm_id": farm.ID,
		"name":    q.Name,
	}))
	req = withPrincipal(req, auth.Principal{Username: "op", Roles: []string{"operator"}})
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no isolation field, ordinary queue write) — body: %s", rr.Code, rr.Body)
	}
}

// seedFarmAndIsolatedQueue creates a farm and a queue whose run_as_user is
// already set to "render-svc". seedFarmAndQueue's queue never has isolation
// set, so it cannot exercise the omit-preserves / explicit-null-clears gate
// on update — these tests need a queue that already carries an OS identity.
func seedFarmAndIsolatedQueue(t *testing.T, st *fake.Store) (store.Farm, store.Queue) {
	t.Helper()
	ctx := t.Context()
	now := time.Now()

	farm := store.Farm{ID: uuid.NewString(), Name: "farm-q-" + uuid.NewString(), CreatedAt: now, UpdatedAt: now}
	f, err := st.CreateFarm(ctx, farm)
	if err != nil {
		t.Fatalf("seedFarmAndIsolatedQueue: CreateFarm: %v", err)
	}

	runAsUser := "render-svc"
	q := store.Queue{
		ID: uuid.NewString(), FarmID: f.ID, Name: "queue-" + uuid.NewString(),
		Priority: 10, CreatedAt: now, UpdatedAt: now,
		RunAsUser: &runAsUser,
	}
	created, err := st.CreateQueue(ctx, q)
	if err != nil {
		t.Fatalf("seedFarmAndIsolatedQueue: CreateQueue: %v", err)
	}
	return f, created
}

// TestUpdateQueue_OmittingIsolationKeysPreservesRunAsUser is the regression
// test for the critical bug: an operator PUT that never mentions
// run_as_user/run_as_group must not clear a previously-set identity just
// because those keys are absent from the body.
func TestUpdateQueue_OmittingIsolationKeysPreservesRunAsUser(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm, q := seedFarmAndIsolatedQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPut, "/api/v1/queues/"+q.ID, jsonBody(t, map[string]any{
		"farm_id":  farm.ID,
		"name":     q.Name,
		"priority": 99,
	}))
	req = withPrincipal(req, auth.Principal{Username: "op", Roles: []string{"operator"}})
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (ordinary field-only PUT on isolated queue) — body: %s", rr.Code, rr.Body)
	}

	stored, err := st.GetQueue(t.Context(), q.ID)
	if err != nil {
		t.Fatalf("GetQueue: %v", err)
	}
	if stored.RunAsUser == nil || *stored.RunAsUser != "render-svc" {
		t.Errorf("stored run_as_user = %v, want render-svc (must be preserved, not cleared)", stored.RunAsUser)
	}
	if stored.Priority != 99 {
		t.Errorf("stored priority = %d, want 99", stored.Priority)
	}
}

// TestUpdateQueue_ExplicitNullRunAsUserRequiresIsolationManage asserts that
// sending run_as_user explicitly as null (a deliberate clear) is gated the
// same as setting it, and that a rejected request leaves the store untouched.
func TestUpdateQueue_ExplicitNullRunAsUserRequiresIsolationManage(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm, q := seedFarmAndIsolatedQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPut, "/api/v1/queues/"+q.ID, jsonBody(t, map[string]any{
		"farm_id":     farm.ID,
		"name":        q.Name,
		"run_as_user": nil,
	}))
	req = withPrincipal(req, auth.Principal{Username: "op", Roles: []string{"operator"}})
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (explicit null must be gated) — body: %s", rr.Code, rr.Body)
	}

	stored, err := st.GetQueue(t.Context(), q.ID)
	if err != nil {
		t.Fatalf("GetQueue: %v", err)
	}
	if stored.RunAsUser == nil || *stored.RunAsUser != "render-svc" {
		t.Errorf("stored run_as_user = %v, want render-svc (rejected write must not have happened)", stored.RunAsUser)
	}
}

// TestUpdateQueue_ExplicitNullRunAsUserAllowedForAdmin asserts that an admin
// may deliberately clear run_as_user by sending it explicitly as null.
func TestUpdateQueue_ExplicitNullRunAsUserAllowedForAdmin(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm, q := seedFarmAndIsolatedQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPut, "/api/v1/queues/"+q.ID, jsonBody(t, map[string]any{
		"farm_id":     farm.ID,
		"name":        q.Name,
		"run_as_user": nil,
	}))
	req = withPrincipal(req, auth.Principal{Username: "root", Roles: []string{"admin"}})
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (admin explicit null clears isolation) — body: %s", rr.Code, rr.Body)
	}

	stored, err := st.GetQueue(t.Context(), q.ID)
	if err != nil {
		t.Fatalf("GetQueue: %v", err)
	}
	if stored.RunAsUser != nil {
		t.Errorf("stored run_as_user = %v, want nil (explicit null must clear)", *stored.RunAsUser)
	}
}

// TestUpdateQueue_OmittedRunAsUserAllowedForAdminPreserved asserts that an
// admin PUT that omits the isolation keys also preserves the stored value —
// preservation is not operator-specific, it's key-presence-specific.
func TestUpdateQueue_OmittedRunAsUserAllowedForAdminPreserved(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm, q := seedFarmAndIsolatedQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPut, "/api/v1/queues/"+q.ID, jsonBody(t, map[string]any{
		"farm_id": farm.ID,
		"name":    q.Name,
	}))
	req = withPrincipal(req, auth.Principal{Username: "root", Roles: []string{"admin"}})
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — body: %s", rr.Code, rr.Body)
	}

	stored, err := st.GetQueue(t.Context(), q.ID)
	if err != nil {
		t.Fatalf("GetQueue: %v", err)
	}
	if stored.RunAsUser == nil || *stored.RunAsUser != "render-svc" {
		t.Errorf("stored run_as_user = %v, want render-svc (preserved for admin too)", stored.RunAsUser)
	}
}

func TestUpdateQueue_RunAsUserAllowedForAdmin(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm, q := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPut, "/api/v1/queues/"+q.ID, jsonBody(t, map[string]any{
		"farm_id":     farm.ID,
		"name":        q.Name,
		"run_as_user": "render-svc",
	}))
	req = withPrincipal(req, auth.Principal{Username: "root", Roles: []string{"admin"}})
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — body: %s", rr.Code, rr.Body)
	}

	stored, err := st.GetQueue(t.Context(), q.ID)
	if err != nil {
		t.Fatalf("GetQueue: %v", err)
	}
	if stored.RunAsUser == nil || *stored.RunAsUser != "render-svc" {
		t.Errorf("stored run_as_user = %v, want render-svc", stored.RunAsUser)
	}
}

// TestCreateQueue_RunAsGroupWithoutRunAsUser_Rejected proves an admin (who
// holds isolation.manage and would otherwise sail past
// requireIsolationManage) still gets a 400 when run_as_group is set with no
// run_as_user: that combination selects no OS identity at all — the
// scheduler only gates isolation on RunAsUser (internal/scheduler/
// assign.go) — so accepting it would silently give the operator no isolation
// and no warning.
func TestCreateQueue_RunAsGroupWithoutRunAsUser_Rejected(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm, _ := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPost, "/api/v1/queues", jsonBody(t, map[string]any{
		"farm_id":      farm.ID,
		"name":         "group-only-queue",
		"run_as_group": "render",
	}))
	req = withPrincipal(req, auth.Principal{Username: "root", Roles: []string{"admin"}})
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — body: %s", rr.Code, rr.Body)
	}
	if !strings.Contains(rr.Body.String(), "run_as_group requires run_as_user") {
		t.Errorf("body %q does not mention the run_as_group/run_as_user requirement", rr.Body.String())
	}
}

// TestUpdateQueue_RunAsGroupWithoutRunAsUser_Rejected is the update-side
// equivalent, submitting both keys explicitly in the same PUT.
func TestUpdateQueue_RunAsGroupWithoutRunAsUser_Rejected(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm, q := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPut, "/api/v1/queues/"+q.ID, jsonBody(t, map[string]any{
		"farm_id":      farm.ID,
		"name":         q.Name,
		"run_as_user":  nil,
		"run_as_group": "render",
	}))
	req = withPrincipal(req, auth.Principal{Username: "root", Roles: []string{"admin"}})
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — body: %s", rr.Code, rr.Body)
	}
	if !strings.Contains(rr.Body.String(), "run_as_group requires run_as_user") {
		t.Errorf("body %q does not mention the run_as_group/run_as_user requirement", rr.Body.String())
	}
}

// TestUpdateQueue_ClearingRunAsUserWhileGroupOmitted_Rejected covers what
// the handler's own request-only check cannot see: run_as_user is cleared
// explicitly, run_as_group is omitted (preserved) from an already-non-empty
// value. The resolved post-write state would be group-without-user, caught
// by the store-layer check (store.ErrRunAsGroupWithoutUser, mapped to 400)
// rather than the handler's early one.
func TestUpdateQueue_ClearingRunAsUserWhileGroupOmitted_Rejected(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm, q := seedFarmAndIsolatedQueue(t, st) // run_as_user = "render-svc", no group yet
	admin := auth.Principal{Username: "root", Roles: []string{"admin"}}
	r := newQueueRouter(st)

	// First, an admin sets run_as_group so the queue carries both fields.
	setGroupReq := withPrincipal(newReq(t, http.MethodPut, "/api/v1/queues/"+q.ID, jsonBody(t, map[string]any{
		"farm_id":      farm.ID,
		"name":         q.Name,
		"run_as_group": "render",
	})), admin)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, setGroupReq)
	if rr.Code != http.StatusOK {
		t.Fatalf("setup: status = %d, want 200 — body: %s", rr.Code, rr.Body)
	}

	// Now clear run_as_user while omitting run_as_group (preserved).
	req := withPrincipal(newReq(t, http.MethodPut, "/api/v1/queues/"+q.ID, jsonBody(t, map[string]any{
		"farm_id":     farm.ID,
		"name":        q.Name,
		"run_as_user": nil,
	})), admin)
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — body: %s", rr.Code, rr.Body)
	}

	stored, err := st.GetQueue(t.Context(), q.ID)
	if err != nil {
		t.Fatalf("GetQueue: %v", err)
	}
	if stored.RunAsUser == nil || *stored.RunAsUser != "render-svc" {
		t.Errorf("run_as_user = %v, want render-svc (rejected update must not partially apply)", stored.RunAsUser)
	}
	if stored.RunAsGroup == nil || *stored.RunAsGroup != "render" {
		t.Errorf("run_as_group = %v, want render (rejected update must not partially apply)", stored.RunAsGroup)
	}
}

// ── listQueues ────────────────────────────────────────────────────────────────

func TestListQueues_Success(t *testing.T) {
	st := fake.New()
	defer st.Close()
	seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodGet, "/api/v1/queues", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
}

func TestListQueues_PausedFilter(t *testing.T) {
	st := fake.New()
	defer st.Close()
	r := newQueueRouter(st)

	for _, paused := range []string{"true", "false"} {
		req := newReq(t, http.MethodGet, "/api/v1/queues?paused="+paused, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("paused=%s: want 200, got %d", paused, rr.Code)
		}
	}
}

// ── getQueue ──────────────────────────────────────────────────────────────────

func TestGetQueue_NotFound(t *testing.T) {
	st := fake.New()
	defer st.Close()
	r := newQueueRouter(st)

	req := newReq(t, http.MethodGet, "/api/v1/queues/missing", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
}

func TestGetQueue_Found(t *testing.T) {
	st := fake.New()
	defer st.Close()
	_, q := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodGet, "/api/v1/queues/"+q.ID, nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
}

// ── updateQueue ───────────────────────────────────────────────────────────────

func TestUpdateQueue_InvalidJSON(t *testing.T) {
	st := fake.New()
	defer st.Close()
	_, q := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPut, "/api/v1/queues/"+q.ID, badJSON())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestUpdateQueue_MissingFarmID(t *testing.T) {
	st := fake.New()
	defer st.Close()
	_, q := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPut, "/api/v1/queues/"+q.ID, jsonBody(t, map[string]any{
		"name": "new-name",
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestUpdateQueue_FarmNotFound(t *testing.T) {
	st := fake.New()
	defer st.Close()
	_, q := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPut, "/api/v1/queues/"+q.ID, jsonBody(t, map[string]any{
		"farm_id": "no-such-farm",
		"name":    "renamed",
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestUpdateQueue_NotFound(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm, _ := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPut, "/api/v1/queues/missing", jsonBody(t, map[string]any{
		"farm_id": farm.ID,
		"name":    "renamed",
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
}

func TestUpdateQueue_Conflict(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm, q1 := seedFarmAndQueue(t, st)
	// Create a second queue whose name we'll try to collide with.
	now := time.Now()
	q2 := store.Queue{ID: uuid.NewString(), FarmID: farm.ID, Name: "other-queue-" + uuid.NewString(), CreatedAt: now, UpdatedAt: now}
	if _, err := st.CreateQueue(t.Context(), q2); err != nil {
		t.Fatalf("CreateQueue q2: %v", err)
	}
	r := newQueueRouter(st)

	// Try to rename q1 to q2's name — should conflict.
	req := newReq(t, http.MethodPut, "/api/v1/queues/"+q1.ID, jsonBody(t, map[string]any{
		"farm_id": farm.ID,
		"name":    q2.Name,
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", rr.Code)
	}
}

func TestUpdateQueue_Success(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm, q := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPut, "/api/v1/queues/"+q.ID, jsonBody(t, map[string]any{
		"farm_id": farm.ID,
		"name":    q.Name, // same name, no conflict
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── deleteQueue ───────────────────────────────────────────────────────────────

func TestDeleteQueue_NotFound(t *testing.T) {
	st := fake.New()
	defer st.Close()
	r := newQueueRouter(st)

	req := newReq(t, http.MethodDelete, "/api/v1/queues/missing", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
}

func TestDeleteQueue_Success(t *testing.T) {
	st := fake.New()
	defer st.Close()
	_, q := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodDelete, "/api/v1/queues/"+q.ID, nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d", rr.Code)
	}
}

// TestQueueRetryOverrides_Rejected asserts queue create validates the retry
// override bounds at the API boundary, mirroring the farm-level checks.
func TestQueueRetryOverrides_Rejected(t *testing.T) {
	st := fake.New()
	defer st.Close()
	if _, err := st.CreateFarm(t.Context(), store.Farm{ID: "farm-1", Name: "f"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	r := newQueueRouter(st)

	tests := []struct {
		name string
		body map[string]any
		want string
	}{
		{"zero max_attempts", map[string]any{"farm_id": "farm-1", "name": "q1", "max_attempts": 0}, "max_attempts must be >= 1"},
		{"negative retry_delay_seconds", map[string]any{"farm_id": "farm-1", "name": "q2", "retry_delay_seconds": -1}, "retry_delay_seconds must be >= 0"},
		{"negative failure_limit", map[string]any{"farm_id": "farm-1", "name": "q3", "failure_limit": -1}, "failure_limit must be >= 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := newReq(t, http.MethodPost, "/api/v1/queues", jsonBody(t, tt.body))
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400 — body: %s", rr.Code, rr.Body)
			}
			if !strings.Contains(rr.Body.String(), tt.want) {
				t.Errorf("body %q does not contain %q", rr.Body.String(), tt.want)
			}
		})
	}
}
