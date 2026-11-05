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

func TestLedger_LogLines(t *testing.T) {
	l := simworker.NewLedger()
	l.Record(simworker.Event{TaskID: "t1", AttemptID: "a1", Kind: simworker.KindLeased})
	l.Record(simworker.Event{TaskID: "t1", AttemptID: "a1", Kind: simworker.KindTerminal, Outcome: simworker.OutcomeSucceeded, LogLines: 120})
	l.Record(simworker.Event{TaskID: "t2", AttemptID: "a2", Kind: simworker.KindCrashed, LogLines: 7})
	if got := l.LogLines(); got != 127 {
		t.Errorf("LogLines = %d, want 127", got)
	}
}
