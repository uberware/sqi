// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"testing"

	"github.com/uberware/sqi/internal/store"
)

// TestJob_ListDependentsAndBlocked exercises CreateJobDependencies,
// ListDependents, ListBlockedJobs, and GetJob's DependsOn population together,
// mirroring the equivalent SQLite test.
func TestJob_ListDependentsAndBlocked(t *testing.T) {
	s := New()
	farm := mustCreateFarm(t, s, "listdeps")
	queue := mustCreateQueue(t, s, farm.ID, "queue-listdeps", "listdeps")

	up := mustCreateJob(t, s, "up-listdeps", farm.ID, queue.ID)
	down := store.Job{
		ID: "down-listdeps", FarmID: farm.ID, QueueID: queue.ID, Name: "down",
		Priority: 50, Status: store.JobStatusBlocked,
	}
	if _, err := s.CreateJob(ctx(), down); err != nil {
		t.Fatalf("CreateJob(down): %v", err)
	}
	if err := s.CreateJobDependencies(ctx(), down.ID, []string{up.ID}); err != nil {
		t.Fatalf("CreateJobDependencies: %v", err)
	}

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

	up := mustCreateJob(t, s, "up-delcascade", farm.ID, queue.ID)
	down := store.Job{
		ID: "down-delcascade", FarmID: farm.ID, QueueID: queue.ID, Name: "down",
		Priority: 50, Status: store.JobStatusBlocked,
	}
	if _, err := s.CreateJob(ctx(), down); err != nil {
		t.Fatalf("CreateJob(down): %v", err)
	}
	if err := s.CreateJobDependencies(ctx(), down.ID, []string{up.ID}); err != nil {
		t.Fatalf("CreateJobDependencies: %v", err)
	}

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
