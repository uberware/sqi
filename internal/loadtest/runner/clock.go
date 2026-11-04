// SPDX-License-Identifier: AGPL-3.0-or-later

package runner

import (
	"fmt"
	"time"
)

const (
	// coarseClock is the wall-clock step above which sub-millisecond push
	// lags cannot be measured. Go's wall clock steps about every 0.5 ms on
	// Windows and every 100 ns or less on Linux and macOS.
	coarseClock = 50 * time.Microsecond
	// clockProbe bounds how long wallClockStep watches the clock.
	clockProbe = 50 * time.Millisecond
	// clockSteps is how many wall-clock steps wallClockStep watches.
	clockSteps = 8
)

// wallClockStep is the smallest step of this host's wall clock seen in a
// few steps (at most clockProbe), the resolution of every timestamp that
// crosses a process boundary. It is 0 when the clock did not step at all.
func wallClockStep() time.Duration {
	deadline := time.Now().Add(clockProbe)
	prev := time.Now().Round(0) // the wall reading alone
	var smallest time.Duration
	for steps := 0; steps < clockSteps && time.Now().Before(deadline); {
		now := time.Now().Round(0)
		if d := now.Sub(prev); d > 0 {
			if smallest == 0 || d < smallest {
				smallest = d
			}
			prev = now
			steps++
		}
	}
	return smallest
}

// wsLagCaveat explains, for a host whose wall clock steps coarsely, why the
// WebSocket lag histogram reads 0 for pushes that arrive within one step:
// the server stamps a push with its wall clock and the client compares that
// stamp with its own. It is "" for a fine clock.
func wsLagCaveat(step time.Duration) string {
	if step <= coarseClock {
		return ""
	}
	return fmt.Sprintf("lags below about %s cannot be resolved on this host: its wall clock advances in steps of %s, so a push delivered within one step reads 0 (shown as the histogram's 1µs floor)",
		step.Round(10*time.Microsecond), step.Round(time.Microsecond))
}
