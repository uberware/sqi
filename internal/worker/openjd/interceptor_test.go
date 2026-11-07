// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/worker/openjd"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// ── Test doubles ──────────────────────────────────────────────────────────────

// mockDownstream records all lines forwarded to it.
type mockDownstream struct {
	mu    sync.Mutex
	lines []capturedLine
}

type capturedLine struct {
	taskID, attemptID, sessionID, stream, line string
}

func (m *mockDownstream) HandleLine(_ context.Context, taskID, attemptID, sessionID, stream, line string) {
	m.mu.Lock()
	m.lines = append(m.lines, capturedLine{taskID, attemptID, sessionID, stream, line})
	m.mu.Unlock()
}

func (m *mockDownstream) snapshot() []capturedLine {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]capturedLine, len(m.lines))
	copy(out, m.lines)
	return out
}

// mockNATS captures messages published to it.
type mockNATS struct {
	mu   sync.Mutex
	msgs []capturedMsg
}

type capturedMsg struct {
	subject string
	data    []byte
}

func (m *mockNATS) Publish(subj string, data []byte) error {
	cp := make([]byte, len(data))
	copy(cp, data)
	m.mu.Lock()
	m.msgs = append(m.msgs, capturedMsg{subject: subj, data: cp})
	m.mu.Unlock()
	return nil
}

func (m *mockNATS) snapshot() []capturedMsg {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]capturedMsg, len(m.msgs))
	copy(out, m.msgs)
	return out
}

// decodeStatus unmarshals a captured NATS message into a TaskStatusMsg.
func decodeStatus(t *testing.T, data []byte) protocol.TaskStatusMsg {
	t.Helper()
	var msg protocol.TaskStatusMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("decodeStatus: %v", err)
	}
	return msg
}

// setup builds a registered Interceptor with mocked downstream and NATS.
// It returns the interceptor, downstream mock, NATS mock, and a context.
// cancel must be called to trigger the per-task cancel (simulates openjd_fail).
func setup(t *testing.T) (*openjd.Interceptor, *mockDownstream, *mockNATS, context.Context, context.CancelFunc) {
	t.Helper()
	downstream := &mockDownstream{}
	nc := &mockNATS{}
	logger := discardLogger()
	i := openjd.New(downstream, nc, "test-worker", logger)

	ctx := context.Background()
	cancelCtx, cancel := context.WithCancel(ctx)

	const attemptID = "attempt-1"
	i.RegisterTask(attemptID, "task-1", "job-1", "sess-1", cancel)
	return i, downstream, nc, cancelCtx, cancel
}

// discardLogger returns a slog.Logger that discards all output.
func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// ── openjd_progress tests ─────────────────────────────────────────────────────

// TestProgressDirectiveUpdatesState verifies that a valid openjd_progress line
// updates the in-memory progress percentage and is NOT forwarded.
func TestProgressDirectiveUpdatesState(t *testing.T) {
	i, downstream, _, ctx, _ := setup(t)
	const attemptID = "attempt-1"

	i.HandleLine(ctx, "task-1", attemptID, "sess-1", "stdout", "openjd_progress: 42")

	if got := downstream.snapshot(); len(got) != 0 {
		t.Errorf("openjd_progress line was forwarded to downstream (%d lines), want 0", len(got))
	}

	p := i.LastProgress(attemptID)
	if p == nil {
		t.Fatal("LastProgress returned nil, want 42")
	}
	if *p != 42 {
		t.Errorf("LastProgress = %d, want 42", *p)
	}
}

