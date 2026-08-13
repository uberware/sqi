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

// TestDeadline_ClockIsSampledNotReadOnEveryCharge pins that charge SAMPLES the
// clock rather than reading it every time, and it exists because every other
// test in this file passes unchanged under an implementation that reads it on
// every single charge.
//
// That is not a hypothetical worry. Without this test, a future reader who
// notices that the first-charge branch is what actually catches the expired
// deadlines those tests assert on could delete the sinceCheck machinery as
// dead weight — "the interval never fires" — keep the whole suite green, and
// put a time.Now() on the hottest function in the package. This repository has
// already paid for one unmetered walk on a server path (~9 minutes of CPU per
// anonymous request), and a syscall per charge is the same shape of mistake:
// invisible to correctness tests, expensive in production. If this test fails,
// the question to ask is not "how do I relax the bound" but "why is the clock
// being read this often".
//
// It asserts on the SURVIVING path — an ample deadline, so both halves run to
// completion — because that is where the cost is paid. An evaluation that
// trips reads the clock once and stops.
func TestDeadline_ClockIsSampledNotReadOnEveryCharge(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)

	// Part one drives the meter directly, so both counts are EXACT: charges are
	// counted by the loop and reads by the clock.
	t.Run("meter", func(t *testing.T) {
		const charges = 100_000
		reads := 0
		m := newMeter(1<<40, 1<<40)
		m.deadline = base.Add(time.Hour)
		m.now = func() time.Time { reads++; return base }

		for i := range charges {
			if err := m.charge(1); err != nil {
				t.Fatalf("charge %d: %v", i, err)
			}
		}
		// Expected: one read on the first charge plus one per interval, ~98.
		// The bound is deliberately loose — this is an order-of-magnitude
		// claim, not a pin on deadlineCheckInterval's exact value — but a
		// per-charge read scores 100,000 and fails it by three orders.
		if reads > charges/100 {
			t.Errorf("clock reads = %d for %d charges; want at most %d. The check "+
				"must be SAMPLED (every deadlineCheckInterval charges), not read "+
				"on every charge: charge is the hot path of every expression",
				reads, charges, charges/100)
		}
		if reads == 0 {
			t.Error("clock reads = 0; the deadline was never checked at all")
		}
	})

	// Part two runs a real expression end to end. It cannot count charges, so
	// it uses the section 1.3.10 operation count as a proxy — sound here
	// because this comprehension's operations are almost all one-per-charge
	// element charges, so ops is within a whisker of the charge count.
	t.Run("expression", func(t *testing.T) {
		reads := 0
		_, ops, err := EvalWithMetrics(
			`[x * 2 for x in range(100000)]`, MapSymbols{}, TAny,
			WithOperationLimit(100_000_000),
			WithMemoryLimit(1_000_000_000),
			WithDeadline(base.Add(time.Hour)),
			WithClock(func() time.Time { reads++; return base }),
		)
		if err != nil {
			t.Fatalf("Eval: %v", err)
		}
		if reads > int(ops)/100 {
			t.Errorf("clock reads = %d for %d operations; want at most %d",
				reads, ops, int(ops)/100)
		}
	})
}

// TestDeadline_ReserveRefusesBeforeMaterializing pins the check in
// meter.reserve, which is the one that bounds the SIZE of the window rather
// than its length in charges.
//
// reserve is called with a count in hand immediately before a large
// materialization — repeatList, repeatString, rangeList, a comprehension's
// iterable, a range_expr coercion. Checking the deadline there is what turns
// "the biggest construction in the sampling window runs to completion and is
// caught on the charge after it" into "it never starts". The expressions below
// are chosen so that the FIRST metered thing each does is a reservation, and
// each would otherwise build tens of megabytes before any charge noticed.
func TestDeadline_ReserveRefusesBeforeMaterializing(t *testing.T) {
	for _, src := range []string{
		`[0] * 1000000`,
		`"x" * 10000000`,
		`range(1000000)`,
		`sorted([1] + range_expr("1-1000000"))`,
	} {
		t.Run(src, func(t *testing.T) {
			base := time.Unix(1_700_000_000, 0)
			_, err := Eval(
				src, MapSymbols{}, TAny,
				WithOperationLimit(100_000_000),
				WithMemoryLimit(1_000_000_000),
				WithDeadline(base.Add(-time.Second)), // already expired
				WithClock(func() time.Time { return base }),
			)
			if !errors.Is(err, ErrDeadlineExceeded) {
				t.Fatalf("err = %v, want ErrDeadlineExceeded", err)
			}
		})
	}
}

