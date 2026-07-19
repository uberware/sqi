// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

// Parity test: ListJobs' Owner filter must compare case-insensitively on
// both backends. SQLite does this via an explicit `COLLATE NOCASE` on the
// comparison (the `owner` column itself carries no collation — see
// internal/store/migrations/00001_placeholder.sql); the fake store mirrors
// it with strings.EqualFold. See internal/api/jobscope.go's scopeFilter,
// which pins ListJobsOptions.Owner to the caller's username: without this,
// a stored owner of "Alice" would not match a scoped lookup for "alice".

import (
	"context"
	"testing"

	"github.com/uberware/sqi/internal/store"
)

func TestJobStore_ListJobs_OwnerCaseInsensitive(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()

			if _, err := st.CreateFarm(ctx, store.Farm{ID: "farm-1", Name: "f"}); err != nil {
				t.Fatalf("CreateFarm: %v", err)
			}
			if _, err := st.CreateQueue(ctx, store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "q"}); err != nil {
				t.Fatalf("CreateQueue: %v", err)
			}
			for _, j := range []store.Job{
				{ID: "job-alice", FarmID: "farm-1", QueueID: "queue-1", Name: "Alice's job", Owner: "Alice", Status: store.JobStatusPending},
				{ID: "job-bob", FarmID: "farm-1", QueueID: "queue-1", Name: "Bob's job", Owner: "bob", Status: store.JobStatusPending},
			} {
				if _, err := st.CreateJob(ctx, j); err != nil {
					t.Fatalf("CreateJob(%s): %v", j.ID, err)
				}
			}

			page, err := st.ListJobs(ctx, store.ListJobsOptions{Owner: "aliCE"})
			if err != nil {
				t.Fatalf("ListJobs: %v", err)
			}
			if len(page.Items) != 1 || page.Items[0].ID != "job-alice" {
				t.Fatalf("ListJobs(Owner: %q) = %v, want only job-alice", "aliCE", page.Items)
			}
		})
	}
}