// TestProgressDirectiveBoundaryValues verifies that the boundary values 0 and
// 100 are accepted and that the in-memory state is updated accordingly.
func TestProgressDirectiveBoundaryValues(t *testing.T) {
	i, _, _, ctx, _ := setup(t)
	const attemptID = "attempt-1"

	i.HandleLine(ctx, "task-1", attemptID, "sess-1", "stdout", "openjd_progress: 0")
	p := i.LastProgress(attemptID)
	if p == nil || *p != 0 {
		t.Errorf("LastProgress after 0 = %v, want &0", p)
	}

	i.HandleLine(ctx, "task-1", attemptID, "sess-1", "stdout", "openjd_progress: 100")
	p = i.LastProgress(attemptID)
	if p == nil || *p != 100 {
		t.Errorf("LastProgress after 100 = %v, want &100", p)
	}
}

// TestProgressInvalidValueIgnored verifies that a malformed openjd_progress
// line is silently dropped and NOT forwarded to downstream.
func TestProgressInvalidValueIgnored(t *testing.T) {
	for _, line := range []string{
		"openjd_progress: abc",
		"openjd_progress: -1",
		"openjd_progress: 101",
		"openjd_progress: ",
		"openjd_progress: NaN",
		"openjd_progress: Inf",
		"openjd_progress: -Inf",
		"openjd_progress: 100.5",
		"openjd_progress: -0.5",
		"openjd_progress: 1e3",
	} {
		t.Run(line, func(t *testing.T) {
			downstream := &mockDownstream{}
			nc := &mockNATS{}
			i := openjd.New(downstream, nc, "", discardLogger())
			i.RegisterTask("a", "t", "j", "s", func() {})
			i.HandleLine(context.Background(), "t", "a", "s", "stdout", line)
			if got := downstream.snapshot(); len(got) != 0 {
				t.Errorf("invalid openjd_progress was forwarded (%d lines)", len(got))
			}
			if p := i.LastProgress("a"); p != nil {
				t.Errorf("LastProgress = %d, want nil for invalid input", *p)
			}
		})
	}
}

// ── openjd_status tests ───────────────────────────────────────────────────────

// TestStatusDirectivePublishesNATS verifies that an openjd_status line
// publishes an intermediate "running" status to the correct NATS subject and
// is NOT forwarded to the downstream handler.
func TestStatusDirectivePublishesNATS(t *testing.T) {
	i, downstream, nc, ctx, _ := setup(t)
	const attemptID = "attempt-1"

	i.HandleLine(ctx, "task-1", attemptID, "sess-1", "stdout", "openjd_status: rendering frame 10")

	if got := downstream.snapshot(); len(got) != 0 {
		t.Errorf("openjd_status line was forwarded to downstream (%d lines), want 0", len(got))
	}

	msgs := nc.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 NATS message, got %d", len(msgs))
	}
	wantSubj := bus.TaskStatusSubject("test-worker", "job-1")
	if msgs[0].subject != wantSubj {
		t.Errorf("NATS subject = %q, want %q", msgs[0].subject, wantSubj)
	}

	status := decodeStatus(t, msgs[0].data)
	if status.Status != "running" {
		t.Errorf("Status = %q, want \"running\"", status.Status)
	}
	if status.Message != "rendering frame 10" {
		t.Errorf("Message = %q, want %q", status.Message, "rendering frame 10")
	}
	if status.TaskID != "task-1" {
		t.Errorf("TaskID = %q, want %q", status.TaskID, "task-1")
	}
	if status.AttemptID != attemptID {
		t.Errorf("AttemptID = %q, want %q", status.AttemptID, attemptID)
	}
	if status.JobID != "job-1" {
		t.Errorf("JobID = %q, want %q", status.JobID, "job-1")
	}
	if status.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want %q", status.SessionID, "sess-1")
	}
}

