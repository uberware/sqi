// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"slices"
	"testing"

	"github.com/uberware/sqi/internal/store"
)

// TestJob_ListDependentsAndBlocked exercises the dependency edges a submission
// writes, ListDependents, ListBlockedJobs, and GetJob's DependsOn population
// together, mirroring the equivalent SQLite test.
func TestJob_ListDependentsAndBlocked(t *testing.T) {
	s := New()
	farm := mustCreateFarm(t, s, "listdeps")
	queue := mustCreateQueue(t, s, farm.ID, "queue-listdeps", "listdeps")

	up := newJob("up-listdeps", farm.ID, queue.ID).submit(t, s).Job
	down := mustSubmit(t, s, store.JobSubmission{
		Job: store.Job{
			ID: "down-listdeps", FarmID: farm.ID, QueueID: queue.ID, Name: "down",
			Priority: 50, Status: store.JobStatusBlocked,
		},
		DependsOn: []string{up.ID},
	}).Job

	deps, err := s.ListDependents(ctx(), up.ID)
	if err != nil {
		t.Fatalf("ListDependents: %v", err)
	}
	if len(deps) != 1 || deps[0] != down.ID {
		t.Fatalf("ListDependents = %v, want [%s]", deps, down.ID)
	}

	blocked, err := s.ListBlockedJobs(ctx())
	if err != nil {
		t.Fatalf("ListBlockedJobs: %v", err)
	}
	if len(blocked) != 1 || blocked[0].ID != down.ID {
		t.Fatalf("ListBlockedJobs = %v, want [%s]", blocked, down.ID)
	}

	// GetJob populates DependsOn.
	got, err := s.GetJob(ctx(), down.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if len(got.DependsOn) != 1 || got.DependsOn[0] != up.ID {
		t.Fatalf("GetJob DependsOn = %v, want [%s]", got.DependsOn, up.ID)
	}
}

// TestJob_DeleteJob_RemovesOutgoingEdgesKeepsIncoming verifies the asymmetric
// delete behavior: deleting the downstream job removes its outgoing edges,
// but deleting the upstream job leaves the (now-dangling) edge intact so the
// reconciler can observe "upstream deleted".
func TestJob_DeleteJob_RemovesOutgoingEdgesKeepsIncoming(t *testing.T) {
	s := New()
	farm := mustCreateFarm(t, s, "delcascade")
	queue := mustCreateQueue(t, s, farm.ID, "queue-delcascade", "delcascade")

	up := newJob("up-delcascade", farm.ID, queue.ID).submit(t, s).Job
	down := mustSubmit(t, s, store.JobSubmission{
		Job: store.Job{
			ID: "down-delcascade", FarmID: farm.ID, QueueID: queue.ID, Name: "down",
			Priority: 50, Status: store.JobStatusBlocked,
		},
		DependsOn: []string{up.ID},
	}).Job

	// Deleting the UPSTREAM must NOT delete the edge (no FK on depends_on_job_id):
	if err := s.DeleteJob(ctx(), up.ID); err != nil {
		t.Fatalf("DeleteJob(up): %v", err)
	}
	deps, err := s.ListJobDependencyIDs(ctx(), down.ID)
	if err != nil {
		t.Fatalf("ListJobDependencyIDs: %v", err)
	}
	if len(deps) != 1 || deps[0] != up.ID {
		t.Fatalf("after upstream delete, edge should survive: got %v", deps)
	}

	// Deleting the DOWNSTREAM removes its outgoing edges:
	if err := s.DeleteJob(ctx(), down.ID); err != nil {
		t.Fatalf("DeleteJob(down): %v", err)
	}
	deps, err = s.ListDependents(ctx(), up.ID)
	if err != nil {
		t.Fatalf("ListDependents: %v", err)
	}
	if len(deps) != 0 {
		t.Fatalf("after downstream delete, no edges should remain: got %v", deps)
	}
}

// TestJob_CreateJobSubmission_DuplicateDependencyIsIgnored verifies that a
// submission naming the same upstream twice neither fails nor produces a
// duplicate edge — the slices.Contains guard makes the edge write idempotent,
// mirroring SQLite's INSERT OR IGNORE + primary key. It replaces the dedup test
// of the per-row edge writer, which a submission's DependsOn has superseded.
func TestJob_CreateJobSubmission_DuplicateDependencyIsIgnored(t *testing.T) {
	s := New()
	farm := mustCreateFarm(t, s, "dedup")
	queue := mustCreateQueue(t, s, farm.ID, "queue-dedup", "dedup")

	up1 := newJob("up1-dedup", farm.ID, queue.ID).submit(t, s).Job
	up2 := newJob("up2-dedup", farm.ID, queue.ID).submit(t, s).Job
	down := store.Job{
		ID: "down-dedup", FarmID: farm.ID, QueueID: queue.ID, Name: "down",
		Priority: 50, Status: store.JobStatusBlocked,
	}
	// up1 is named twice.
	mustSubmit(t, s, store.JobSubmission{Job: down, DependsOn: []string{up1.ID, up1.ID, up2.ID}})

	deps, err := s.ListJobDependencyIDs(ctx(), down.ID)
	if err != nil {
		t.Fatalf("ListJobDependencyIDs: %v", err)
	}
	want := []string{up1.ID, up2.ID}
	slices.Sort(want)
	if !slices.Equal(deps, want) {
		t.Fatalf("ListJobDependencyIDs = %v, want %v (no duplicate edge)", deps, want)
	}
}

// TestJob_ListJobDependencyIDs_OrderedByUpstreamID locks in the ordering fix:
// ListJobDependencyIDs must return upstream IDs sorted by ID, regardless of
// the order they were passed in the submission or created in, mirroring the
// equivalent SQLite test.
func TestJob_ListJobDependencyIDs_OrderedByUpstreamID(t *testing.T) {
	s := New()
	farm := mustCreateFarm(t, s, "order")
	queue := mustCreateQueue(t, s, farm.ID, "queue-order", "order")

	zeta := newJob("zeta-order", farm.ID, queue.ID).submit(t, s).Job
	alpha := newJob("alpha-order", farm.ID, queue.ID).submit(t, s).Job
	mike := newJob("mike-order", farm.ID, queue.ID).submit(t, s).Job
	down := store.Job{
		ID: "down-order", FarmID: farm.ID, QueueID: queue.ID, Name: "down",
		Priority: 50, Status: store.JobStatusBlocked,
	}
	// Deliberately not alphabetical.
	mustSubmit(t, s, store.JobSubmission{Job: down, DependsOn: []string{zeta.ID, alpha.ID, mike.ID}})

	deps, err := s.ListJobDependencyIDs(ctx(), down.ID)
	if err != nil {
		t.Fatalf("ListJobDependencyIDs: %v", err)
	}
	want := []string{alpha.ID, mike.ID, zeta.ID}
	if !slices.Equal(deps, want) {
		t.Fatalf("ListJobDependencyIDs = %v, want %v", deps, want)
	}
}
