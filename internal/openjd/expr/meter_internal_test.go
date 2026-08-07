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