// TestStatusDirectiveSanitizesText verifies that a task cannot publish a NUL
// byte or an unbounded status text.
func TestStatusDirectiveSanitizesText(t *testing.T) {
	i, _, nc, ctx, _ := setup(t)

	line := "openjd_status: bad\x00te\x01xt " + strings.Repeat("x", 5000)
	i.HandleLine(ctx, "task-1", "attempt-1", "sess-1", "stdout", line)

	msgs := nc.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 NATS message, got %d", len(msgs))
	}
	got := decodeStatus(t, msgs[0].data).Message
	if strings.ContainsAny(got, "\x00\x01") {
		t.Errorf("Message %q still holds control characters", got)
	}
	if len(got) != protocol.MaxStatusTextBytes || !strings.HasPrefix(got, "badtext xxx") {
		t.Errorf("Message len = %d, prefix %q; want %d bytes starting %q",
			len(got), got[:min(len(got), 11)], protocol.MaxStatusTextBytes, "badtext xxx")
	}
}

// TestStatusDirectiveMultiple verifies that status updates are throttled: two
// different status lines back to back publish one message at once and the
// second after the throttle window.
func TestStatusDirectiveMultiple(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		i, _, nc, ctx, _ := setup(t)
		const attemptID = "attempt-1"

		i.HandleLine(ctx, "task-1", attemptID, "sess-1", "stdout", "openjd_status: step 1")
		i.HandleLine(ctx, "task-1", attemptID, "sess-1", "stdout", "openjd_status: step 2")
		synctest.Wait()
		if got := len(nc.snapshot()); got != 1 {
			t.Fatalf("expected 1 NATS message at once, got %d", got)
		}

		time.Sleep(openjd.DefaultProgressInterval + 100*time.Millisecond)
		synctest.Wait()
		msgs := nc.snapshot()
		if len(msgs) != 2 {
			t.Fatalf("expected 2 NATS messages after the window, got %d", len(msgs))
		}
		if m := decodeStatus(t, msgs[1].data).Message; m != "step 2" {
			t.Errorf("second Message = %q, want %q", m, "step 2")
		}
	})
}

// ── snapshot and throttle tests ───────────────────────────────────────────────

func TestProgressParsesFloats(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"0.0", 0}, {"50.0", 50}, {"43.5", 43}, {"99.99", 99}, {"100", 100}, {" 7 ", 7},
	} {
		t.Run(tc.in, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				i, _, _, ctx, _ := setup(t)
				i.HandleLine(ctx, "task-1", "attempt-1", "sess-1", "stdout", "openjd_progress: "+tc.in)
				p := i.LastProgress("attempt-1")
				if p == nil || *p != tc.want {
					t.Errorf("LastProgress = %v, want %d", p, tc.want)
				}
			})
		})
	}
}

func TestSnapshot_ProgressOnlyLineSends(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		i, _, nc, ctx, _ := setup(t)
		i.HandleLine(ctx, "task-1", "attempt-1", "sess-1", "stdout", "openjd_progress: 42")
		synctest.Wait()

		msgs := nc.snapshot()
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		if want := bus.TaskStatusSubject("test-worker", "job-1"); msgs[0].subject != want {
			t.Errorf("subject = %q, want %q", msgs[0].subject, want)
		}
		m := decodeStatus(t, msgs[0].data)
		if m.Status != "running" || m.Message != "" || m.LastProgress == nil || *m.LastProgress != 42 {
			t.Errorf("message = %+v, want running, no Message, LastProgress 42", m)
		}
	})
}

func TestSnapshot_StatusKeptAcrossProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		i, _, nc, ctx, _ := setup(t)
		i.HandleLine(ctx, "task-1", "attempt-1", "sess-1", "stdout", "openjd_status: frame 1")
		time.Sleep(2 * time.Second)
		i.HandleLine(ctx, "task-1", "attempt-1", "sess-1", "stdout", "openjd_progress: 10")
		time.Sleep(2 * time.Second)
		synctest.Wait()

		msgs := nc.snapshot()
		if len(msgs) != 2 {
			t.Fatalf("got %d messages, want 2", len(msgs))
		}
		m := decodeStatus(t, msgs[1].data)
		if m.Message != "frame 1" || m.LastProgress == nil || *m.LastProgress != 10 {
			t.Errorf("second message = %+v, want Message frame 1, LastProgress 10", m)
		}
	})
}

