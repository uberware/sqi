// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"testing"

	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/store/storetest"
)

// newCheckedFake returns a fake store that asserts invariant I3 ("an active
// usage claim exists iff its attempt is open") when the test ends, so every
// scheduler test that uses it checks I3.
func newCheckedFake(t *testing.T) *fake.Store {
	t.Helper()
	st := fake.New()
	checkClaimsAtEnd(t, st)
	return st
}

// checkClaimsAtEnd registers an end-of-test I3 assertion on st. For a store
// that must be closed, register it after the store's Close cleanup: cleanups
// run last-in first-out, so the check then sees an open store. It uses
// context.Background because t.Context is already canceled when cleanups run.
func checkClaimsAtEnd(t *testing.T, st storetest.InvariantChecker) {
	t.Helper()
	t.Cleanup(func() {
		v, err := st.ClaimInvariantViolations(context.Background())
		if err != nil {
			t.Errorf("I3 check at test end (%T): %v", st, err)
			return
		}
		if len(v) != 0 {
			t.Errorf("invariant I3 violated at test end (%T): active claims on closed attempts or terminal tasks: %v", st, v)
		}
	})
}