// TestDeadline_OperationLimitBeatsTheDeadline pins the precedence in charge and
// in reserve: when ONE call breaches both bounds, it reports the operation
// limit.
//
// The order is a deliberate design property, not an accident of statement
// order. An operation breach is deterministic and is a property of the template
// (a caller maps it to 422); a deadline breach depends on machine load and says
// only that the server gave up (503). Reporting the deterministic one in
// preference keeps an invalid template from being blamed on a busy host, where
// it would look transient and be retried forever.
//
// It drives the meter directly rather than an expression, because "one call
// breaches both" is not something an expression can be relied on to arrange:
// the deadline is checked on the FIRST charge, and the first charge of almost
// every expression is a single rule-1 operation that no realistic operation
// limit refuses. The precedence would then never be exercised at all, and a
// test written against an expression would pass while asserting nothing.
func TestDeadline_OperationLimitBeatsTheDeadline(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)

	newBreachingMeter := func() *meter {
		m := newMeter(1<<40, 10)
		m.deadline = base.Add(-time.Hour) // long gone
		m.now = func() time.Time { return base }
		return m
	}

	t.Run("charge", func(t *testing.T) {
		err := newBreachingMeter().charge(100) // 100 operations against a limit of 10
		if !errors.Is(err, errOperationLimit) {
			t.Fatalf("err = %v, want errOperationLimit", err)
		}
		if errors.Is(err, ErrDeadlineExceeded) {
			t.Errorf("err = %v; a charge breaching BOTH bounds must report the "+
				"deterministic operation limit, not the wall clock", err)
		}
	})

	t.Run("reserve", func(t *testing.T) {
		err := newBreachingMeter().reserve(100)
		if !errors.Is(err, errOperationLimit) {
			t.Fatalf("err = %v, want errOperationLimit", err)
		}
		if errors.Is(err, ErrDeadlineExceeded) {
			t.Errorf("err = %v; a reservation that cannot fit the operation "+
				"budget must say so, whatever the clock reads", err)
		}
	})
}

// TestDeadline_MessageDoesNotRepeatItself pins the rendered text, because the
// error surfaces verbatim in an HTTP response body. The wrap must ADD
// information (how far past the deadline the check landed), not restate
// ErrDeadlineExceeded's own sentence — an earlier revision rendered as
// "expr: evaluation deadline exceeded: evaluation exceeded its wall-clock
// deadline".
func TestDeadline_MessageDoesNotRepeatItself(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)

	_, err := Eval(
		`1 + 1`, MapSymbols{}, TAny,
		WithDeadline(base.Add(-2*time.Second)),
		WithClock(func() time.Time { return base }),
	)
	if !errors.Is(err, ErrDeadlineExceeded) {
		t.Fatalf("err = %v, want ErrDeadlineExceeded", err)
	}
	got := err.Error()
	if strings.Count(got, "deadline") != 1 {
		t.Errorf("err = %q; %q appears %d times, want exactly 1 — the wrap must "+
			"add information, not repeat the sentinel", got, "deadline",
			strings.Count(got, "deadline"))
	}
	if !strings.Contains(got, "2s") {
		t.Errorf("err = %q; want the overrun (2s) named", got)
	}
}

// TestDeadline_TripsWithRealClock is the only test in this package that runs
// PRODUCTION'S OWN CLOCK, and that is the entire reason it exists.
//
// Every other test here passes WithClock, so every other test exercises the
// m.now != nil branch of deadlinePassed and none of them ever reaches the
// m.now == nil default that production takes on every evaluation. That default
// is one line, and one line is exactly the size of the mistake nothing else
// here can see — a clock that is never consulted, or consulted against the
// wrong field, in the only configuration that ships.
//
// An earlier revision of this comment defended the test on a different and
// FALSE ground: that "a version of this feature where WithDeadline sets a field
// nothing ever reads would pass all three tests above". It would not —
// TestDeadline_TripsWithFakeClock would get a nil error and fail. The argument
// above is the true one, and the distinction matters because the design
// predicts this test will one day be proposed for deletion as slow or flaky by
// someone who has not read the reasoning. It is neither: it injects nothing,
// sets an already-expired deadline, and an expired deadline trips on the first
// check. Do not "fix" it by injecting a clock — that deletes the only coverage
// of the shipping path while leaving a green test behind.
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
