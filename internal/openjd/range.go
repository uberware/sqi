// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// intRange represents a contiguous, stepped sequence of integers [Start, End]
// with the given Step.  Step is always ≥ 1.
type intRange struct {
	Start int
	End   int
	Step  int
}

// iterate yields every integer in the range.
func (r intRange) iterate() []int {
	if r.Step <= 0 {
		r.Step = 1
	}
	var out []int
	for v := r.Start; v <= r.End; v += r.Step {
		out = append(out, v)
	}
	return out
}

// parseIntRangeExpr parses an OpenJD integer range expression string.
//
// Grammar:
//
//	<RangeExpr>  ::= <Element> (',' <Element>)*
//	<Element>    ::= <Number> | <Number>'-'<Number> | <Number>'-'<Number>':'<Step>
//	<Number>     ::= optional sign + decimal digits
//	<Step>       ::= non-zero positive decimal integer
//
// Example inputs: "1", "1-100", "1-100:2", "1,5,10", "1-10,20-30", "0-100:5,200".
func parseIntRangeExpr(expr string) ([]int, error) {
	if strings.TrimSpace(expr) == "" {
		return nil, errors.New("openjd: range expression is empty")
	}

	seen := make(map[int]struct{})
	var result []int

	for part := range strings.SplitSeq(expr, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		vals, err := parseRangeElement(part)
		if err != nil {
			return nil, fmt.Errorf("openjd: range expression %q: %w", expr, err)
		}
		for _, v := range vals {
			if _, dup := seen[v]; !dup {
				seen[v] = struct{}{}
				result = append(result, v)
			}
		}
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("openjd: range expression %q produced no values", expr)
	}
	return result, nil
}

// parseRangeElement handles one comma-separated element of a range expression.
func parseRangeElement(s string) ([]int, error) {
	// Check for a step suffix: "start-end:step"
	var stepPart string
	if idx := strings.LastIndex(s, ":"); idx >= 0 {
		stepPart = s[idx+1:]
		s = s[:idx]
	}

	// Check for a hyphen indicating a range (handle negative numbers carefully).
	// Strategy: find the last '-' that is not at position 0 (sign of start).
	start, end, isRange, err := splitRange(s)
	if err != nil {
		return nil, err
	}

	if !isRange {
		// Single value.
		if stepPart != "" {
			return nil, fmt.Errorf("step (%s) requires a range, not a single value", stepPart)
		}
		return []int{start}, nil
	}

	step := 1
	if stepPart != "" {
		step, err = strconv.Atoi(strings.TrimSpace(stepPart))
		if err != nil || step <= 0 {
			return nil, fmt.Errorf("invalid step %q: must be a positive integer", stepPart)
		}
	}

	if start > end {
		return nil, fmt.Errorf("range start (%d) must be ≤ end (%d)", start, end)
	}

	r := intRange{Start: start, End: end, Step: step}
	return r.iterate(), nil
}

// splitRange splits "start-end" (possibly with negative numbers) into (start, end, true, nil).
// Returns (start, 0, false, nil) for a bare number.
func splitRange(s string) (start, end int, isRange bool, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, false, errors.New("empty value in range expression")
	}

	// Find the hyphen that acts as the range separator.
	// We skip position 0 (could be a leading minus) and look for the next '-'.
	sepIdx := -1
	for i := 1; i < len(s); i++ {
		if s[i] == '-' {
			sepIdx = i
			break
		}
	}

	if sepIdx < 0 {
		// No separator: single number.
		n, parseErr := strconv.Atoi(s)
		if parseErr != nil {
			return 0, 0, false, fmt.Errorf("invalid integer %q", s)
		}
		return n, 0, false, nil
	}

	startStr := s[:sepIdx]
	endStr := s[sepIdx+1:]

	st, parseErr := strconv.Atoi(strings.TrimSpace(startStr))
	if parseErr != nil {
		return 0, 0, false, fmt.Errorf("invalid range start %q", startStr)
	}
	en, parseErr := strconv.Atoi(strings.TrimSpace(endStr))
	if parseErr != nil {
		return 0, 0, false, fmt.Errorf("invalid range end %q", endStr)
	}

	return st, en, true, nil
}
