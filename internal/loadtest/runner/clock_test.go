// SPDX-License-Identifier: AGPL-3.0-or-later

package runner

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestWallClockStep(t *testing.T) {
	step := wallClockStep()
	if step <= 0 || step > 20*time.Millisecond {
		t.Fatalf("wallClockStep = %s, want a step between 0 and 20ms", step)
	}
	t.Logf("wall clock step on %s: %s", runtime.GOOS, step)
}

func TestWSLagCaveat(t *testing.T) {
	if got := wsLagCaveat(100 * time.Nanosecond); got != "" {
		t.Errorf("a fine clock has a caveat: %q", got)
	}
	if got := wsLagCaveat(570 * time.Microsecond); !strings.Contains(got, "570µs") {
		t.Errorf("a coarse clock's caveat %q does not name its step", got)
	}
}
