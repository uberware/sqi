// SPDX-License-Identifier: AGPL-3.0-or-later

package presettest_test

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/presettest"
)

func TestArgvSetDiff(t *testing.T) {
	want := [][]string{{"1", "1-1"}, {"2", "2-2"}}
	if msgs := presettest.ArgvSetDiff(want, [][]string{{"2", "2-2"}, {"1", "1-1"}}); len(msgs) != 0 {
		t.Fatalf("equal sets in another order: %v", msgs)
	}
	msgs := presettest.ArgvSetDiff(want, [][]string{{"1", "1-1"}, {"3", "3-3"}})
	joined := strings.Join(msgs, "\n")
	if len(msgs) != 2 || !strings.Contains(joined, `["2" "2-2"]`) || !strings.Contains(joined, `["3" "3-3"]`) {
		t.Fatalf("msgs = %v, want one missing [2 2-2] and one unexpected [3 3-3]", msgs)
	}
	// An argument containing a space must not collide with two arguments.
	if msgs := presettest.ArgvSetDiff([][]string{{"a b"}}, [][]string{{"a", "b"}}); len(msgs) != 2 {
		t.Fatalf("argument-boundary collision: %v", msgs)
	}
}
