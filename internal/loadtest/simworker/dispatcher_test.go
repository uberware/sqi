// SPDX-License-Identifier: AGPL-3.0-or-later

package simworker_test

import (
	"encoding/json"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/simworker"
	"github.com/uberware/sqi/internal/worker/logstreamer"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/status"
)

// published is one message the fake connection saw.
type published struct {
	subject string
	data    []byte
}

// fakeConn records every publish from the status and log publishers, in order.
type fakeConn struct {
	mu        sync.Mutex
	msgs      []published
	onPublish func(subj string, data []byte)
}

func (f *fakeConn) Publish(subj string, data []byte) error {
	f.mu.Lock()
	f.msgs = append(f.msgs, published{subject: subj, data: slices.Clone(data)})
	hook := f.onPublish
	f.mu.Unlock()
	if hook != nil {
		hook(subj, data)
	}
	return nil
}

func (f *fakeConn) setHook(h func(string, []byte)) {
	f.mu.Lock()
	f.onPublish = h
	f.mu.Unlock()
}

func (f *fakeConn) snapshot() []published {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.msgs)
}

// statuses decodes the task-status messages, in publish order.
func (f *fakeConn) statuses(t *testing.T) []protocol.TaskStatusMsg {
	t.Helper()
	var out []protocol.TaskStatusMsg
	for _, m := range f.snapshot() {
		if !strings.HasPrefix(m.subject, "task.status.") {
			continue
		}
		var s protocol.TaskStatusMsg
		if err := json.Unmarshal(m.data, &s); err != nil {
			t.Fatalf("decode status: %v", err)
		}
		out = append(out, s)
	}
	return out
}

const testWorkerID = "w1"

func newTestDispatcher(t *testing.T, plan simworker.TaskPlan) (*simworker.Dispatcher, *fakeConn, *simworker.Ledger) {
	t.Helper()
	conn := &fakeConn{}
	logger := slog.New(slog.DiscardHandler)
	st := status.New(conn, status.Config{WorkerID: testWorkerID}, logger)
	logs := logstreamer.New(conn, testWorkerID, logstreamer.DefaultConfig(), logger)
	l := simworker.NewLedger()
	b := simworker.BehaviorFunc(func(*protocol.AssignMsg, *rand.Rand) simworker.TaskPlan { return plan })
	d := simworker.NewDispatcher(testWorkerID, st, logs, nil, b, l, rand.New(rand.NewPCG(1, 2)))
	return d, conn, l
}

