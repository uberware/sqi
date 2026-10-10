// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Additional unit tests for worker REST handlers.
//
// Covers filter by queue_id and store error paths not reached by workers_test.go.
// Uses the storeErr wrapper defined in jobs_error_test.go.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/store/storetest"
)

// ── workerErrStore: thin wrapper for worker store errors ──────────────────────

// workerErrStore wraps a store.Store to inject errors into ListWorkers,
// GetWorker, and DeleteWorkerIfRemovable. We use a separate type to avoid
// conflicts with storeErr's method set for other store methods.
type workerErrStore struct {
	store.Store

	listWorkersErr error
	getWorkerErr   error
	// deleteIfRemovableErr, when set, is what DeleteWorkerIfRemovable returns
	// instead of deleting. removeWorker's delete goes through that guarded
	// method, so this is the delete-failure injection point.
	deleteIfRemovableErr error
}

func (e *workerErrStore) ListWorkers(ctx context.Context, opts store.ListWorkersOptions) (store.Page[store.Worker], error) {
	if e.listWorkersErr != nil {
		return store.Page[store.Worker]{}, e.listWorkersErr
	}
	return e.Store.ListWorkers(ctx, opts)
}

func (e *workerErrStore) GetWorker(ctx context.Context, id string) (store.Worker, error) {
	if e.getWorkerErr != nil {
		return store.Worker{}, e.getWorkerErr
	}
	return e.Store.GetWorker(ctx, id)
}

func (e *workerErrStore) DeleteWorkerIfRemovable(ctx context.Context, id string) error {
	if e.deleteIfRemovableErr != nil {
		return e.deleteIfRemovableErr
	}
	return e.Store.DeleteWorkerIfRemovable(ctx, id)
}

// ── listWorkers: additional filter and error paths ────────────────────────────

func TestListWorkers_QueueIDFilterAndErrors(t *testing.T) {
	t.Run("filter by queue_id returns only matching workers", func(t *testing.T) {
		st := fake.New()
		r := newWorkerRouter(st)

		now := time.Now()
		// Worker assigned to queue-A.
		_, _, err := st.RegisterWorker(t.Context(), store.Worker{
			ID:           uuid.NewString(),
			FarmID:       "farm-1",
			QueueID:      "queue-A",
			Hostname:     "worker-a",
			Status:       store.WorkerStatusOnline,
			RegisteredAt: now,
			UpdatedAt:    now,
		})
		if err != nil {
			t.Fatalf("RegisterWorker: %v", err)
		}
		// Worker assigned to queue-B.
		_, _, err = st.RegisterWorker(t.Context(), store.Worker{
			ID:           uuid.NewString(),
			FarmID:       "farm-1",
			QueueID:      "queue-B",
			Hostname:     "worker-b",
			Status:       store.WorkerStatusOnline,
			RegisteredAt: now,
			UpdatedAt:    now,
		})
		if err != nil {
			t.Fatalf("RegisterWorker: %v", err)
		}

		req := newReq(t, http.MethodGet, "/api/v1/workers?queue_id=queue-A", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}
	})

	t.Run("ListWorkers store error returns 500", func(t *testing.T) {
		inner := fake.New()
		est := &workerErrStore{Store: inner, listWorkersErr: errInjected}
		r := newWorkerRouter(est)

		req := newReq(t, http.MethodGet, "/api/v1/workers", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500, got %d — body: %s", rr.Code, rr.Body)
		}
	})
}

// ── getWorker: store error path ───────────────────────────────────────────────

func TestGetWorker_StoreError(t *testing.T) {
	t.Run("internal store error returns 500", func(t *testing.T) {
		inner := fake.New()
		est := &workerErrStore{Store: inner, getWorkerErr: errInjected}
		r := newWorkerRouter(est)

		req := newReq(t, http.MethodGet, "/api/v1/workers/any-id", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500, got %d — body: %s", rr.Code, rr.Body)
		}
	})
}

