// SPDX-License-Identifier: AGPL-3.0-or-later

package simworker_test

import (
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/simworker"
)

func TestLedger_EventsIsACopy(t *testing.T) {
	l := simworker.NewLedger()
	l.Record(simworker.Event{TaskID: "t1", Kind: simworker.KindLeased})
	evs := l.Events()
	evs[0].TaskID = "changed"
	if got := l.Events()[0].TaskID; got != "t1" {
		t.Errorf("Events()[0].TaskID = %q after mutating the copy, want t1", got)
	}
	if l.Events()[0].At.IsZero() {
		t.Error("Record left At zero, want it stamped")
	}
}

func TestLedger_ConcurrentRecord(t *testing.T) {
	l := simworker.NewLedger()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				l.Record(simworker.Event{Kind: simworker.KindRunning})
			}
		})
	}
	wg.Wait()
	if n := len(l.Events()); n != 800 {
		t.Errorf("len(Events()) = %d, want 800", n)
	}
}

func TestLedger_SetOutcome(t *testing.T) {
	l := simworker.NewLedger()
	base := time.Unix(1000, 0)
	l.Record(simworker.Event{At: base, TaskID: "t1", AttemptID: "a1", Kind: simworker.KindTerminal, Outcome: "failed"})
	l.Record(simworker.Event{At: base.Add(time.Second), TaskID: "t1", AttemptID: "a2", Kind: simworker.KindTerminal, Outcome: "succeeded"})
	l.Record(simworker.Event{At: base.Add(2 * time.Second), TaskID: "t1", AttemptID: "a3", Kind: simworker.KindLeased})
	l.Record(simworker.Event{At: base, TaskID: "t2", Kind: simworker.KindRunning})

	tests := []struct {
		name    string
		taskID  string
		want    bool
		outcome map[string]string // attempt -> expected outcome after the call
	}{
		{"rewrites the last terminal", "t1", true, map[string]string{"a1": "failed", "a2": "rewritten", "a3": ""}},
		{"no terminal event", "t2", false, nil},
		{"unknown task", "nope", false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := l.SetOutcome(tt.taskID, "rewritten"); got != tt.want {
				t.Fatalf("SetOutcome(%q) = %v, want %v", tt.taskID, got, tt.want)
			}
			got := map[string]string{}
			for _, e := range l.Events() {
				if e.TaskID == tt.taskID && e.AttemptID != "" {
					got[e.AttemptID] = e.Outcome
				}
			}
			if tt.outcome != nil && !maps.Equal(got, tt.outcome) {
				t.Errorf("outcomes = %v, want %v", got, tt.outcome)
			}
		})
	}
}

func TestLedger_ConcurrentPeak(t *testing.T) {
	at := func(s int) time.Time { return time.Unix(int64(1000+s), 0) }
	ev := func(s int, attempt, job, kind string) simworker.Event {
		return simworker.Event{At: at(s), AttemptID: attempt, TaskID: "task-" + attempt, JobID: job, Kind: kind}
	}
	tests := []struct {
		name   string
		events []simworker.Event
		want   map[string]int
	}{
		{
			name: "overlapping attempts in one key",
			events: []simworker.Event{
				ev(0, "a1", "j1", simworker.KindLeased),
				ev(1, "a2", "j1", simworker.KindLeased),
				ev(2, "a3", "j2", simworker.KindLeased),
				ev(3, "a1", "j1", simworker.KindTerminal),
				ev(4, "a4", "j1", simworker.KindLeased),
				ev(5, "a2", "j1", simworker.KindCanceled),
				ev(6, "a4", "j1", simworker.KindCrashed),
			},
			want: map[string]int{"j1": 2, "j2": 1},
		},
		{
			name: "an end and a start at the same instant do not overlap",
			events: []simworker.Event{
				ev(0, "a1", "j1", simworker.KindLeased),
				ev(1, "a2", "j1", simworker.KindLeased),
				ev(1, "a1", "j1", simworker.KindTerminal),
			},
			want: map[string]int{"j1": 1},
		},
		{
			name: "an attempt with no end stays counted",
			events: []simworker.Event{
				ev(0, "a1", "j1", simworker.KindLeased),
				ev(1, "a1", "j1", simworker.KindRunning),
				ev(2, "a2", "j1", simworker.KindLeased),
			},
			want: map[string]int{"j1": 2},
		},
		{name: "empty", events: nil, want: map[string]int{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := simworker.NewLedger()
			for _, e := range tt.events {
				l.Record(e)
			}
			got := l.ConcurrentPeak(func(e simworker.Event) string { return e.JobID })
			if !maps.Equal(got, tt.want) {
				t.Errorf("ConcurrentPeak = %v, want %v", got, tt.want)
			}
		})
	}
}
