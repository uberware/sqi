// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

// Parity test: ListJobs' Owner filter must compare case-insensitively on
// every backend. SQLite does this via an explicit `COLLATE NOCASE` on the
// comparison (the `owner` column itself carries no collation — see
// internal/store/migrations/00001_placeholder.sql); the fake store mirrors
// it with strings.EqualFold. See internal/api/jobscope.go's scopeFilter,
// which pins ListJobsOptions.Owner to the caller's username: without this,
// a stored owner of "Alice" would not match a scoped lookup for "alice".

import (
	"context"
	"slices"
	"testing"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
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
				storetest.Submit(t, st, store.JobSubmission{Job: j})
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

// TestJobStore_DeclaredExtensionsRoundTrip is the cross-backend parity for the
// jobs.declared_extensions column (migration 00027). Three states must survive
// a write/read cycle identically on SQLite and on the fake, because the
// scheduler branches on all three: NOT RECORDED (a row written before the
// column existed -- fall back to the raw-template byte scan), RECORDED EMPTY
// (the template declares no extensions -- do NOT fall back), and RECORDED
// NON-EMPTY (use the list).
//
// Conflating the first two is the regression this column's ” default exists
// to prevent: every pre-migration row would read as "declares nothing", and an
// EXPR job already in the queue would silently lose its worker-caps gate.
func TestJobStore_DeclaredExtensionsRoundTrip(t *testing.T) {
	cases := []struct {
		id           string
		declared     []string
		recorded     bool
		wantDeclared []string
		wantRecorded bool
	}{
		{
			id: "job-legacy", declared: nil, recorded: false,
			wantDeclared: nil, wantRecorded: false,
		},
		{
			id: "job-recorded-empty", declared: []string{}, recorded: true,
			wantDeclared: []string{}, wantRecorded: true,
		},
		{
			id: "job-recorded-nil", declared: nil, recorded: true,
			wantDeclared: []string{}, wantRecorded: true,
		},
		{
			id: "job-recorded-expr", declared: []string{"EXPR", "TASK_CHUNKING"}, recorded: true,
			wantDeclared: []string{"EXPR", "TASK_CHUNKING"}, wantRecorded: true,
		},
	}

	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if _, err := st.CreateFarm(ctx, store.Farm{ID: "farm-1", Name: "f"}); err != nil {
				t.Fatalf("CreateFarm: %v", err)
			}
			if _, err := st.CreateQueue(ctx, store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "q"}); err != nil {
				t.Fatalf("CreateQueue: %v", err)
			}

			for _, tc := range cases {
				t.Run(tc.id, func(t *testing.T) {
					created := storetest.Submit(t, st, store.JobSubmission{Job: store.Job{
						ID: tc.id, FarmID: "farm-1", QueueID: "queue-1", Name: tc.id,
						Status: store.JobStatusPending, RawTemplate: "{}",
						DeclaredExtensions: tc.declared, ExtensionsRecorded: tc.recorded,
					}}).Job
					assertDeclaredExtensions(t, "CreateJobSubmission", created, tc.wantDeclared, tc.wantRecorded)

					got, err := st.GetJob(ctx, tc.id)
					if err != nil {
						t.Fatalf("GetJob: %v", err)
					}
					assertDeclaredExtensions(t, "GetJob", got, tc.wantDeclared, tc.wantRecorded)
				})
			}

			// ListJobs is the scheduler-adjacent read path that must not lose
			// the field either -- a job listed but not fetched still answers
			// the same question.
			page, err := st.ListJobs(ctx, store.ListJobsOptions{})
			if err != nil {
				t.Fatalf("ListJobs: %v", err)
			}
			byID := make(map[string]store.Job, len(page.Items))
			for _, j := range page.Items {
				byID[j.ID] = j
			}
			for _, tc := range cases {
				assertDeclaredExtensions(t, "ListJobs", byID[tc.id], tc.wantDeclared, tc.wantRecorded)
			}
		})
	}
}