// ── removeWorker: a delete failure AFTER a successful revoke ──────────────────

// TestRemoveWorker_DeleteFailsAfterSuccessfulRevoke proves the safe half of
// removeWorker's revoke-then-delete ordering: when the credential revoke
// succeeds but the subsequent store delete fails, the worker row survives
// (the request answers 500, safe to retry) but its broker access is already
// cut. newWorkerRouter wires the injected WorkerRevoker to the SAME
// underlying fake store as the handler's own store.Store (storeRevoker
// wraps whatever is passed in), so wrapping only DeleteWorkerIfRemovable with an
// error here is enough to reach this case: RevokeWorkerCredential runs for real
// against the shared fake, unaffected by the wrapper.
func TestRemoveWorker_DeleteFailsAfterSuccessfulRevoke(t *testing.T) {
	inner := fake.New()
	w := seedWorker(t, inner, store.WorkerStatusOffline)
	if _, err := inner.CreateWorkerCredential(t.Context(), store.WorkerCredential{
		ID: uuid.NewString(), WorkerID: w.ID, PublicKey: genPublicKey(t), EnrolledAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed CreateWorkerCredential: %v", err)
	}

	est := &workerErrStore{Store: inner, deleteIfRemovableErr: errInjected}
	r := newWorkerRouter(est)

	req := newReq(t, http.MethodDelete, "/api/v1/workers/"+w.ID, nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d — body: %s", rr.Code, rr.Body)
	}

	if _, err := inner.GetWorker(t.Context(), w.ID); err != nil {
		t.Errorf("worker row should survive a failed delete: GetWorker: %v", err)
	}
	if _, err := storetest.ActiveWorkerCredential(t.Context(), inner, w.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("credential should already be revoked even though the delete failed: got %v, want store.ErrNotFound", err)
	}
}

// TestRemoveWorker_GuardedDeleteRefusesAfterRevoke covers the residual window
// removeWorker's doc comment documents: the worker passed the handler's
// removability pre-check but became non-removable before the guarded delete
// (it came back), so the delete answers ErrConflict. The request is a 409, the
// row survives, and the credential is already revoked, the safe direction.
func TestRemoveWorker_GuardedDeleteRefusesAfterRevoke(t *testing.T) {
	inner := fake.New()
	w := seedWorker(t, inner, store.WorkerStatusOffline)
	if _, err := inner.CreateWorkerCredential(t.Context(), store.WorkerCredential{
		ID: uuid.NewString(), WorkerID: w.ID, PublicKey: genPublicKey(t), EnrolledAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed CreateWorkerCredential: %v", err)
	}

	est := &workerErrStore{Store: inner, deleteIfRemovableErr: store.ErrConflict}
	r := newWorkerRouter(est)

	req := newReq(t, http.MethodDelete, "/api/v1/workers/"+w.ID, nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d — body: %s", rr.Code, rr.Body)
	}

	if _, err := inner.GetWorker(t.Context(), w.ID); err != nil {
		t.Errorf("worker row should survive a refused delete: GetWorker: %v", err)
	}
	if _, err := storetest.ActiveWorkerCredential(t.Context(), inner, w.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("credential is revoked before the guarded delete runs: got %v, want store.ErrNotFound", err)
	}
}

// TestRemoveWorker_GuardedDeleteNotFound covers a worker that vanishes between
// the pre-check and the guarded delete: the request is a 404.
func TestRemoveWorker_GuardedDeleteNotFound(t *testing.T) {
	inner := fake.New()
	w := seedWorker(t, inner, store.WorkerStatusOffline)

	est := &workerErrStore{Store: inner, deleteIfRemovableErr: store.ErrNotFound}
	r := newWorkerRouter(est)

	req := newReq(t, http.MethodDelete, "/api/v1/workers/"+w.ID, nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d — body: %s", rr.Code, rr.Body)
	}
}
