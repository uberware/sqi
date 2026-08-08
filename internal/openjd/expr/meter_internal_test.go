// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"strings"
	"testing"
)

func TestSizeOf(t *testing.T) {
	tests := []struct {
		name string
		val  Value
		want int64
	}{
		{"int", Int(42), valueHeaderBytes},
		{"bool", Bool(true), valueHeaderBytes},
		{"null", Null(), valueHeaderBytes},
		{"empty string", String(""), valueHeaderBytes},
		{"ascii string", String("abc"), valueHeaderBytes + 3},
		// Bytes, not runes: memory is bytes. "é" is two bytes in UTF-8.
		{"non-ascii string", String("é"), valueHeaderBytes + 2},
		{"empty list", List(TInt, nil), valueHeaderBytes},
		{"list of two ints", List(TInt, []Value{Int(1), Int(2)}), valueHeaderBytes + 2*valueHeaderBytes},
		{"unresolved carries no payload", Unresolved(TString), valueHeaderBytes},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sizeOf(tt.val); got != tt.want {
				t.Errorf("sizeOf(%s) = %d; want %d", tt.val, got, tt.want)
			}
		})
	}
}

func TestCeilDiv256(t *testing.T) {
	// The specification's own worked example: 'a' * 100000 charges
	// ceil(100000/256) string-processing units. Note the spec's TEXT states
	// 392 and its total 393, but 256*390 = 99840 leaving 160, so the correct
	// value is 391 -- a spec arithmetic error, not a rule error. The reference
	// agrees with the rule (it reports 392 total = 1 call + 391).
	tests := []struct {
		in   int
		want int64
	}{{0, 0}, {1, 1}, {255, 1}, {256, 1}, {257, 2}, {100000, 391}}
	for _, tt := range tests {
		if got := ceilDiv256(tt.in); got != tt.want {
			t.Errorf("ceilDiv256(%d) = %d; want %d", tt.in, got, tt.want)
		}
	}
}

func TestMeter_OperationLimit(t *testing.T) {
	m := newMeter(defaultMemoryLimit, 3)
	for i := range 3 {
		if err := m.charge(1); err != nil {
			t.Fatalf("charge %d: unexpected error %v", i, err)
		}
	}
	err := m.charge(1)
	if !errors.Is(err, errOperationLimit) {
		t.Fatalf("charge past the limit = %v; want errOperationLimit", err)
	}
	if !strings.Contains(err.Error(), "3") {
		t.Errorf("error %q does not name the limit", err)
	}
}

// TestMeter_MemoryLimitIsExact pins alloc's bound check to "mem > memLimit"
// with no slack in either direction -- a real budget, not an approximate one.
// Written to catch a one-step weakening to "mem > memLimit+1" (Task 11's
// mutation-testing step 8.1): that mutation lets an allocation land exactly
// ONE byte over the limit without erroring, which this test's second case
// exercises directly.
func TestMeter_MemoryLimitIsExact(t *testing.T) {
	m := newMeter(valueHeaderBytes, defaultOperationLimit) // limit = 64
	if err := m.alloc(Int(1)); err != nil {                // mem becomes 64, AT the limit
		t.Fatalf("alloc exactly at the limit: unexpected error %v", err)
	}
	m2 := newMeter(valueHeaderBytes, defaultOperationLimit) // limit = 64
	if err := m2.alloc(String("x")); !errors.Is(err, errMemoryLimit) {
		// sizeOf(String("x")) = 65: exactly one byte over the limit.
		t.Fatalf("alloc one byte over the limit = %v; want errMemoryLimit", err)
	}
}

func TestMeter_MemoryLimitAndRelease(t *testing.T) {
	m := newMeter(valueHeaderBytes*2, defaultOperationLimit)
	if err := m.alloc(Int(1)); err != nil {
		t.Fatalf("first alloc: %v", err)
	}
	if err := m.alloc(Int(2)); err != nil {
		t.Fatalf("second alloc: %v", err)
	}
	if err := m.alloc(Int(3)); !errors.Is(err, errMemoryLimit) {
		t.Fatalf("third alloc = %v; want errMemoryLimit", err)
	}
	// Releasing frees room again: the bound is on LIVE memory, not on
	// cumulative allocation. This is the whole difference between section
	// 1.3.9 and a fixed per-operation ceiling like maxStringBytes.
	m.release(Int(1))
	m.release(Int(2))
	m.release(Int(3))
	if m.mem != 0 {
		t.Fatalf("after releasing everything, mem = %d; want 0", m.mem)
	}
	if err := m.alloc(Int(4)); err != nil {
		t.Fatalf("alloc after release: %v", err)
	}
}

