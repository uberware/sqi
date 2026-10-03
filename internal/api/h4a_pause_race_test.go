// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/store/sqlite"
)

// h4aBackends returns a fresh store for each backend the H4a race tests run
// over (spec §8.1), keyed by subtest name. The SQLite store lives in a temp
// directory and is closed when the test ends.
func h4aBackends(t *testing.T) map[string]store.Store {
	t.Helper()
	sq, err := sqlite.Open(t.Context(), t.TempDir()+"/test.db", sqlite.DefaultOptions())
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := sq.Close(); err != nil {
			t.Errorf("close sqlite store: %v", err)
		}
	})
	return map[string]store.Store{"fake": fake.New(), "sqlite": sq}
}

// completeDuringPatchStore runs hook between the handler's status read and its
// status write. patchJob calls UpdateJob on that path both before and after
// the fix, so the hook lands in the window either way.
type completeDuringPatchStore struct {
	store.Store

	hook func()
	once sync.Once
}

func (s *completeDuringPatchStore) UpdateJob(ctx context.Context, j store.Job) (store.Job, error) {
	out, err := s.Store.UpdateJob(ctx, j)
	s.once.Do(s.hook)
	return out, err
}

func TestH4a_F10_PauseDoesNotOverwriteCompletion(t *testing.T) {
	for name, st := range h4aBackends(t) {
		t.Run(name, func(t *testing.T) {
			j := seedJob(t, st, store.JobStatusRunning)
			// The seeded job has no steps, so FinalizeJob completes it: the same
			// write the scheduler makes when the last step finishes.
			wrapped := &completeDuringPatchStore{Store: st, hook: func() {
				status, changed, err := st.FinalizeJob(context.Background(), j.ID, time.Now())
				if err != nil || !changed || status != store.JobStatusCompleted {
					t.Errorf("FinalizeJob in hook = (%q, %v, %v), want (completed, true, nil)", status, changed, err)
				}
			}}
			r := newJobRouter(wrapped, &fakeScheduler{})

			req := newReq(t, http.MethodPatch, "/api/v1/jobs/"+j.ID, jsonBody(t, patchJobRequest{Action: "pause"}))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != http.StatusConflict {
				t.Fatalf("PATCH pause over a just-completed job = %d, want 409; body=%s", rr.Code, rr.Body)
			}
			got, err := st.GetJob(t.Context(), j.ID)
			if err != nil {
				t.Fatalf("GetJob: %v", err)
			}
			if got.Status != store.JobStatusCompleted {
				t.Fatalf("job = %q, want completed", got.Status)
			}
		})
	}
}