func TestThrottle_LeadingAndTrailing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		i, _, nc, ctx, _ := setup(t)
		send := func(v int) {
			i.HandleLine(ctx, "task-1", "attempt-1", "sess-1", "stdout", fmt.Sprintf("openjd_progress: %d", v))
		}
		send(1)
		if got := len(nc.snapshot()); got != 1 {
			t.Fatalf("leading: got %d messages, want 1", got)
		}
		time.Sleep(500 * time.Millisecond)
		send(2)
		time.Sleep(500 * time.Millisecond)
		send(3)
		if got := len(nc.snapshot()); got != 1 {
			t.Fatalf("within window: got %d messages, want 1", got)
		}
		time.Sleep(1100 * time.Millisecond)
		synctest.Wait()
		msgs := nc.snapshot()
		if len(msgs) != 2 {
			t.Fatalf("after window: got %d messages, want 2", len(msgs))
		}
		if m := decodeStatus(t, msgs[1].data); m.LastProgress == nil || *m.LastProgress != 3 {
			t.Errorf("trailing LastProgress = %v, want 3", m.LastProgress)
		}
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if got := len(nc.snapshot()); got != 2 {
			t.Errorf("got %d messages after quiet period, want 2", got)
		}
	})
}

func TestThrottle_UnchangedValuesDoNotSend(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		i, _, nc, ctx, _ := setup(t)
		line := func(l string) { i.HandleLine(ctx, "task-1", "attempt-1", "sess-1", "stdout", l) }
		line("openjd_progress: 5")
		line("openjd_status: x")
		time.Sleep(3 * time.Second)
		line("openjd_progress: 5")
		line("openjd_status: x")
		time.Sleep(3 * time.Second)
		synctest.Wait()
		if got := len(nc.snapshot()); got != 2 {
			t.Errorf("got %d messages, want 2", got)
		}
	})
}

func TestFinishProgress_StopsTrailingSend(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		i, _, nc, ctx, _ := setup(t)
		line := func(l string) { i.HandleLine(ctx, "task-1", "attempt-1", "sess-1", "stdout", l) }
		line("openjd_progress: 1")
		line("openjd_progress: 2")
		p := i.FinishProgress("attempt-1")
		if p == nil || *p != 2 {
			t.Fatalf("FinishProgress = %v, want &2", p)
		}
		time.Sleep(5 * time.Second)
		synctest.Wait()
		line("openjd_progress: 3")
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if got := len(nc.snapshot()); got != 1 {
			t.Errorf("got %d messages, want 1", got)
		}
	})
}

func TestDeregister_StopsTrailingSend(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		i, _, nc, ctx, _ := setup(t)
		line := func(l string) { i.HandleLine(ctx, "task-1", "attempt-1", "sess-1", "stdout", l) }
		line("openjd_progress: 1")
		line("openjd_progress: 2")
		i.Deregister("attempt-1")
		time.Sleep(5 * time.Second)
		synctest.Wait()
		line("openjd_progress: 3")
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if got := len(nc.snapshot()); got != 1 {
			t.Errorf("got %d messages, want 1", got)
		}
	})
}

// TestStdoutMessagesSample feeds the directive lines of the OpenJD
// stdout-messages sample job template.
func TestStdoutMessagesSample(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		i, _, nc, ctx, _ := setup(t)
		for _, l := range []string{
			"openjd_progress: 0.0",
			"openjd_status: Loading the things",
			"openjd_progress: 50.0",
			"openjd_status: Processing the things",
			"openjd_progress: 100",
		} {
			i.HandleLine(ctx, "task-1", "attempt-1", "sess-1", "stdout", l)
			time.Sleep(3 * time.Second)
		}
		synctest.Wait()
		if p := i.LastProgress("attempt-1"); p == nil || *p != 100 {
			t.Errorf("LastProgress = %v, want 100", p)
		}
		msgs := nc.snapshot()
		if len(msgs) == 0 {
			t.Fatal("no messages published")
		}
		m := decodeStatus(t, msgs[len(msgs)-1].data)
		if m.Message != "Processing the things" || m.LastProgress == nil || *m.LastProgress != 100 {
			t.Errorf("last message = %+v, want Processing the things at 100", m)
		}
	})
}