// TestMeter_ChargeElements and TestMeter_ChargeBytes are not in the brief's
// own list, but are needed to exercise chargeElements/chargeBytes: nothing in
// this task wires them into evaluation (that lands in later tasks in this
// sub-project), and an untested, uncalled method fails golangci-lint's
// "unused" check. These stay black-box: they charge through the same public
// meter surface the other meter tests use, matching section 1.3.10 rules 2
// and 3 (element count, and ceil(len/256) string-processing units).
func TestMeter_ChargeElements(t *testing.T) {
	m := newMeter(defaultMemoryLimit, 5)
	if err := m.chargeElements(5); err != nil {
		t.Fatalf("chargeElements(5) at the limit: unexpected error %v", err)
	}
	if m.ops != 5 {
		t.Fatalf("ops = %d; want 5", m.ops)
	}
	m2 := newMeter(defaultMemoryLimit, 5)
	if err := m2.chargeElements(6); !errors.Is(err, errOperationLimit) {
		t.Fatalf("chargeElements(6) over the limit = %v; want errOperationLimit", err)
	}
}

func TestMeter_ChargeBytes(t *testing.T) {
	m := newMeter(defaultMemoryLimit, defaultOperationLimit)
	if err := m.chargeBytes(strings.Repeat("a", 100000)); err != nil {
		t.Fatalf("chargeBytes: unexpected error %v", err)
	}
	// The spec's own worked example (see TestCeilDiv256): ceil(100000/256) = 391.
	if m.ops != 391 {
		t.Fatalf("ops = %d; want 391", m.ops)
	}
	m2 := newMeter(defaultMemoryLimit, 1)
	if err := m2.chargeBytes(strings.Repeat("a", 257)); !errors.Is(err, errOperationLimit) {
		t.Fatalf("chargeBytes over the limit = %v; want errOperationLimit", err)
	}
}

func TestNewEvalCtx_DefaultsAreOn(t *testing.T) {
	ec := newEvalCtx("", nil, nil)
	if ec.m == nil {
		t.Fatal("newEvalCtx left the meter nil; a no-option evaluation must still be bounded")
	}
	if ec.m.memLimit != defaultMemoryLimit || ec.m.opLimit != defaultOperationLimit {
		t.Errorf("defaults = (%d, %d); want (%d, %d)",
			ec.m.memLimit, ec.m.opLimit, defaultMemoryLimit, defaultOperationLimit)
	}
}

func TestWithLimits_Override(t *testing.T) {
	ec := newEvalCtx("", nil, []Option{WithMemoryLimit(7), WithOperationLimit(9)})
	if ec.m.memLimit != 7 || ec.m.opLimit != 9 {
		t.Errorf("limits = (%d, %d); want (7, 9)", ec.m.memLimit, ec.m.opLimit)
	}
}

func TestWithLimits_NonPositiveIsAnError(t *testing.T) {
	// There is no unlimited mode. A value that reads like "off" must not BE
	// off in a package reachable from POST /api/v1/jobs, so a non-positive
	// limit is reported rather than silently meaning unbounded.
	for _, tt := range []struct {
		name string
		opt  Option
	}{
		{"zero memory", WithMemoryLimit(0)},
		{"negative memory", WithMemoryLimit(-1)},
		{"zero operations", WithOperationLimit(0)},
		{"negative operations", WithOperationLimit(-1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Eval("1", nil, TInt, tt.opt); err == nil {
				t.Fatal("a non-positive limit was accepted; want an error")
			}
		})
	}
}

// TestOperatorDispatch_CarriesTheCallersMeter proves an operator reaches
// callShape with the caller's meter rather than a zero-value evalCtx.
//
// Written to FAIL against the pre-task implementation, where applyBinary and
// applyUnary call callShape(evalCtx{}, ...) -- a context whose m field is nil.
// Without this, Task 3's charging would silently never fire for any operator,
// and every count assertion written against it would still pass.
func TestOperatorDispatch_CarriesTheCallersMeter(t *testing.T) {
	var seen *meter
	probe := Shape{
		Params: []Type{TInt, TInt},
		Ret:    TInt,
		FnCtx: func(ec evalCtx, args []Value) (Value, error) {
			seen = ec.m
			return Int(args[0].AsInt() + args[1].AsInt()), nil
		},
	}
	ec := newEvalCtx("", nil, nil)
	b := bindings{}
	if _, err := callShape(ec, probe, b, []Value{Int(1), Int(2)}); err != nil {
		t.Fatalf("callShape: %v", err)
	}
	if seen == nil {
		t.Fatal("callShape received a nil meter; the evaluation context was not threaded")
	}
	if seen != ec.m {
		t.Fatal("callShape received a different meter than the caller's")
	}
}

