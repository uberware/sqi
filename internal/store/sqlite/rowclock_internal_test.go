// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import "testing"

// TestRowClock_TextStrictlyIncreases pins what insertTasksTx relies on: every
// stamp is later than the one before, and its text sorts after it too, since
// SQLite orders created_at as TEXT. Back-to-back calls read the same wall-clock
// instant on Windows, so many stamps are the previous one plus a nanosecond.
func TestRowClock_TextStrictlyIncreases(t *testing.T) {
	var c rowClock
	prev := c.next()
	for range 10_000 {
		got := c.next()
		if got <= prev {
			t.Fatalf("stamp %q does not sort after %q", got, prev)
		}
		pt, err := textToTime(prev)
		if err != nil {
			t.Fatalf("textToTime(%q): %v", prev, err)
		}
		gt, err := textToTime(got)
		if err != nil {
			t.Fatalf("textToTime(%q): %v", got, err)
		}
		if !gt.After(pt) {
			t.Fatalf("stamp %v is not after %v", gt, pt)
		}
		prev = got
	}
}
