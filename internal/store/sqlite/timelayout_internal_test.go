// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"testing"
	"time"
)

// storedWidth is the length of every timestamp the store writes. A fixed
// width is what makes SQLite's TEXT comparison agree with time order.
const storedWidth = len("2006-01-02T15:04:05.000000000Z")

func TestTimeToText_IsFixedWidthUTC(t *testing.T) {
	base := time.Date(2026, 10, 10, 12, 0, 5, 0, time.UTC)
	cases := []struct {
		name string
		in   time.Time
		want string
	}{
		{"whole second", base, "2026-10-10T12:00:05.000000000Z"},
		{"one fractional digit", base.Add(500 * time.Millisecond), "2026-10-10T12:00:05.500000000Z"},
		{"nine digits", base.Add(123456789 * time.Nanosecond), "2026-10-10T12:00:05.123456789Z"},
		{"non-UTC input", base.In(time.FixedZone("CET", 3600)), "2026-10-10T12:00:05.000000000Z"},
		{"zero time", time.Time{}, "0001-01-01T00:00:00.000000000Z"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := timeToText(c.in)
			if got != c.want {
				t.Fatalf("timeToText(%v) = %q, want %q", c.in, got, c.want)
			}
			if len(got) != storedWidth {
				t.Fatalf("timeToText(%v) = %q, %d characters, want %d", c.in, got, len(got), storedWidth)
			}
		})
	}
}

// TestTextToTime_ReadsEveryStoredForm pins the lenient read: every form an
// earlier release stored, and the form this one stores, read back as the same
// instant. parseLayout is time.RFC3339Nano, the layout every earlier release
// parses with, so the nine-digit case is also the downgrade guarantee.
func TestTextToTime_ReadsEveryStoredForm(t *testing.T) {
	half := time.Date(2026, 10, 10, 12, 0, 5, 500_000_000, time.UTC)
	whole := time.Date(2026, 10, 10, 12, 0, 5, 0, time.UTC)
	cases := []struct {
		stored string
		want   time.Time
	}{
		{"2026-10-10T12:00:05Z", whole},
		{"2026-10-10T12:00:05.000000000Z", whole},
		{"2026-10-10T12:00:05.5Z", half},
		{"2026-10-10T12:00:05.500Z", half},
		{"2026-10-10T12:00:05.500000000Z", half},
	}
	for _, c := range cases {
		got, err := textToTime(c.stored)
		if err != nil {
			t.Fatalf("textToTime(%q): %v", c.stored, err)
		}
		if !got.Equal(c.want) {
			t.Fatalf("textToTime(%q) = %v, want %v", c.stored, got, c.want)
		}
	}
}

func TestTimeToText_RoundTripsNanoseconds(t *testing.T) {
	for _, in := range []time.Time{
		time.Date(2026, 10, 10, 12, 0, 5, 1, time.UTC),
		time.Date(2026, 10, 10, 12, 0, 5, 999_999_999, time.UTC),
		time.Now().UTC(),
	} {
		got, err := textToTime(timeToText(in))
		if err != nil {
			t.Fatalf("textToTime(timeToText(%v)): %v", in, err)
		}
		if !got.Equal(in) {
			t.Fatalf("round trip of %v = %v", in, got)
		}
	}
}