// TestApplyBinary_TakesAContext is a compile-level assertion: applyBinary and
// applyUnary must accept an evalCtx. It fails to build before this task.
func TestApplyBinary_TakesAContext(t *testing.T) {
	ec := newEvalCtx("", nil, nil)
	v, err := applyBinary(ec, OpAdd, Int(1), Int(2))
	if err != nil {
		t.Fatalf("applyBinary: %v", err)
	}
	if v.AsInt() != 3 {
		t.Errorf("1 + 2 = %d; want 3", v.AsInt())
	}
	n, err := applyUnary(ec, OpNeg, Int(5))
	if err != nil {
		t.Fatalf("applyUnary: %v", err)
	}
	if n.AsInt() != -5 {
		t.Errorf("-5 = %d; want -5", n.AsInt())
	}
}

// opsFor evaluates src with a generous budget and returns the operation count.
func opsFor(t *testing.T, src string) int64 {
	t.Helper()
	e, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	ec := newEvalCtx(src, MapSymbols(nil), nil)
	if _, err := evalNode(e.root, ec, TAny, 0); err != nil {
		t.Fatalf("eval(%q): %v", src, err)
	}
	return ec.m.ops
}

func TestOperationCount_RuleOne(t *testing.T) {
	tests := []struct {
		src  string
		want int64
	}{
		// A literal is not a call. Confirmed against the reference: 0.
		{"1", 0},
		{"'abc'", 0},
		// One operator is one call.
		{"1 + 2", 1},
		{"-1", 1},
		// Two operators are two calls.
		{"1 + 2 + 3", 2},
		// The ==/!= fast path (applyBinary) returns before matchShapes ever
		// runs, so it never reaches callShape's charge -- it needs its own,
		// separately verified below and here end-to-end.
		{"1 == 2", 1},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			if got := opsFor(t, tt.src); got != tt.want {
				t.Errorf("ops(%q) = %d; want %d", tt.src, got, tt.want)
			}
		})
	}
}

func TestOperationLimit_IsReported(t *testing.T) {
	_, err := Eval("1 + 1 + 1 + 1 + 1", nil, TInt, WithOperationLimit(2))
	if !errors.Is(err, errOperationLimit) {
		t.Fatalf("evaluating past the operation limit = %v; want errOperationLimit", err)
	}
}

func TestOperationLimit_UnderTheLimitSucceeds(t *testing.T) {
	v, err := Eval("1 + 1 + 1 + 1 + 1", nil, TInt, WithOperationLimit(4))
	if err != nil {
		t.Fatalf("evaluating within the operation limit: %v", err)
	}
	if v.AsInt() != 5 {
		t.Errorf("= %d; want 5", v.AsInt())
	}
}

// TestOperationCount_EqualityFastPath covers ops.go's ==/!= fast path in
// applyBinary directly, exercising the charge site the source-driven
// TestOperationCount_RuleOne case above cannot reach on its own: the
// UNRESOLVED-operand branch. That branch returns Unresolved(TBool) before
// matchShapes/callShape ever runs, exactly like the resolved comparison a few
// lines below it, so it needs its own charge -- and this is the only way to
// drive an unresolved operand into it, since there is no source-level way to
// produce a placeholder value without a bound symbol.
//
// Calling applyBinary directly with hand-built Values follows the precedent
// TestApplyBinary_TakesAContext and TestOperatorDispatch_CarriesTheCallersMeter
// already set in this file.
func TestOperationCount_EqualityFastPath(t *testing.T) {
	tests := []struct {
		name string
		op   Op
		l, r Value
	}{
		{"resolved ==", OpEq, Int(1), Int(2)},
		{"resolved !=", OpNe, Int(1), Int(2)},
		{"unresolved ==, left operand unresolved", OpEq, Unresolved(TInt), Int(2)},
		{"unresolved !=, right operand unresolved", OpNe, Int(1), Unresolved(TInt)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ec := newEvalCtx("", nil, nil)
			if _, err := applyBinary(ec, tt.op, tt.l, tt.r); err != nil {
				t.Fatalf("applyBinary(%s, %s, %s): %v", tt.op, tt.l, tt.r, err)
			}
			if ec.m.ops != 1 {
				t.Errorf("ops = %d; want exactly 1", ec.m.ops)
			}
		})
	}
}

// TestOperationCount_UnresolvedOperandSites covers Task 3's ruling directly:
// when an operand is unresolved, callFunction, applyBinary's general
// (matchShapes) branch, and applyUnary all return before ever reaching
// callShape, so each needs its OWN rule-1 charge -- and the assertion that
// matters is the EXACT count. A future edit that charges twice (once here,
// once by mistakenly still calling into callShape) or drops the charge
// entirely would both slip past a test that only checked "> 0".
//
// TestOperationCount_EqualityFastPath above covers the fourth such site,
// applyBinary's OpEq/OpNe branch; this test covers the remaining three named
// in the brief's Step 5.
// balanceOf evaluates src and returns (live bytes remaining, size of result).
func balanceOf(t *testing.T, src string) (live, resultSize int64) {
	t.Helper()
	e, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	ec := newEvalCtx(src, MapSymbols(nil), nil)
	v, err := evalNode(e.root, ec, TAny, 0)
	if err != nil {
		t.Fatalf("eval(%q): %v", src, err)
	}
	return ec.m.mem, sizeOf(v)
}

