// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/uberware/sqi/internal/store"
)

// TestWorker_ExprLimitsRoundTrip pins migration 00026 and the accessors that
// carry a worker's advertised OpenJD EXPR caps through SQLite.
//
// The scheduler refuses to dispatch an EXPR job to a worker whose caps are
// below the server's configured limits (internal/scheduler/exprcaps.go). If
// these values did not survive the round trip they would read back as zero,
// which the scheduler interprets as "not advertised" — i.e. as the worker's
// compiled-in defaults — and a genuinely tight worker would be handed work it
// cannot run. The values are distinct on purpose: this is FIVE same-typed
// int64s through one JSON column, where a transposition round-trips cleanly and
// no compiler sees it. Every field must appear in both literals below -- a
// four-of-five literal leaves the omitted dimension zero on both sides of the
// comparison, which is the one shape this test cannot detect. The field count
// is asserted at the end for exactly that reason.
func TestWorker_ExprLimitsRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")

	want := store.WorkerExprLimits{
		OperationLimit:          11_111,
		MemoryLimit:             2_222_222,
		AssignmentPositions:     3_333,
		AssignmentRetainedBytes: 4_444_444,
		LetRetainedBytes:        5_555_555,
	}
	registered, err := s.RegisterWorker(ctx, store.Worker{
		ID: "w1", FarmID: "f1", Hostname: "h1", Status: store.WorkerStatusOnline,
		Tags: map[string]string{}, ExprLimits: want,
	})
	if err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	if registered.ExprLimits != want {
		t.Errorf("RegisterWorker returned ExprLimits %+v, want %+v", registered.ExprLimits, want)
	}

	fetched, err := s.GetWorker(ctx, "w1")
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if fetched.ExprLimits != want {
		t.Errorf("GetWorker ExprLimits = %+v, want %+v", fetched.ExprLimits, want)
	}

	// Re-registration must overwrite them: a worker whose expr.* configuration
	// was tightened and then restarted must not keep advertising its old caps.
	tightened := store.WorkerExprLimits{
		OperationLimit:          10_000,
		MemoryLimit:             1_000_000,
		AssignmentPositions:     2_000,
		AssignmentRetainedBytes: 2_000_000,
		LetRetainedBytes:        1_000_000,
	}
	if _, err := s.RegisterWorker(ctx, store.Worker{
		ID: "w1", FarmID: "f1", Hostname: "h1", Status: store.WorkerStatusOnline,
		Tags: map[string]string{}, ExprLimits: tightened,
	}); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	again, err := s.GetWorker(ctx, "w1")
	if err != nil {
		t.Fatalf("GetWorker after re-register: %v", err)
	}
	if again.ExprLimits != tightened {
		t.Errorf("after re-registration ExprLimits = %+v, want %+v", again.ExprLimits, tightened)
	}

	// UpdateWorker is the admin-edit path; it must not drop them either.
	again.Name = "renamed"
	updated, err := s.UpdateWorker(ctx, again)
	if err != nil {
		t.Fatalf("UpdateWorker: %v", err)
	}
	if updated.ExprLimits != tightened {
		t.Errorf("UpdateWorker returned ExprLimits %+v, want %+v", updated.ExprLimits, tightened)
	}

	// The comparisons above are only as complete as the literals that feed
	// them. A dimension left out of BOTH literals is zero on both sides of
	// every == in this test, so it passes while nothing checks the column
	// carries it -- and that is not hypothetical: this test shipped covering
	// four of five fields after the fifth was added. Counting fields would
	// only catch the NEXT one; assert instead that every field of both
	// literals is actually populated, which catches an omission in the literal
	// that already exists.
	for _, lit := range []struct {
		name string
		v    store.WorkerExprLimits
	}{{"want", want}, {"tightened", tightened}} {
		rv := reflect.ValueOf(lit.v)
		for i := range rv.NumField() {
			if rv.Field(i).IsZero() {
				t.Errorf("%s.%s is zero: a dimension this test does not populate round-trips as "+
					"zero on BOTH sides of every comparison above, so the column could drop it "+
					"with nothing failing", lit.name, rv.Type().Field(i).Name)
			}
		}
	}
}

// TestWorker_UnadvertisedExprLimitsAreZero pins the pre-migration shape: a
// worker that reports nothing reads back as the zero value, which the scheduler
// resolves to the worker's compiled-in defaults rather than to "no limits".
// The migration's DEFAULT '{}' is what makes this a zero struct and not a scan
// error on an existing row.
func TestWorker_UnadvertisedExprLimitsAreZero(t *testing.T) {
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
	if (got.ExprLimits != store.WorkerExprLimits{}) {
		t.Errorf("ExprLimits = %+v, want the zero value", got.ExprLimits)
	}
}
