// SPDX-License-Identifier: AGPL-3.0-or-later

// Package intrange implements the OpenJD base specification's <IntRangeExpr>
// grammar (2023-09 Template Schemas, section 3.4.1.1.1): the succinct
// "1,3,7-10:2" form used for frame ranges.
//
// It is a leaf shared by two callers that DISAGREE about what to accept, which
// is why Parse is spec-faithful and ParseWithPolicy exists:
//
//   - internal/openjd/expr (the EXPR extension) needs the spec's behavior, and
//     uses Parse.
//   - internal/openjd (task-parameter expansion) is stricter than the spec in
//     two ways that predate this package: it rejects a range whose start
//     exceeds its end (the spec makes "1 - -1" the single value [1]) and it
//     rejects a negative step (the spec's set formula defines one). It also
//     orders values first-seen rather than increasing, which it does in its own
//     expansion code rather than here.
//
// Those divergences are deliberate to preserve, NOT bugs to fix in passing:
// changing them changes which job templates are accepted and the order in which
// tasks are generated. They are recorded in the sub-project B2 design document.
package intrange

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Range is one element of a range expression: a stepped sequence of integers
// anchored at Start. Step is non-zero and may be negative.
type Range struct {
	Start int
	End   int
	Step  int
}

// Policy narrows what ParseWithPolicy accepts below what the specification
// permits. The zero Policy accepts everything the specification does.
type Policy struct {
	// PositiveStepOnly rejects a zero or negative step.
	PositiveStepOnly bool
	// AscendingOnly rejects an element whose start exceeds its end.
	AscendingOnly bool
}

// String renders the range in the grammar's own notation, for test names and
// diagnostics.
func (r Range) String() string {
	if r.Start == r.End {
		return strconv.Itoa(r.Start)
	}
	if r.Step == 1 {
		return fmt.Sprintf("%d-%d", r.Start, r.End)
	}
	return fmt.Sprintf("%d-%d:%d", r.Start, r.End, r.Step)
}

// Count returns how many integers the range yields, computed arithmetically
// without allocating — the allocation-free equivalent of len(r.Iterate()).
//
// The specification's set is {x} union {x+mn : m in Z+, x+mn <= y if n > 0,
// x+mn >= y if n < 0}, so the start is ALWAYS a member and the count is never
// zero. If the span overflows it saturates to math.MaxInt, which exceeds any
// caller's bound.
func (r Range) Count() int {
	step := r.Step
	if step == 0 {
		step = 1
	}
	span := r.End - r.Start
	if (span > 0) != (step > 0) {
		// The end is on the far side of the start from the step's direction, so
		// no term beyond the start qualifies.
		return 1
	}
	if span > 0 && r.End < r.Start || span < 0 && r.End > r.Start {
		// Subtraction overflowed; the range is far larger than any bound.
		return math.MaxInt
	}
	if span < 0 {
		span, step = -span, -step
	}
	return span/step + 1
}

// Iterate yields every integer in the range, in the order the step defines.
func (r Range) Iterate() []int {
	step := r.Step
	if step == 0 {
		step = 1
	}
	out := make([]int, 0, min(r.Count(), 1024))
	if step > 0 {
		for v := r.Start; v <= r.End || v == r.Start; v += step {
			out = append(out, v)
			if v > math.MaxInt-step {
				break
			}
		}
		return out
	}
	for v := r.Start; v >= r.End || v == r.Start; v += step {
		out = append(out, v)
		if v < math.MinInt-step {
			break
		}
	}
	return out
}

// Parse reads a range expression into its elements, accepting everything the
// specification permits. It does not expand them; a caller decides its own
// ordering, de-duplication and size bound.
func Parse(s string) ([]Range, error) { return ParseWithPolicy(s, Policy{}) }

// ParseWithPolicy is Parse, restricted by p.
func ParseWithPolicy(s string, p Policy) ([]Range, error) {
	if strings.TrimSpace(s) == "" {
		return nil, errors.New("range expression is empty")
	}
	var ranges []Range
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		r, err := parseElement(part, p)
		if err != nil {
			return nil, err
		}
		ranges = append(ranges, r)
	}
	if len(ranges) == 0 {
		return nil, errors.New("range expression is empty")
	}
	return ranges, nil
}

// parseElement reads one comma-separated element: a bare integer, a range, or a
// range with a step.
func parseElement(s string, p Policy) (Range, error) {
	stepText := ""
	if idx := strings.LastIndex(s, ":"); idx >= 0 {
		stepText = s[idx+1:]
		s = s[:idx]
	}
	start, end, isRange, err := splitRange(s)
	if err != nil {
		return Range{}, err
	}
	if !isRange {
		if stepText != "" {
			return Range{}, fmt.Errorf("step (%s) requires a range, not a single value", stepText)
		}
		return Range{Start: start, End: start, Step: 1}, nil
	}
	step, err := parseStep(stepText, p)
	if err != nil {
		return Range{}, err
	}
	if p.AscendingOnly && start > end {
		return Range{}, fmt.Errorf("range start (%d) must be ≤ end (%d)", start, end)
	}
	return Range{Start: start, End: end, Step: step}, nil
}

// parseStep reads an element's step suffix. The error strings are load-bearing:
// internal/openjd's existing tests assert them byte for byte, which is how the
// policy layer is verified to have preserved its behavior.
func parseStep(stepText string, p Policy) (int, error) {
	if stepText == "" {
		return 1, nil
	}
	step, err := strconv.Atoi(strings.TrimSpace(stepText))
	switch {
	case err != nil, step == 0:
		if p.PositiveStepOnly {
			return 0, fmt.Errorf("invalid step %q: must be a positive integer", stepText)
		}
		return 0, fmt.Errorf("invalid step %q: must be a non-zero integer", stepText)
	case p.PositiveStepOnly && step < 0:
		return 0, fmt.Errorf("invalid step %q: must be a positive integer", stepText)
	}
	return step, nil
}

// splitRange splits "start-end" into its bounds, or reports a bare integer. The
// separator is the first "-" after position 0, since position 0 can only be the
// start's own sign.
func splitRange(s string) (start, end int, isRange bool, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, false, errors.New("empty value in range expression")
	}
	sepIdx := -1
	for i := 1; i < len(s); i++ {
		if s[i] == '-' {
			sepIdx = i
			break
		}
	}
	if sepIdx < 0 {
		n, parseErr := strconv.Atoi(s)
		if parseErr != nil {
			return 0, 0, false, fmt.Errorf("invalid integer %q", s)
		}
		return n, 0, false, nil
	}
	st, parseErr := strconv.Atoi(strings.TrimSpace(s[:sepIdx]))
	if parseErr != nil {
		return 0, 0, false, fmt.Errorf("invalid range start %q", s[:sepIdx])
	}
	en, parseErr := strconv.Atoi(strings.TrimSpace(s[sepIdx+1:]))
	if parseErr != nil {
		return 0, 0, false, fmt.Errorf("invalid range end %q", s[sepIdx+1:])
	}
	return st, en, true, nil
}