// assertDeclaredExtensions compares both halves of the tri-state at once: the
// list is meaningless without the "was it recorded" flag beside it.
func assertDeclaredExtensions(t *testing.T, via string, job store.Job, wantList []string, wantRecorded bool) {
	t.Helper()
	if job.ExtensionsRecorded != wantRecorded {
		t.Fatalf("%s(%s).ExtensionsRecorded = %v, want %v", via, job.ID, job.ExtensionsRecorded, wantRecorded)
	}
	if !slices.Equal(job.DeclaredExtensions, wantList) {
		t.Fatalf("%s(%s).DeclaredExtensions = %#v, want %#v", via, job.ID, job.DeclaredExtensions, wantList)
	}
	if (job.DeclaredExtensions == nil) != (wantList == nil) {
		t.Fatalf("%s(%s).DeclaredExtensions nil-ness = %v, want %v",
			via, job.ID, job.DeclaredExtensions == nil, wantList == nil)
	}
	declared, recorded := job.DeclaresExtension("EXPR")
	if recorded != wantRecorded || declared != slices.Contains(wantList, "EXPR") {
		t.Fatalf("%s(%s).DeclaresExtension(EXPR) = (%v, %v), want (%v, %v)",
			via, job.ID, declared, recorded, slices.Contains(wantList, "EXPR"), wantRecorded)
	}
}

func seedJobFarm(t *testing.T, st store.Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.CreateFarm(ctx, store.Farm{ID: "farm-1", Name: "f"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "q"}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
}

func TestJobStore_EstimateAndProductRoundTrip(t *testing.T) {
	secs := 3600
	cases := []struct {
		id      string
		est     *int
		product string
		version string
	}{
		{id: "job-with", est: &secs, product: "blender-batch-render", version: "1.2.0"},
		{id: "job-without"},
	}
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			seedJobFarm(t, st)
			check := func(t *testing.T, where string, got store.Job, est *int, product, version string) {
				t.Helper()
				if (got.EstimatedSeconds == nil) != (est == nil) || (est != nil && *got.EstimatedSeconds != *est) {
					t.Errorf("%s: EstimatedSeconds = %v, want %v", where, got.EstimatedSeconds, est)
				}
				if got.ProductName != product || got.ProductVersion != version {
					t.Errorf("%s: product = %q/%q, want %q/%q", where, got.ProductName, got.ProductVersion, product, version)
				}
			}
			for _, tc := range cases {
				created := storetest.Submit(t, st, store.JobSubmission{Job: store.Job{
					ID: tc.id, FarmID: "farm-1", QueueID: "queue-1", Name: tc.id,
					Status: store.JobStatusPending, RawTemplate: "{}",
					EstimatedSeconds: tc.est, ProductName: tc.product, ProductVersion: tc.version,
				}}).Job
				check(t, tc.id+" CreateJobSubmission", created, tc.est, tc.product, tc.version)
				got, err := st.GetJob(ctx, tc.id)
				if err != nil {
					t.Fatalf("GetJob: %v", err)
				}
				check(t, tc.id+" GetJob", got, tc.est, tc.product, tc.version)
			}
			page, err := st.ListJobs(ctx, store.ListJobsOptions{})
			if err != nil {
				t.Fatalf("ListJobs: %v", err)
			}
			byID := map[string]store.Job{}
			for _, j := range page.Items {
				byID[j.ID] = j
			}
			for _, tc := range cases {
				j, ok := byID[tc.id]
				if !ok {
					t.Fatalf("ListJobs missing %s", tc.id)
				}
				check(t, tc.id+" ListJobs", j, tc.est, tc.product, tc.version)
			}
		})
	}
}

func TestJobStore_UpdateJobEstimate(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			seedJobFarm(t, st)
			first := 3600
			created := storetest.Submit(t, st, store.JobSubmission{Job: store.Job{
				ID: "job-1", FarmID: "farm-1", QueueID: "queue-1", Name: "j",
				Status: store.JobStatusPending, RawTemplate: "{}",
				EstimatedSeconds: &first, ProductName: "p", ProductVersion: "1.0.0",
			}}).Job
			next := 7200
			created.EstimatedSeconds = &next
			created.ProductName = "other"
			created.ProductVersion = "9.9.9"
			got, err := st.UpdateJob(ctx, created)
			if err != nil {
				t.Fatalf("UpdateJob: %v", err)
			}
			if got.EstimatedSeconds == nil || *got.EstimatedSeconds != 7200 {
				t.Errorf("EstimatedSeconds = %v, want 7200", got.EstimatedSeconds)
			}
			if got.ProductName != "p" || got.ProductVersion != "1.0.0" {
				t.Errorf("product = %q/%q, want unchanged p/1.0.0", got.ProductName, got.ProductVersion)
			}
			got.EstimatedSeconds = nil
			got, err = st.UpdateJob(ctx, got)
			if err != nil {
				t.Fatalf("UpdateJob nil: %v", err)
			}
			if got.EstimatedSeconds != nil {
				t.Errorf("EstimatedSeconds = %v, want nil", *got.EstimatedSeconds)
			}
		})
	}
}
