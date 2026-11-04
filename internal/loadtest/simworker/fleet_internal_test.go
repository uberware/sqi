// SPDX-License-Identifier: AGPL-3.0-or-later

package simworker

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/scenario"
)

func churnGroup(crash, disconnect float64) scenario.WorkerGroup {
	return scenario.WorkerGroup{
		Name: "g",
		Churn: scenario.Churn{
			CrashPerHour: crash, DisconnectPerHour: disconnect,
			RestartAfter: scenario.Duration{Duration: 10 * time.Second},
		},
	}
}

func TestFleet_ChurnDeterministic(t *testing.T) {
	g := churnGroup(6, 4)
	const n = 20
	a := churnSchedule(42, g, n, time.Hour)
	b := churnSchedule(42, g, n, time.Hour)
	if len(a) == 0 {
		t.Fatal("schedule is empty at 10 events per worker-hour")
	}
	if !slices.Equal(a, b) {
		t.Fatal("same seed produced different schedules")
	}
	if c := churnSchedule(43, g, n, time.Hour); slices.Equal(a, c) {
		t.Error("different seeds produced the same schedule")
	}
}

func TestChurnSchedule_Shape(t *testing.T) {
	const n = 50
	horizon := time.Hour

	tests := []struct {
		name            string
		crash, discon   float64
		wantCrash       bool
		wantDisconnects bool
	}{
		{"crashes only", 12, 0, true, false},
		{"disconnects only", 0, 12, false, true},
		{"both", 12, 12, true, true},
		{"neither", 0, 0, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := churnSchedule(7, churnGroup(tt.crash, tt.discon), n, horizon)
			var crashes, discons int
			for _, e := range ev {
				if e.At < 0 || e.At >= horizon {
					t.Errorf("event at %s is outside [0, %s)", e.At, horizon)
				}
				if e.Worker < 0 || e.Worker >= n {
					t.Errorf("event for worker %d, want [0, %d)", e.Worker, n)
				}
				switch e.Kind {
				case churnCrash:
					crashes++
				case churnDisconnect:
					discons++
				}
			}
			if (crashes > 0) != tt.wantCrash || (discons > 0) != tt.wantDisconnects {
				t.Errorf("crashes=%d disconnects=%d, want crash=%v disconnect=%v", crashes, discons, tt.wantCrash, tt.wantDisconnects)
			}
			// 50 workers x 12/h = 600 expected; the sd is about 25.
			for _, c := range []struct {
				got  int
				rate float64
			}{{crashes, tt.crash}, {discons, tt.discon}} {
				want := c.rate * n * horizon.Hours()
				if math.Abs(float64(c.got)-want) > 5*math.Sqrt(want+1) {
					t.Errorf("got %d events, want about %.0f", c.got, want)
				}
			}
			if !slices.IsSortedFunc(ev, compareChurn) {
				t.Error("schedule is not sorted by time")
			}
		})
	}
}

// A worker's events do not depend on how many workers the group has, so growing
// a group does not reshuffle the ones already in it.
func TestChurnSchedule_PerWorkerStreams(t *testing.T) {
	g := churnGroup(30, 30)
	only := func(ev []churnEvent, w int) []churnEvent {
		return slices.DeleteFunc(slices.Clone(ev), func(e churnEvent) bool { return e.Worker != w })
	}
	small := churnSchedule(9, g, 3, time.Hour)
	big := churnSchedule(9, g, 8, time.Hour)
	for w := range 3 {
		if !slices.Equal(only(small, w), only(big, w)) {
			t.Errorf("worker %d's events changed when the group grew", w)
		}
	}
}

func TestWorkerSeed(t *testing.T) {
	seen := map[uint64]int{}
	for i := range 1000 {
		s := workerSeed(5, i)
		if j, dup := seen[s]; dup {
			t.Fatalf("workers %d and %d share seed %d", j, i, s)
		}
		seen[s] = i
		if workerSeed(5, i) != s {
			t.Fatalf("worker %d's seed is not stable", i)
		}
	}
	if workerSeed(5, 0) == workerSeed(6, 0) {
		t.Error("run seeds 5 and 6 give worker 0 the same seed")
	}
}