// ── openjd_fail tests ─────────────────────────────────────────────────────────

// TestFailDirectiveCancelsAndStoresReason verifies that an openjd_fail line
// calls the cancel function, stores the failure reason, and is NOT forwarded.
func TestFailDirectiveCancelsAndStoresReason(t *testing.T) {
	i, downstream, _, cancelCtx, _ := setup(t)
	const attemptID = "attempt-1"

	i.HandleLine(cancelCtx, "task-1", attemptID, "sess-1", "stdout", "openjd_fail: out of memory")

	// Line must not be forwarded.
	if got := downstream.snapshot(); len(got) != 0 {
		t.Errorf("openjd_fail line was forwarded to downstream (%d lines), want 0", len(got))
	}

	// Fail reason must be stored.
	reason, ok := i.TakeFailReason(attemptID)
	if !ok {
		t.Fatal("TakeFailReason returned ok=false, want true")
	}
	if reason != "out of memory" {
		t.Errorf("TakeFailReason reason = %q, want %q", reason, "out of memory")
	}

	// Cancel must have been called: the derived context should be done.
	select {
	case <-cancelCtx.Done():
		// expected
	default:
		t.Error("context was not canceled after openjd_fail")
	}
}

// TestFailDirectiveOnlyFirstTriggers verifies that when a process emits
// multiple openjd_fail lines, only the first triggers the cancel and stores the
// reason.
func TestFailDirectiveOnlyFirstTriggers(t *testing.T) {
	cancelCount := 0
	downstream := &mockDownstream{}
	nc := &mockNATS{}
	i := openjd.New(downstream, nc, "", discardLogger())
	i.RegisterTask("a", "t", "j", "s", func() { cancelCount++ })

	i.HandleLine(context.Background(), "t", "a", "s", "stdout", "openjd_fail: first reason")
	i.HandleLine(context.Background(), "t", "a", "s", "stdout", "openjd_fail: second reason")

	if cancelCount != 1 {
		t.Errorf("cancel called %d times, want 1", cancelCount)
	}

	reason, ok := i.TakeFailReason("a")
	if !ok || reason != "first reason" {
		t.Errorf("TakeFailReason = (%q, %v), want (\"first reason\", true)", reason, ok)
	}
}

// TestFailDirectiveNoForward verifies the fail line is not forwarded even when
// the fail text is empty.
func TestFailDirectiveEmptyReason(t *testing.T) {
	downstream := &mockDownstream{}
	nc := &mockNATS{}
	i := openjd.New(downstream, nc, "", discardLogger())
	i.RegisterTask("a", "t", "j", "s", func() {})

	i.HandleLine(context.Background(), "t", "a", "s", "stdout", "openjd_fail:")

	if got := downstream.snapshot(); len(got) != 0 {
		t.Errorf("openjd_fail line was forwarded (%d lines), want 0", len(got))
	}
	reason, ok := i.TakeFailReason("a")
	if !ok {
		t.Fatal("TakeFailReason ok=false, want true")
	}
	if reason != "" {
		t.Errorf("reason = %q, want empty string", reason)
	}
}

// ── pass-through tests ────────────────────────────────────────────────────────

