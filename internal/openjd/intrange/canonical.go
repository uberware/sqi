// SPDX-License-Identifier: AGPL-3.0-or-later

package intrange

import (
	"cmp"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Canonical returns the canonical form (Template Schemas §3.4.1.1.1) of the
// range expression whose elements are rs: a string determined by its list of
// values alone, so element order, how values are split into elements, and
// white space do not show in it. The list is split from the left into runs of
// a constant difference; a run of three or more values is written "x-y" (step
// 1) or "x-y:s", any shorter run one value at a time, and the runs are joined
// by "," with no white space.
//
// It is computed from the elements' arithmetic progressions, never by listing
// the values: a run that follows a progression jumps to its end in one move,
// so the work is proportional to the number of elements however many values
// they name.
//
// ok is false when two elements' spans overlap (see [Overlaps]): their union
// is then not a concatenation of progressions, and settling it needs the
// values, so the caller decides what to do. ok is also false for an empty rs.
func Canonical(rs []Range) (string, bool) {
	ps, disjoint := sortedProgressions(rs)
	if !disjoint || len(ps) == 0 {
		return "", false
	}
	w := walker{ps: ps}
	var b strings.Builder
	pos, more := position{}, true
	for more {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		pos, more = w.writeRun(&b, pos)
	}
	return b.String(), true
}

// CanonicalValues returns the canonical form of a set of integers, given in
// any order and possibly repeated. Distinct values never overlap, so it always
// has an answer; vals is not modified.
func CanonicalValues(vals []int) string {
	sorted := slices.Clone(vals)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	rs := make([]Range, len(sorted))
	for i, v := range sorted {
		rs[i] = Range{Start: v, End: v, Step: 1}
	}
	c, _ := Canonical(rs)
	return c
}

// Overlaps reports whether two elements of a range expression overlap, as
// Template Schemas §3.4.1.1.1 defines it: an element's SPAN runs from the
// smallest to the largest value in its set, not between its written endpoints,
// and two elements overlap when their spans share an integer, even when their
// sets share no value ("1-9:2,2-10:2" overlaps; "1-10:4,10-15" does not). It
// sorts once and compares neighbors, so it costs O(n log n) in the number of
// elements however many values they name.
func Overlaps(rs []Range) bool {
	_, disjoint := sortedProgressions(rs)
	return !disjoint
}

// sortedProgressions normalizes rs to progressions sorted by start and reports
// whether their spans are pairwise disjoint. Sorted by start, a span overlaps
// an earlier one exactly when it meets its immediate predecessor's.
func sortedProgressions(rs []Range) ([]progression, bool) {
	ps := make([]progression, len(rs))
	for i, r := range rs {
		ps[i] = progressionOf(r)
	}
	slices.SortFunc(ps, func(a, b progression) int { return cmp.Compare(a.start, b.start) })
	for i := 1; i < len(ps); i++ {
		if ps[i].start <= ps[i-1].last() {
			return ps, false
		}
	}
	return ps, true
}

// progression is one element as an ascending arithmetic progression: start,
// start+step, ..., start+steps*step. step is positive; for a single value it is
// 1 and steps is 0. The arithmetic is unsigned so that a span reaching across
// the whole int range is exact.
type progression struct {
	start int
	step  uint64
	steps uint64
}

func (p progression) at(k uint64) int { return fromBits(bits(p.start) + k*p.step) }

// bits reinterprets an int as its two's-complement bit pattern. The walk does
// its span and difference arithmetic in uint64, where End - Start and the
// difference of two ints are exact even when they exceed the int range, and
// converts back only values that are themselves in range.
func bits(n int) uint64 { return uint64(n) } //nolint:gosec // deliberate two's-complement reinterpretation; see doc comment

// fromBits is bits' inverse, for a result the caller knows is in range.
func fromBits(u uint64) int { return int(u) } //nolint:gosec // inverse of bits, applied only to in-range values

func (p progression) last() int { return p.at(p.steps) }

// progressionOf normalizes one element. A step pointing away from the end
// leaves the start alone (the specification's set always holds the start); a
// negative step is turned around so the progression ascends.
func progressionOf(r Range) progression {
	step := r.Step
	if step == 0 {
		step = 1
	}
	switch {
	case step > 0 && r.End > r.Start:
		s := bits(step)
		return progression{start: r.Start, step: s, steps: (bits(r.End) - bits(r.Start)) / s}
	case step < 0 && r.End < r.Start:
		s := bits(^step) + 1 // |step|, exact for math.MinInt
		n := (bits(r.Start) - bits(r.End)) / s
		return progression{start: fromBits(bits(r.Start) - n*s), step: s, steps: n}
	}
	return progression{start: r.Start, step: 1}
}

// position is one value of the walk: step k of progression p.
type position struct {
	p int
	k uint64
}

type walker struct{ ps []progression }

func (w walker) value(at position) int { return w.ps[at.p].at(at.k) }

func (w walker) next(at position) (position, bool) {
	if at.k < w.ps[at.p].steps {
		return position{at.p, at.k + 1}, true
	}
	if at.p+1 < len(w.ps) {
		return position{at.p + 1, 0}, true
	}
	return position{}, false
}

// writeRun writes the run that starts at from and returns where the next run
// starts.
func (w walker) writeRun(b *strings.Builder, from position) (position, bool) {
	first := w.value(from)
	b.WriteString(strconv.Itoa(first))
	second, ok := w.next(from)
	if !ok {
		return position{}, false
	}
	// Values ascend, so the difference is positive. It may not fit in 64
	// signed bits, and then no run of three can have it as its step.
	d := bits(w.value(second)) - bits(first)
	if d > math.MaxInt64 {
		return second, true
	}
	last, long := second, false
	for {
		p := w.ps[last.p]
		if p.step == d && last.k < p.steps {
			last.k, long = p.steps, true
			continue
		}
		nx, ok := w.next(last)
		if !ok || bits(w.value(nx))-bits(w.value(last)) != d {
			break
		}
		last, long = nx, true
	}
	if !long {
		return second, true
	}
	b.WriteByte('-')
	b.WriteString(strconv.Itoa(w.value(last)))
	if d != 1 {
		b.WriteByte(':')
		b.WriteString(strconv.FormatUint(d, 10))
	}
	return w.next(last)
}
