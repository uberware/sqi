// SPDX-License-Identifier: AGPL-3.0-or-later

package power

import (
	"context"
	"time"
)

// WatchResume calls onResume each time the machine appears to have been
// suspended and woken, until ctx is done. It samples every interval (2 s in
// production) and compares how far the wall clock advanced with how far the
// monotonic clock did, both read from now.
//
// The comparison works because the monotonic clock stops while the machine is
// suspended on Linux, macOS and Windows, while the wall clock is corrected on
// wake. An advance of the wall clock that outruns the monotonic one by more
// than 3 x interval + 5 s is a suspend; anything shorter is a scheduling stall
// or ordinary clock slew. A manual step of the wall clock forward by that much
// triggers it too, which only costs a harmless re-registration.
//
// now must return times that carry a monotonic reading, as [time.Now] does: a
// time without one compares the wall clock with itself and never fires.
func WatchResume(ctx context.Context, interval time.Duration, now func() time.Time, onResume func()) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	start := now()
	sample := func() (time.Time, time.Duration) {
		n := now()
		return n.Round(0), n.Sub(start) // Round(0) drops the monotonic reading; Sub uses it
	}
	watch(ctx, ticker.C, interval, sample, onResume)
}

// resumeSlack is added to three sampling intervals to make the wall-clock lead
// that counts as a resume.
const resumeSlack = 5 * time.Second

// watch is [WatchResume] with its clocks and tick source supplied: sample
// returns the wall-clock time and the monotonic time elapsed since a fixed
// point.
func watch(
	ctx context.Context, ticks <-chan time.Time, interval time.Duration,
	sample func() (time.Time, time.Duration), onResume func(),
) {
	lastWall, lastMono := sample()
	threshold := 3*interval + resumeSlack
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			wall, mono := sample()
			if wall.Sub(lastWall)-(mono-lastMono) > threshold {
				onResume()
			}
			lastWall, lastMono = wall, mono
		}
	}
}