// TestUnrecognizedOpenJDPrefixForwarded verifies that lines beginning with
// openjd_ but not matching a known directive are forwarded unmodified.
func TestUnrecognizedOpenJDPrefixForwarded(t *testing.T) {
	i, downstream, _, ctx, _ := setup(t)
	const attemptID = "attempt-1"

	line := "openjd_unknown: some value"
	i.HandleLine(ctx, "task-1", attemptID, "sess-1", "stdout", line)

	got := downstream.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 forwarded line, got %d", len(got))
	}
	if got[0].line != line {
		t.Errorf("forwarded line = %q, want %q", got[0].line, line)
	}
}

// TestNormalLinesForwarded verifies that ordinary stdout lines are forwarded
// unchanged to the downstream handler.
func TestNormalLinesForwarded(t *testing.T) {
	i, downstream, _, ctx, _ := setup(t)
	const attemptID = "attempt-1"

	lines := []string{
		"Rendering frame 1 of 100",
		"[INFO] Scene loaded",
		"",
	}
	for _, l := range lines {
		i.HandleLine(ctx, "task-1", attemptID, "sess-1", "stdout", l)
	}

	got := downstream.snapshot()
	if len(got) != len(lines) {
		t.Fatalf("expected %d forwarded lines, got %d", len(lines), len(got))
	}
	for idx, cl := range got {
		if cl.line != lines[idx] {
			t.Errorf("[%d] forwarded line = %q, want %q", idx, cl.line, lines[idx])
		}
		if cl.stream != "stdout" {
			t.Errorf("[%d] stream = %q, want \"stdout\"", idx, cl.stream)
		}
	}
}

// TestStderrAlwaysForwarded verifies that all stderr lines — including those
// that contain openjd_ prefixes — are forwarded unmodified without interception.
func TestStderrAlwaysForwarded(t *testing.T) {
	i, downstream, nc, ctx, _ := setup(t)
	const attemptID = "attempt-1"

	stderrLines := []string{
		"openjd_progress: 50",
		"openjd_status: should not publish",
		"openjd_fail: should not cancel",
		"normal stderr line",
	}
	for _, l := range stderrLines {
		i.HandleLine(ctx, "task-1", attemptID, "sess-1", "stderr", l)
	}

	// All lines must be forwarded.
	got := downstream.snapshot()
	if len(got) != len(stderrLines) {
		t.Fatalf("expected %d forwarded lines, got %d", len(stderrLines), len(got))
	}
	for idx, cl := range got {
		if cl.line != stderrLines[idx] {
			t.Errorf("[%d] line = %q, want %q", idx, cl.line, stderrLines[idx])
		}
		if cl.stream != "stderr" {
			t.Errorf("[%d] stream = %q, want \"stderr\"", idx, cl.stream)
		}
	}

	// No NATS messages should have been published (openjd_status on stderr).
	if n := len(nc.snapshot()); n != 0 {
		t.Errorf("expected 0 NATS messages for stderr directives, got %d", n)
	}

	// openjd_fail on stderr must not set a fail reason.
	if reason, ok := i.TakeFailReason(attemptID); ok {
		t.Errorf("TakeFailReason = (%q, true) after stderr openjd_fail, want false", reason)
	}

	// openjd_progress on stderr must not update in-memory progress state.
	if p := i.LastProgress(attemptID); p != nil {
		t.Errorf("LastProgress = %d, want nil (stderr openjd_progress must not update state)", *p)
	}
}

// ── lifecycle tests ───────────────────────────────────────────────────────────

// TestDeregisterClearsState verifies that after Deregister, TakeFailReason and
// LastProgress return zero values and HandleLine is a no-op for directives.
func TestDeregisterClearsState(t *testing.T) {
	i, downstream, _, ctx, _ := setup(t)
	const attemptID = "attempt-1"

	i.HandleLine(ctx, "task-1", attemptID, "sess-1", "stdout", "openjd_progress: 55")
	i.Deregister(attemptID)

	if p := i.LastProgress(attemptID); p != nil {
		t.Errorf("LastProgress after Deregister = %d, want nil", *p)
	}
	if _, ok := i.TakeFailReason(attemptID); ok {
		t.Error("TakeFailReason returned ok=true after Deregister")
	}

	// Directive lines after deregister must not be forwarded (no registration).
	i.HandleLine(ctx, "task-1", attemptID, "sess-1", "stdout", "openjd_progress: 60")
	if got := downstream.snapshot(); len(got) != 0 {
		t.Errorf("directive forwarded after Deregister: %v", got)
	}
}

