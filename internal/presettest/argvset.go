// SPDX-License-Identifier: AGPL-3.0-or-later

package presettest

import (
	"fmt"
	"sort"
	"strings"
)

// ArgvSetDiff compares two sets of argument vectors and returns one message per
// difference, sorted, or none when they are equal. Order is ignored, since
// tasks are leased concurrently and run in no fixed order, but both directions
// are checked: a missing argv and an unexpected one are different regressions.
// Arguments are joined on NUL, so "a b" never equals ["a", "b"].
func ArgvSetDiff(want, got [][]string) []string {
	msgs := append(argvSetMinus(got, want, "expected argv never observed: %q"), ArgvUnexpected(want, got)...)
	sort.Strings(msgs)
	return msgs
}

// ArgvUnexpected is the one-sided [ArgvSetDiff]: it reports only the argvs in
// got that are not in want, sorted. It suits a failed job, which need not have
// run every task, so an expected argv that never appeared is not an error there.
func ArgvUnexpected(want, got [][]string) []string {
	return argvSetMinus(want, got, "observed argv not expected: %q")
}

// argvSetMinus returns one message, formatted with format, per distinct argv in
// from that is absent from minus, sorted.
func argvSetMinus(minus, from [][]string, format string) []string {
	key := func(argv []string) string { return strings.Join(argv, "\x00") }
	skip := make(map[string]struct{}, len(minus)+len(from))
	for _, a := range minus {
		skip[key(a)] = struct{}{}
	}
	var msgs []string
	for _, a := range from {
		k := key(a)
		if _, ok := skip[k]; ok {
			continue
		}
		skip[k] = struct{}{} // report each distinct argv once
		msgs = append(msgs, fmt.Sprintf(format, a))
	}
	sort.Strings(msgs)
	return msgs
}
