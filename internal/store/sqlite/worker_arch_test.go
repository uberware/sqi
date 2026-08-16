// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite_test

import (
	"context"
	"testing"

	"github.com/uberware/sqi/internal/store"
)

// TestWorker_ArchRoundTrip pins migration 00028 and the four SQL sites that
// carry a worker's CPU architecture through SQLite.
//
// It exercises all three accessors deliberately. The arch column had to be
// added to the shared column list, the INSERT, the ON CONFLICT DO UPDATE, the
// standalone UPDATE, the scan and two argument lists — and a miss in any ONE of
// them is silent in a way the compiler cannot see: the value is a plain string
// passed positionally, so an omission reads back as "" rather than failing.
// UpdateWorker in particular has its own statement that no other test here
// covers for this field.
//
// "" is what a worker predating this field reports, and the scheduler treats it
// as "matches no attr.worker.cpu.arch requirement" (scheduler.cpuArch). So a
// round trip that quietly dropped the value would not error anywhere — it would
// just stop that worker from ever being eligible for architecture-gated work,
// which is precisely the bug this column exists to fix.
func TestWorker_ArchRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")

	// GOARCH spelling, not the OpenJD token: the store holds what the host
	// reported, and internal/scheduler translates on the read side.
	const want = "arm64"

	registered, err := s.RegisterWorker(ctx, store.Worker{
		ID: "w1", FarmID: "f1", Hostname: "h1", Status: store.WorkerStatusOnline,
		Tags: map[string]string{}, Arch: want,
	})
	if err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	if registered.Arch != want {
		t.Errorf("RegisterWorker returned Arch %q, want %q", registered.Arch, want)
	}

	fetched, err := s.GetWorker(ctx, "w1")
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if fetched.Arch != want {
		t.Errorf("GetWorker Arch = %q, want %q", fetched.Arch, want)
	}

	// Re-registration must overwrite it. A host genuinely changes architecture
	// when a worker ID is reused on new hardware, or when a binary that did not
	// report arch is replaced by one that does — the stale value would make the
	// worker eligible for work its CPU cannot run.
	const changed = "amd64"
	if _, err := s.RegisterWorker(ctx, store.Worker{
		ID: "w1", FarmID: "f1", Hostname: "h1", Status: store.WorkerStatusOnline,
		Tags: map[string]string{}, Arch: changed,
	}); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	again, err := s.GetWorker(ctx, "w1")
	if err != nil {
		t.Fatalf("GetWorker after re-register: %v", err)
	}
	if again.Arch != changed {
		t.Errorf("after re-registration Arch = %q, want %q", again.Arch, changed)
	}

	// UpdateWorker is a separate statement from the upsert above and is the one
	// most easily missed, since nothing else in this file exercises it.
	again.Arch = want
	updated, err := s.UpdateWorker(ctx, again)
	if err != nil {
		t.Fatalf("UpdateWorker: %v", err)
	}
	if updated.Arch != want {
		t.Errorf("UpdateWorker returned Arch = %q, want %q", updated.Arch, want)
	}
	refetched, err := s.GetWorker(ctx, "w1")
	if err != nil {
		t.Fatalf("GetWorker after update: %v", err)
	}
	if refetched.Arch != want {
		t.Errorf("after UpdateWorker Arch = %q, want %q -- the UPDATE statement "+
			"or its argument list is missing the column", refetched.Arch, want)
	}
}

// TestWorker_ArchDefaultsEmpty pins the migration's empty-string DEFAULT for a
// worker that does not report an architecture: a NULL would be a scan error
// rather than an empty string, and every row that existed before 00028 ran gets
// this value.
func TestWorker_ArchDefaultsEmpty(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")

	if _, err := s.RegisterWorker(ctx, store.Worker{
		ID: "w1", FarmID: "f1", Hostname: "h1", Status: store.WorkerStatusOnline,
		Tags: map[string]string{},
	}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	got, err := s.GetWorker(ctx, "w1")
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if got.Arch != "" {
		t.Errorf("Arch = %q for a worker that reported none, want empty", got.Arch)
	}
}
