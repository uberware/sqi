// SPDX-License-Identifier: AGPL-3.0-or-later

package presettest

import (
	"fmt"
	"strings"
)

// ArgvSetDiff compares two sets of argument vectors and returns one message per
// difference, or none when they are equal. Order is ignored -- tasks are leased
// concurrently, so invocation order is genuinely nondeterministic -- but both
// directions are checked: a missing argv and an unexpected one are different
// regressions. Arguments are joined on NUL, so "a b" never equals ["a", "b"].
func ArgvSetDiff(want, got [][]string) []string {
	key := func(argv []string) string { return strings.Join(argv, "\x00") }
	wantSet := make(map[string][]string, len(want))
	for _, a := range want {
		wantSet[key(a)] = a
	}
	gotSet := make(map[string][]string, len(got))
	for _, a := range got {
		gotSet[key(a)] = a
	}
	var msgs []string
	for k, a := range wantSet {
		if _, ok := gotSet[k]; !ok {
			msgs = append(msgs, fmt.Sprintf("expected inner argv never observed: %v", a))
		}
	}
	for k, a := range gotSet {
		if _, ok := wantSet[k]; !ok {
			msgs = append(msgs, fmt.Sprintf("observed inner argv not expected: %v", a))
		}
	}
	return msgs
}