// TestMemory_ZeroBalance is the leak detector.
//
// After a top-level evaluation, the only value still live must be the result.
// The identity catches drift in BOTH directions -- a missed release inflates it,
// a double-release deflates it -- which is exactly what a release discipline in
// a language with no destructors needs, and what no hand-picked set of
// allocation assertions provides.
func TestMemory_ZeroBalance(t *testing.T) {
	srcs := []string{
		"1",
		"1 + 2",
		"1 + 2 + 3",
		"-1",
		"'abc' + 'def'",
		"[1,2,3]",
		"[1,2,3][0]",
		"[1,2,3][0:2]",
		"[1,2] + [3]",
		"[x * 2 for x in [1,2,3]]",
		"sum([1,2,3])",
		"1 if True else 2",
		"True and False",
		"len('abc')",
		"sorted([3,1,2])",
		"'a,b,c'.split(',')",
	}
	for _, src := range srcs {
		t.Run(src, func(t *testing.T) {
			live, want := balanceOf(t, src)
			if live != want {
				t.Errorf("live memory after eval = %d; want %d (the result's own size).\n"+
					"  A larger number means a value was never released; a smaller one means\n"+
					"  something was released twice.", live, want)
			}
		})
	}
}

func TestMemoryLimit_IsReported(t *testing.T) {
	// A list of 100 ints is 101 headers = 6464 bytes at valueHeaderBytes = 64.
	_, err := Eval("range(100)", nil, TAny, WithMemoryLimit(valueHeaderBytes*10))
	if !errors.Is(err, errMemoryLimit) {
		t.Fatalf("evaluating past the memory limit = %v; want errMemoryLimit", err)
	}
}

// TestMemoryLimit_CatchesCumulativeWorkThatTheFloorDoesNot is the whole
// justification for section 1.3.9 over limits.go's fixed maxStringBytes.
//
// Each individual repetition stays under the per-operation ceiling; what breaks
// the budget is holding many of them live at once. A fixed per-operation bound
// cannot see this at all, which is why the tracker recorded that nested
// repetition "grows ~91 MB per level with no cumulative accounting".
func TestMemoryLimit_CatchesCumulativeWorkThatTheFloorDoesNot(t *testing.T) {
	// 200 strings of 100_000 bytes each is ~20 MB live, well under
	// maxStringBytes (10 MB) for any SINGLE operation.
	const src = "['a' * 100000 for i in range(200)]"
	if _, err := Eval(src, nil, TAny, WithMemoryLimit(5_000_000)); !errors.Is(err, errMemoryLimit) {
		t.Fatalf("%s under a 5 MB budget = %v; want errMemoryLimit", src, err)
	}
	// The same expression succeeds under a budget that fits it, proving the
	// failure above is the BUDGET and not the floor.
	if _, err := Eval(src, nil, TAny, WithMemoryLimit(100_000_000)); err != nil {
		t.Fatalf("%s under a 100 MB budget: %v", src, err)
	}
}

func TestOperationCount_UnresolvedOperandSites(t *testing.T) {
	t.Run("applyBinary general branch", func(t *testing.T) {
		ec := newEvalCtx("", nil, nil)
		if _, err := applyBinary(ec, OpAdd, Unresolved(TInt), Int(2)); err != nil {
			t.Fatalf("applyBinary(OpAdd, unresolved, 2): %v", err)
		}
		if ec.m.ops != 1 {
			t.Errorf("ops = %d; want exactly 1", ec.m.ops)
		}
	})
	t.Run("applyUnary", func(t *testing.T) {
		ec := newEvalCtx("", nil, nil)
		if _, err := applyUnary(ec, OpNeg, Unresolved(TInt)); err != nil {
			t.Fatalf("applyUnary(OpNeg, unresolved): %v", err)
		}
		if ec.m.ops != 1 {
			t.Errorf("ops = %d; want exactly 1", ec.m.ops)
		}
	})
	t.Run("callFunction", func(t *testing.T) {
		ec := newEvalCtx("", nil, nil)
		if _, err := callFunction(ec, "int", []Value{Unresolved(TInt)}, false); err != nil {
			t.Fatalf("callFunction(int, unresolved): %v", err)
		}
		if ec.m.ops != 1 {
			t.Errorf("ops = %d; want exactly 1", ec.m.ops)
		}
	})
}