// TestUnregisteredAttemptPassThroughNonDirective verifies that non-directive
// stdout lines from an unregistered attempt still reach the downstream.
func TestUnregisteredAttemptPassThroughNonDirective(t *testing.T) {
	downstream := &mockDownstream{}
	nc := &mockNATS{}
	i := openjd.New(downstream, nc, "", discardLogger())
	// No RegisterTask call.

	i.HandleLine(context.Background(), "t", "unregistered", "s", "stdout", "hello world")

	got := downstream.snapshot()
	if len(got) != 1 || got[0].line != "hello world" {
		t.Errorf("expected \"hello world\" forwarded; got %v", got)
	}
}

// TestTakeFailReasonIdempotent verifies that TakeFailReason is idempotent —
// calling it multiple times returns the same reason without clearing it.
func TestTakeFailReasonIdempotent(t *testing.T) {
	i, _, _, ctx, _ := setup(t)
	const attemptID = "attempt-1"

	i.HandleLine(ctx, "task-1", attemptID, "sess-1", "stdout", "openjd_fail: disk full")

	for range 3 {
		reason, ok := i.TakeFailReason(attemptID)
		if !ok || reason != "disk full" {
			t.Errorf("TakeFailReason = (%q, %v), want (\"disk full\", true)", reason, ok)
		}
	}
}

// TestConcurrentHandleLine verifies that concurrent HandleLine calls from
// multiple goroutines (simulating the two scanner goroutines per task in the
// executor) do not race on the Interceptor's internal state.
//
// Run with: go test -race ./internal/worker/openjd/...
func TestConcurrentHandleLine(t *testing.T) {
	t.Parallel()
	downstream := &mockDownstream{}
	nc := &mockNATS{}
	i := openjd.New(downstream, nc, "", discardLogger())

	const N = 50
	// Register N attempts so concurrent goroutines don't contend on a single
	// attemptState lock — we want to stress the outer attempts map too.
	for idx := range N {
		id := fmt.Sprintf("attempt-%d", idx)
		i.RegisterTask(id, "task", "job", "sess", func() {})
	}

	var wg sync.WaitGroup
	for idx := range N {
		wg.Add(2)
		id := fmt.Sprintf("attempt-%d", idx)
		// stdout goroutine: sends a mix of directives and normal lines
		go func(attemptID string) {
			defer wg.Done()
			for j := range 20 {
				switch j % 4 {
				case 0:
					i.HandleLine(context.Background(), "task", attemptID, "sess", "stdout",
						fmt.Sprintf("openjd_progress: %d", j*5%101))
				case 1:
					i.HandleLine(context.Background(), "task", attemptID, "sess", "stdout",
						fmt.Sprintf("openjd_status: step %d", j))
				case 2:
					i.HandleLine(context.Background(), "task", attemptID, "sess", "stdout",
						"openjd_unknown: ignored")
				case 3:
					i.HandleLine(context.Background(), "task", attemptID, "sess", "stdout",
						fmt.Sprintf("normal line %d", j))
				}
			}
		}(id)
		// stderr goroutine: all lines pass through
		go func(attemptID string) {
			defer wg.Done()
			for j := range 20 {
				i.HandleLine(context.Background(), "task", attemptID, "sess", "stderr",
					fmt.Sprintf("stderr line %d", j))
			}
		}(id)
	}
	wg.Wait()

	// Deregister all attempts; no panic or deadlock must occur.
	for idx := range N {
		i.Deregister(fmt.Sprintf("attempt-%d", idx))
	}
}
