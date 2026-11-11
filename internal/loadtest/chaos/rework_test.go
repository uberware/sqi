// SPDX-License-Identifier: AGPL-3.0-or-later

package chaos

import (
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/marker"
	"github.com/uberware/sqi/internal/loadtest/report"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/simworker"
	"github.com/uberware/sqi/internal/store"
)

func TestRework_SimulatedUncreditedSuccessCounts(t *testing.T) {
	ts := newSQLiteStore(t)
	a := seedRun(t, ts.st, readyJob("j1", "s1", "t1"), "t1", store.TaskStatusFailed)
	t0 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	l := simworker.NewLedger()
	l.Record(simworker.Event{At: t0.Add(11 * time.Second), WorkerID: "sim-1", TaskID: "t1", AttemptID: a.ID, Kind: simworker.KindLeased})
	l.Record(simworker.Event{At: t0.Add(14 * time.Second), WorkerID: "sim-1", TaskID: "t1", AttemptID: a.ID, Kind: simworker.KindTerminal, Outcome: "succeeded"})
	faults := []report.FaultRecord{
		{Index: 0, Kind: "kill", Start: t0},
		{Index: 1, Kind: "kill", Start: t0.Add(10 * time.Second)},
		{Index: 2, Kind: "kill", Start: t0.Add(30 * time.Second)},
	}
	got, err := ComputeRework(t.Context(), ts.st, l, nil, checkFarm, faults)
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempts != 1 || got.Seconds != 3 {
		t.Errorf("Rework = %+v, want 1 attempt of 3s", got)
	}
	if want := map[int]int{1: 1}; len(got.ByFault) != 1 || got.ByFault[1] != 1 {
		t.Errorf("ByFault = %v, want %v", got.ByFault, want)
	}
	if len(got.Examples) != 1 || !strings.Contains(got.Examples[0], a.ID) {
		t.Errorf("Examples = %q, want one naming %s", got.Examples, a.ID)
	}
}

func TestRework_SimulatedCreditedSuccessIsNotRework(t *testing.T) {
	ts := newSQLiteStore(t)
	a := seedRun(t, ts.st, readyJob("j1", "s1", "t1"), "t1", store.TaskStatusSucceeded)
	got, err := ComputeRework(t.Context(), ts.st, ledgerOf(ledgerEntry{a.ID, "t1", "succeeded"}), nil, checkFarm,
		[]report.FaultRecord{{Index: 0, Start: time.Now().Add(-time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempts != 0 || got.Seconds != 0 {
		t.Errorf("Rework = %+v, want zero", got)
	}
}

func TestRework_RealExtraCompletionCounts(t *testing.T) {
	ts := newSQLiteStore(t)
	a := seedRun(t, ts.st, realJob(), "t1", store.TaskStatusSucceeded)
	got, err := ts.st.GetTaskAttempt(t.Context(), a.ID)
	if err != nil || got.EndedAt == nil {
		t.Fatalf("GetTaskAttempt = %+v, %v", got, err)
	}
	credited := *got.EndedAt
	key := scenario.TaskKey("job-x", "step-1", "3", "")
	wasted := credited.Add(-time.Minute)
	exec := func(id string, start, end time.Time, code int) marker.Exec {
		e := mainExec(id, key)
		e.Start, e.End, e.ExitCode = start, &end, code
		return e
	}
	execs := []marker.Exec{
		exec("e-credited", credited.Add(-5*time.Second), credited.Add(-time.Millisecond), 0),
		exec("e-wasted", wasted.Add(-8*time.Second), wasted, 0),
		exec("e-failed", wasted.Add(-2*time.Second), wasted, 1), // not completed: not a candidate
		{ID: "c1", Key: key, Role: marker.RoleChatty},
	}
	faults := []report.FaultRecord{
		{Index: 0, Start: wasted.Add(-time.Hour)},
		{Index: 1, Start: wasted.Add(-time.Second)},
		{Index: 2, Start: credited},
	}
	rw, err := ComputeRework(t.Context(), ts.st, nil, execs, checkFarm, faults)
	if err != nil {
		t.Fatal(err)
	}
	if rw.Attempts != 1 || rw.Seconds != 8 {
		t.Errorf("Rework = %+v, want 1 attempt of 8s", rw)
	}
	if rw.ByFault[1] != 1 || len(rw.ByFault) != 1 {
		t.Errorf("ByFault = %v, want {1: 1}", rw.ByFault)
	}
	if len(rw.Examples) != 1 || !strings.Contains(rw.Examples[0], "e-wasted") {
		t.Errorf("Examples = %q, want one naming e-wasted", rw.Examples)
	}
}

func TestRework_RealNoSucceededAttemptCountsEveryCompletion(t *testing.T) {
	ts := newSQLiteStore(t)
	seedRun(t, ts.st, realJob(), "t1", store.TaskStatusFailed)
	key := scenario.TaskKey("job-x", "step-1", "3", "")
	end := time.Now().UTC()
	e := mainExec("e1", key)
	e.Start, e.End = end.Add(-2*time.Second), &end
	rw, err := ComputeRework(t.Context(), ts.st, nil, []marker.Exec{e}, checkFarm, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rw.Attempts != 1 || rw.Seconds != 2 || len(rw.ByFault) != 0 {
		t.Errorf("Rework = %+v, want 1 attempt of 2s with no fault", rw)
	}
}

func TestRework_NoFaultsIsZero(t *testing.T) {
	ts := newSQLiteStore(t)
	a := seedRun(t, ts.st, realJob(), "t1", store.TaskStatusSucceeded)
	got, err := ts.st.GetTaskAttempt(t.Context(), a.ID)
	if err != nil || got.EndedAt == nil {
		t.Fatalf("GetTaskAttempt = %+v, %v", got, err)
	}
	key := scenario.TaskKey("job-x", "step-1", "3", "")
	e := mainExec("e1", key)
	e.Start, e.End = got.EndedAt.Add(-time.Second), got.EndedAt
	rw, err := ComputeRework(t.Context(), ts.st, ledgerOf(ledgerEntry{a.ID, "t1", "succeeded"}), []marker.Exec{e}, checkFarm, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rw.Attempts != 0 || rw.Seconds != 0 || len(rw.ByFault) != 0 || len(rw.Examples) != 0 {
		t.Errorf("Rework = %+v, want zero", rw)
	}
}

func TestRework_ExamplesAreCapped(t *testing.T) {
	ts := newSQLiteStore(t)
	l := simworker.NewLedger()
	for i := range maxReworkExamples + 5 {
		id := "t" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		a := seedRun(t, ts.st, readyJob("j"+id, "s"+id, id), id, store.TaskStatusFailed)
		l.Record(simworker.Event{WorkerID: "sim-1", TaskID: id, AttemptID: a.ID, Kind: simworker.KindLeased})
		l.Record(simworker.Event{WorkerID: "sim-1", TaskID: id, AttemptID: a.ID, Kind: simworker.KindTerminal, Outcome: "succeeded"})
	}
	rw, err := ComputeRework(t.Context(), ts.st, l, nil, checkFarm, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rw.Attempts != maxReworkExamples+5 || len(rw.Examples) != maxReworkExamples {
		t.Errorf("Attempts = %d, Examples = %d, want %d and %d", rw.Attempts, len(rw.Examples), maxReworkExamples+5, maxReworkExamples)
	}
}
