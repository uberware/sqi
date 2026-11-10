// SPDX-License-Identifier: AGPL-3.0-or-later

package power

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// step is how far each clock advanced between two ticks. runWatch replays a
// list of them, so a test can describe a suspend: the wall clock moving while
// the monotonic clock stood still.
type step struct{ wall, mono time.Duration }

func runWatch(t *testing.T, interval time.Duration, steps []step) int {
	t.Helper()
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var wall, mono time.Duration
	calls := 0 // only the watch goroutine samples: the first call is the baseline
	sample := func() (time.Time, time.Duration) {
		if calls > 0 && calls <= len(steps) {
			wall += steps[calls-1].wall
			mono += steps[calls-1].mono
		}
		calls++
		return base.Add(wall), mono
	}

	var fired atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		watch(ctx, ticks, interval, sample, func() { fired.Add(1) })
	}()
	for range steps {
		ticks <- time.Time{}
	}
	cancel()
	<-done
	return int(fired.Load())
}

func TestWatchResume_DetectsJump(t *testing.T) {
	const interval = 2 * time.Second
	// A laptop lid closed for an hour: two seconds of monotonic time, an hour
	// of wall time.
	got := runWatch(t, interval, []step{
		{wall: interval, mono: interval},
		{wall: time.Hour, mono: interval},
		{wall: interval, mono: interval},
	})
	if got != 1 {
		t.Fatalf("onResume fired %d times, want 1", got)
	}
}

func TestWatchResume_IgnoresSteadyClock(t *testing.T) {
	const interval = 2 * time.Second
	steps := make([]step, 50)
	for i := range steps {
		// A scheduling stall on both clocks, and ordinary jitter, are not a resume.
		d := interval
		if i == 10 {
			d = 30 * time.Second
		}
		steps[i] = step{wall: d, mono: d}
	}
	if got := runWatch(t, interval, steps); got != 0 {
		t.Fatalf("onResume fired %d times on a steady clock, want 0", got)
	}
}

func TestWatchResume_ThresholdIsThreeIntervalsPlusFive(t *testing.T) {
	const interval = 2 * time.Second
	threshold := 3*interval + 5*time.Second
	tests := []struct {
		name  string
		extra time.Duration
		want  int
	}{
		{"at the threshold", threshold, 0},
		{"just past it", threshold + time.Millisecond, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runWatch(t, interval, []step{{wall: interval + tt.extra, mono: interval}})
			if got != tt.want {
				t.Fatalf("fired %d times, want %d", got, tt.want)
			}
		})
	}
}

// The public entry point must work on real clocks: a steady clock never fires
// and the loop ends with its context.
func TestWatchResume_StopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var fired atomic.Int32
	done := make(chan struct{})
	go func() {
		WatchResume(ctx, 5*time.Millisecond, time.Now, func() { fired.Add(1) })
		close(done)
	}()
	time.Sleep(40 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("WatchResume did not return after its context was canceled")
	}
	if fired.Load() != 0 {
		t.Errorf("fired %d times on a real steady clock", fired.Load())
	}
}