func assignment(task, attempt string) *protocol.AssignMsg {
	return &protocol.AssignMsg{
		Version: protocol.ProtocolVersion, Type: protocol.TypeAssign,
		TaskID: task, JobID: "j1", StepID: "s1", AttemptID: attempt, WorkerID: testWorkerID,
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ledgerHas reports whether l holds an event of kind for task.
func ledgerHas(l *simworker.Ledger, task, kind string) bool {
	return countKind(l, task, kind) > 0
}

func countKind(l *simworker.Ledger, task, kind string) int {
	n := 0
	for _, e := range l.Events() {
		if (task == "" || e.TaskID == task) && e.Kind == kind {
			n++
		}
	}
	return n
}

func TestDispatcher_SucceededPath(t *testing.T) {
	const lines, lineBytes = 7, 40
	d, conn, l := newTestDispatcher(t, simworker.TaskPlan{
		Duration: 50 * time.Millisecond, Outcome: simworker.OutcomeSucceeded, LogLines: lines, LineBytes: lineBytes,
	})

	// Is the attempt still reported as held at the moment its terminal status
	// goes out?
	var heldAtTerminal, sawTerminal bool
	var hookMu sync.Mutex
	conn.setHook(func(subj string, data []byte) {
		if !strings.HasPrefix(subj, "task.status.") {
			return
		}
		var s protocol.TaskStatusMsg
		if json.Unmarshal(data, &s) != nil || s.Status != "succeeded" {
			return
		}
		hookMu.Lock()
		sawTerminal = true
		heldAtTerminal = slices.Contains(d.ActiveAttemptIDs(), "a1")
		hookMu.Unlock()
	})

	d.Hold("a1")
	if !slices.Contains(d.ActiveAttemptIDs(), "a1") {
		t.Fatal("Hold did not report the attempt")
	}
	start := time.Now()
	if err := d.Dispatch(t.Context(), assignment("t1", "a1")); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if d.ActiveTaskCount() != 1 || !slices.Equal(d.ActiveTaskIDs(), []string{"t1"}) || d.LastAssignmentAt() == nil {
		t.Errorf("state after Dispatch: count %d ids %v last %v", d.ActiveTaskCount(), d.ActiveTaskIDs(), d.LastAssignmentAt())
	}

	waitFor(t, 5*time.Second, "terminal ledger event", func() bool { return ledgerHas(l, "t1", simworker.KindTerminal) })
	waitFor(t, 5*time.Second, "attempt release", func() bool { return !slices.Contains(d.ActiveAttemptIDs(), "a1") })
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("task finished after %v, want at least its 50ms duration", elapsed)
	}

	hookMu.Lock()
	if !sawTerminal || !heldAtTerminal {
		t.Errorf("terminal published %v, attempt held while publishing %v; want both", sawTerminal, heldAtTerminal)
	}
	hookMu.Unlock()
	if d.ActiveTaskCount() != 0 {
		t.Errorf("ActiveTaskCount = %d after the terminal, want 0", d.ActiveTaskCount())
	}

	// Publish order: running, then every log chunk, then the terminal.
	msgs := conn.snapshot()
	var order []string
	var got []string
	for _, m := range msgs {
		switch {
		case strings.HasPrefix(m.subject, "task.status."):
			var s protocol.TaskStatusMsg
			if err := json.Unmarshal(m.data, &s); err != nil {
				t.Fatal(err)
			}
			order = append(order, s.Status)
			if s.Status == "succeeded" && (s.ExitCode == nil || *s.ExitCode != 0) {
				t.Errorf("succeeded exit code = %v, want 0", s.ExitCode)
			}
		case strings.HasPrefix(m.subject, "task.logs."):
			var c protocol.LogChunkMsg
			if err := json.Unmarshal(m.data, &c); err != nil {
				t.Fatal(err)
			}
			order = append(order, "logs")
			got = append(got, strings.Split(c.Data, "\n")...)
		default:
			t.Errorf("unexpected subject %q", m.subject)
		}
	}
	if len(order) < 3 || order[0] != "running" || order[len(order)-1] != "succeeded" {
		t.Fatalf("publish order = %v, want running, logs..., succeeded", order)
	}
	for _, o := range order[1 : len(order)-1] {
		if o != "logs" {
			t.Errorf("publish order = %v, want only log chunks between running and succeeded", order)
		}
	}
	var want []string
	for n := 1; n <= lines; n++ {
		want = append(want, simworker.LogLine("t1", n, lineBytes))
	}
	if !slices.Equal(got, want) {
		t.Errorf("log lines = %q, want %q", got, want)
	}

	var kinds []string
	for _, e := range l.Events() {
		kinds = append(kinds, e.Kind)
		if e.WorkerID != testWorkerID || e.AttemptID != "a1" || e.JobID != "j1" {
			t.Errorf("event %+v has the wrong identity", e)
		}
		if e.Kind == simworker.KindTerminal && (e.Outcome != simworker.OutcomeSucceeded || e.LogLines != lines) {
			t.Errorf("terminal event = %+v, want succeeded with %d lines", e, lines)
		}
	}
	if !slices.Equal(kinds, []string{simworker.KindLeased, simworker.KindRunning, simworker.KindTerminal}) {
		t.Errorf("ledger kinds = %v", kinds)
	}
}

func TestDispatcher_HangThenCancel(t *testing.T) {
	d, conn, l := newTestDispatcher(t, simworker.TaskPlan{
		Duration: 10 * time.Millisecond, Outcome: simworker.OutcomeHang, LogLines: 2, LineBytes: 10,
	})
	if err := d.Dispatch(t.Context(), assignment("t1", "a1")); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	waitFor(t, 5*time.Second, "running", func() bool { return ledgerHas(l, "t1", simworker.KindRunning) })
	time.Sleep(200 * time.Millisecond)
	if st := conn.statuses(t); len(st) != 1 || st[0].Status != "running" {
		t.Fatalf("statuses before cancel = %+v, want only running", st)
	}
	if !slices.Contains(d.ActiveAttemptIDs(), "a1") {
		t.Fatal("hanging attempt not reported as held")
	}

	if d.Cancel("unknown") {
		t.Error("Cancel(unknown) = true, want false")
	}
	if !d.Cancel("t1") {
		t.Fatal("Cancel(t1) = false, want true")
	}
	waitFor(t, 5*time.Second, "canceled ledger event", func() bool { return ledgerHas(l, "t1", simworker.KindCanceled) })
	waitFor(t, 5*time.Second, "attempt release", func() bool { return len(d.ActiveAttemptIDs()) == 0 })

	st := conn.statuses(t)
	if len(st) != 2 || st[1].Status != "canceled" || st[1].ExitCode != nil {
		t.Errorf("statuses = %+v, want running then canceled with no exit code", st)
	}
	if ledgerHas(l, "t1", simworker.KindTerminal) {
		t.Error("a canceled task also recorded a terminal event")
	}
	for _, e := range l.Events() {
		if e.Kind == simworker.KindCanceled && e.LogLines != 2 {
			t.Errorf("canceled event lines = %d, want 2", e.LogLines)
		}
	}
	if d.Cancel("t1") {
		t.Error("Cancel after the task ended = true, want false")
	}
}

func TestDispatcher_FailedPath(t *testing.T) {
	d, conn, l := newTestDispatcher(t, simworker.TaskPlan{Duration: 10 * time.Millisecond, Outcome: simworker.OutcomeFailed})
	if err := d.Dispatch(t.Context(), assignment("t1", "a1")); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	waitFor(t, 5*time.Second, "terminal ledger event", func() bool { return ledgerHas(l, "t1", simworker.KindTerminal) })

	st := conn.statuses(t)
	if len(st) != 2 {
		t.Fatalf("statuses = %+v, want running then failed", st)
	}
	f := st[1]
	if f.Status != "failed" || f.Message == "" || f.ExitCode == nil || *f.ExitCode == 0 {
		t.Errorf("terminal = status %q message %q exit %v, want failed with a reason and a non-zero exit", f.Status, f.Message, f.ExitCode)
	}
	for _, e := range l.Events() {
		if e.Kind == simworker.KindTerminal && e.Outcome != simworker.OutcomeFailed {
			t.Errorf("terminal outcome = %q, want failed", e.Outcome)
		}
	}
}
