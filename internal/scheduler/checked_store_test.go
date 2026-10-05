// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"testing"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/store/sqlite"
)

// newCheckedFake returns a fake store that asserts invariant I3 ("an active
// usage claim exists iff its attempt is open") when the test ends, so every
// scheduler test that uses it checks I3 for free (spec §8.3).
func newCheckedFake(t *testing.T) *fake.Store {
	t.Helper()
	st := fake.New()
	t.Cleanup(func() {
		if v := st.ClaimInvariantViolations(); len(v) != 0 {
			t.Errorf("invariant I3 violated at test end (fake store): active claims on closed attempts or terminal tasks: %v", v)
		}
	})
	return st
}

// checkSQLiteClaimsAtEnd registers the same end-of-test I3 assertion as
// [newCheckedFake] for a SQLite store. Register it after the store's Close
// cleanup: cleanups run last-in first-out, so the check then sees an open
// store. It uses context.Background because t.Context is already canceled when
// cleanups run.
func checkSQLiteClaimsAtEnd(t *testing.T, st *sqlite.Store) {
	t.Helper()
	t.Cleanup(func() {
		v, err := st.ClaimInvariantViolations(context.Background())
		if err != nil {
			t.Errorf("I3 check at test end (SQLite store): %v", err)
			return
		}
		if len(v) != 0 {
			t.Errorf("invariant I3 violated at test end (SQLite store): active claims on closed attempts or terminal tasks: %v", v)
		}
	})
}

// fixtureWriter is the test fixture surface both concrete stores keep and
// store.Store does not: writes with no production caller, kept until H4b
// decides their fate.
type fixtureWriter interface {
	UpdateTaskStatus(ctx context.Context, id string, status store.TaskStatus) error
	UpdateTaskAttempt(ctx context.Context, attempt store.TaskAttempt) (store.TaskAttempt, error)
	SetTaskFailureReason(ctx context.Context, id, reason string) error
}

// fixtures returns st's fixture writes; st must be a concrete store, not a
// wrapper around one.
func fixtures(t *testing.T, st store.Store) fixtureWriter {
	t.Helper()
	fw, ok := st.(fixtureWriter)
	if !ok {
		t.Fatalf("store %T has no fixture writes", st)
	}
	return fw
}
