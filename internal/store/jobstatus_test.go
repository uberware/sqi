// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

func TestPromoteJobRunning(t *testing.T) {
	for _, from := range []store.JobStatus{store.JobStatusPending, store.JobStatusPaused, store.JobStatusCanceled, store.JobStatusCompleted} {
		for name, st := range newStores(t) {
			t.Run(string(from)+"/"+name, func(t *testing.T) {
				g := seedGraph(t, st, graphOpts{jobStatus: from})
				ok, err := st.PromoteJobRunning(t.Context(), g.Job.ID, time.Now().UTC())
				if err != nil {
					t.Fatalf("PromoteJobRunning: %v", err)
				}
				want := from == store.JobStatusPending
				if ok != want {
					t.Fatalf("PromoteJobRunning from %q = %v, want %v", from, ok, want)
				}
				j := mustJob(t, st, g.Job.ID)
				if want && (j.Status != store.JobStatusRunning || j.StartedAt == nil) {
					t.Fatalf("job = %q started_at=%v, want running with started_at", j.Status, j.StartedAt)
				}
				if !want && j.Status != from {
					t.Fatalf("job = %q, want it left at %q", j.Status, from)
				}
			})
		}
	}
}

func TestPromoteJobRunning_MissingJobIsNotPromoted(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ok, err := st.PromoteJobRunning(t.Context(), "nope", time.Now().UTC())
			if err != nil {
				t.Fatalf("PromoteJobRunning(unknown) = %v, want nil", err)
			}
			if ok {
				t.Fatal("PromoteJobRunning(unknown) = true, want false")
			}
		})
	}
}

func TestPauseJob(t *testing.T) {
	cases := map[store.JobStatus]error{
		store.JobStatusPending:   nil,
		store.JobStatusRunning:   nil,
		store.JobStatusCompleted: store.ErrConflict,
		store.JobStatusCanceled:  store.ErrConflict,
		store.JobStatusBlocked:   store.ErrConflict,
	}
	for from, wantErr := range cases {
		for name, st := range newStores(t) {
			t.Run(string(from)+"/"+name, func(t *testing.T) {
				g := seedGraph(t, st, graphOpts{jobStatus: from})
				err := st.PauseJob(t.Context(), g.Job.ID, time.Now().UTC())
				if !errors.Is(err, wantErr) {
					t.Fatalf("PauseJob from %q = %v, want %v", from, err, wantErr)
				}
				got := mustJob(t, st, g.Job.ID).Status
				if wantErr == nil && got != store.JobStatusPaused || wantErr != nil && got != from {
					t.Fatalf("job = %q after PauseJob from %q", got, from)
				}
			})
		}
	}
	for name, st := range newStores(t) {
		t.Run("missing/"+name, func(t *testing.T) {
			if err := st.PauseJob(t.Context(), "nope", time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("PauseJob(unknown) = %v, want ErrNotFound", err)
			}
		})
	}
}
