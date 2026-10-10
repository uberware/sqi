// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"testing"
	"time"
)

// TestRowClock_TextStrictlyIncreases pins what insertTasksTx relies on: every
// stamp is later than the one before, and its text sorts after it too, since
// SQLite orders created_at as TEXT. Back-to-back calls read the same wall-clock
// instant on Windows, and a stamp ending in zero is one RFC3339Nano would trim.
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

func TestRowClock_KeepsTrailingZeros(t *testing.T) {
	// The wall clock is behind last, so next is last plus a nanosecond: a whole
	// second, which RFC3339Nano would write with no fraction at all.
	c := rowClock{last: time.Now().UTC().Add(time.Hour).Truncate(time.Second).Add(-time.Nanosecond)}
	if got := c.next(); len(got) != len("2006-01-02T15:04:05.000000000Z") {
		t.Fatalf("stamp %q is not fixed width", got)
	}
}
