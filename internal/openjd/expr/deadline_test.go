// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestDeadline_TripsWithFakeClock drives the deadline with an injected clock,
// so the test is deterministic and fast. It is the logic test.
func TestDeadline_TripsWithFakeClock(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	now := start
	// Every clock read advances a second, so any expression performing enough
	// charges to trigger a check will blow a one-second budget.
	clock := func() time.Time { now = now.Add(time.Second); return now }

	// A comprehension over a range performs many charges, so the periodic
	// check is reached.
	_, err := Eval(
		`[x * 2 for x in range(100000)]`, MapSymbols{}, TAny,
		WithOperationLimit(100_000_000),
		WithMemoryLimit(100_000_000),
		WithDeadline(start.Add(time.Second)),
		WithClock(clock),
	)
	if err == nil {
		t.Fatal("evaluation completed; want a deadline error")
	}
	if !errors.Is(err, ErrDeadlineExceeded) {
		t.Fatalf("err = %v, want ErrDeadlineExceeded", err)
	}
}

// TestDeadline_NotTrippedWhenAmple pins the other direction: a deadline far in
// the future must not affect an evaluation at all.
func TestDeadline_NotTrippedWhenAmple(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return start }

	v, err := Eval(
		`1 + 1`, MapSymbols{}, TAny,
		WithDeadline(start.Add(time.Hour)),
		WithClock(clock),
	)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if v.String() != "2" {
		t.Errorf("got %s, want 2", v.String())
	}
}

// TestDeadline_ZeroMeansNoDeadline pins the default: an evaluation with no
// deadline set never consults the clock and never trips.
func TestDeadline_ZeroMeansNoDeadline(t *testing.T) {
	called := false
	clock := func() time.Time { called = true; return time.Unix(0, 0) }

	if _, err := Eval(
		`[x for x in range(10000)]`, MapSymbols{}, TAny,
		WithOperationLimit(100_000_000),
		WithMemoryLimit(100_000_000),
		WithClock(clock),
	); err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if called {
		t.Error("the clock was read with no deadline set; the zero deadline must " +
			"short-circuit before any clock read, so the hot path pays nothing")
	}
}

// TestDeadline_TripsAnExpressionTooShortToReachTheInterval pins the
// first-charge check, which is NOT a rounding detail and is the one behavior
// here that the four contract tests cannot see.
//
// deadlineCheckInterval counts charge CALLS, and section 1.3.10 prices 256
// bytes at one operation — so byte-heavy work is charged in a handful of bulk
// calls, not many small ones. Measured with an every-1024-charges-only check,
// every expression below read the clock ZERO times and ran to completion with
// an already-expired deadline, including '("x" * 900000).title()', the ~57 ms
// case the whole backstop was designed against.
//
// That matters beyond one expression: a template is walked position by
// position, each position a separate evaluation with a FRESH meter, so without
// this a template built from small expressions could run arbitrarily far past
// an expired deadline while every meter sat below the interval forever. If this
// test is ever "simplified" away, that hole reopens silently.
func TestDeadline_TripsAnExpressionTooShortToReachTheInterval(t *testing.T) {
	for _, src := range []string{
		`1 + 1`,
		`len([1,2,3])`,
		`[x for x in range(50)]`,
		`("x" * 900000).title()`,
	} {
		t.Run(src, func(t *testing.T) {
			base := time.Unix(1_700_000_000, 0)
			reads := 0
			clock := func() time.Time { reads++; return base }

			_, err := Eval(
				src, MapSymbols{}, TAny,
				WithOperationLimit(100_000_000),
				WithMemoryLimit(100_000_000),
				WithDeadline(base.Add(-time.Second)), // already expired
				WithClock(clock),
			)
			if !errors.Is(err, ErrDeadlineExceeded) {
				t.Fatalf("err = %v, want ErrDeadlineExceeded after %d clock reads; "+
					"an expression below deadlineCheckInterval charges must still "+
					"sample the clock on its first charge", err, reads)
			}
			if reads != 1 {
				t.Errorf("clock reads = %d, want exactly 1: the first charge trips "+
					"an expired deadline, and nothing should run after it", reads)
			}
		})
	}
}

// TestDeadline_TripsWithRealClock is the wiring test, and it uses the REAL
// clock on purpose.
//
// Every other test here injects a clock, which proves the comparison logic and
// proves nothing about whether a deadline is actually plumbed through to the
// meter. A version of this feature where WithDeadline sets a field nothing ever
// reads would pass all three tests above. This one cannot pass that way: it
// sets an already-expired deadline and takes the real time.
//
// Do not "fix" this test by injecting a clock. Its slowness is not the point
// and it is not slow — an expired deadline trips at the first check.
func TestDeadline_TripsWithRealClock(t *testing.T) {
	_, err := Eval(
		`[x * 2 for x in range(100000)]`, MapSymbols{}, TAny,
		WithOperationLimit(100_000_000),
		WithMemoryLimit(100_000_000),
		WithDeadline(time.Now().Add(-time.Second)), // already expired
	)
	if err == nil {
		t.Fatal("evaluation completed with an expired deadline; WithDeadline is " +
			"probably not reaching the meter at all")
	}
	if !errors.Is(err, ErrDeadlineExceeded) {
		t.Fatalf("err = %v, want ErrDeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "deadline") {
		t.Errorf("err = %q; the message should say what happened", err)
	}
}
