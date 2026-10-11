// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore_test

import (
	"slices"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlite"
	"github.com/uberware/sqi/internal/store/sqlstore"
	"github.com/uberware/sqi/internal/store/storetest"
)

// upstreamFixture seeds two pending upstream jobs, up-a and up-b, and the
// farm and queue of a dependent job named blk. It returns a store over the
// same database whose dialect records the anchors it is asked to lock.
func upstreamFixture(t *testing.T) (base *sqlite.Store, s *sqlstore.Store, d *recordingDialect) {
	t.Helper()
	base = openTestStoreWB(t)
	seedJob(t, base, "up-b")
	seedJob(t, base, "up-a")
	seedScope(t, base, "blk")
	d = &recordingDialect{Dialect: base.Dialect()}
	return base, withDialect(t, base, d), d
}

// blockedSub is the submission of the blocked job blk, waiting on up-b and
// up-a (named out of ID order, with up-b twice).
func blockedSub() store.JobSubmission {
	return store.JobSubmission{
		Job:       jobFor("blk", store.JobStatusBlocked),
		DependsOn: []string{"up-b", "up-a", "up-b"},
	}
}

func jobRow(id string, mode sqlstore.LockMode) sqlstore.Anchor {
	return sqlstore.Anchor{Table: "jobs", ID: id, Mode: mode}
}

func wantAnchorCalls(t *testing.T, got, want [][]sqlstore.Anchor) {
	t.Helper()
	if !slices.EqualFunc(got, want, slices.Equal[[]sqlstore.Anchor]) {
		t.Fatalf("LockAnchors calls = %+v, want %+v", got, want)
	}
}

// TestCreateJobSubmission_LocksUpstreamsShared pins the anchors a submission
// asks for: one call, every upstream job row once, shared, in ID order.
func TestCreateJobSubmission_LocksUpstreamsShared(t *testing.T) {
	_, s, d := upstreamFixture(t)
	if _, err := s.CreateJobSubmission(t.Context(), blockedSub()); err != nil {
		t.Fatalf("CreateJobSubmission: %v", err)
	}
	wantAnchorCalls(t, d.anchors, [][]sqlstore.Anchor{{
		jobRow("up-a", sqlstore.LockShare),
		jobRow("up-b", sqlstore.LockShare),
	}})
}

// TestBlockedJobOps_LockOwnRowThenUpstreamsShared pins the anchors of the two
// blocked-job operations: the job's own row exclusively, then each upstream
// job row shared, in ID order.
func TestBlockedJobOps_LockOwnRowThenUpstreamsShared(t *testing.T) {
	ops := map[string]func(t *testing.T, s *sqlstore.Store){
		"ReleaseBlockedJob": func(t *testing.T, s *sqlstore.Store) {
			if _, err := s.ReleaseBlockedJob(t.Context(), "blk", time.Now().UTC()); err != nil {
				t.Fatalf("ReleaseBlockedJob: %v", err)
			}
		},
		"CancelBlockedJob": func(t *testing.T, s *sqlstore.Store) {
			if _, _, err := s.CancelBlockedJob(t.Context(), "blk", "test", time.Now().UTC()); err != nil {
				t.Fatalf("CancelBlockedJob: %v", err)
			}
		},
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			base, s, d := upstreamFixture(t)
			storetest.Submit(t, base, blockedSub())
			op(t, s)
			wantAnchorCalls(t, d.anchors, [][]sqlstore.Anchor{{
				jobRow("blk", sqlstore.LockUpdate),
				jobRow("up-a", sqlstore.LockShare),
				jobRow("up-b", sqlstore.LockShare),
			}})
		})
	}
}
