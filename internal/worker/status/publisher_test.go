// SPDX-License-Identifier: AGPL-3.0-or-later

package status_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/status"
)

// ── Test doubles ──────────────────────────────────────────────────────────────

// sent is one PublishAcked call the stub transport saw.
type sent struct {
	subj  string
	msgID string
	msg   protocol.TaskStatusMsg
}

// stubTransport records every PublishAcked call and answers it with fail's
// verdict. fail receives the message ID and how many times that ID has been
// tried, this call included; a nil fail acks everything.
type stubTransport struct {
	mu    sync.Mutex
	calls []sent
	tries map[string]int
	fail  func(msgID string, try int) error
}

func (s *stubTransport) PublishAcked(_ context.Context, subj string, data []byte, msgID string) error {
	var m protocol.TaskStatusMsg
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("stub: decode: %w", err)
	}
	s.mu.Lock()
	if s.tries == nil {
		s.tries = map[string]int{}
	}
	s.tries[msgID]++
	try := s.tries[msgID]
	s.calls = append(s.calls, sent{subj: subj, msgID: msgID, msg: m})
	fail := s.fail
	s.mu.Unlock()
	if fail != nil {
		return fail(msgID, try)
	}
	return nil
}

func (s *stubTransport) snapshot() []sent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

func (s *stubTransport) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// countID returns how many calls carried msgID.
func (s *stubTransport) countID(msgID string) int {
	n := 0
	for _, c := range s.snapshot() {
		if c.msgID == msgID {
			n++
		}
	}
	return n
}

// logRecord is one record the capturing handler saw.
type logRecord struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

// captureHandler is a slog.Handler that keeps every record.
type captureHandler struct {
	mu      *sync.Mutex
	records *[]logRecord
}

func newCaptureHandler() *captureHandler {
	return &captureHandler{mu: &sync.Mutex{}, records: &[]logRecord{}}
}

func (*captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	rec := logRecord{level: r.Level, msg: r.Message, attrs: map[string]string{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	*h.records = append(*h.records, rec)
	h.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *captureHandler) WithGroup(string) slog.Handler { return h }

// count returns how many records at level carry attempt_id == attemptID, and,
// when msg is not empty, that message.
func (h *captureHandler) count(level slog.Level, attemptID, msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range *h.records {
		if r.level != level || (attemptID != "" && r.attrs["attempt_id"] != attemptID) {
			continue
		}
		if msg != "" && r.msg != msg {
			continue
		}
		n++
	}
	return n
}

// ── Helpers ───────────────────────────────────────────────────────────────────

const testWorker = "worker-abc"

func assign(attemptID string) *protocol.AssignMsg {
	return &protocol.AssignMsg{
		Version:   protocol.ProtocolVersion,
		Type:      protocol.TypeAssign,
		TaskID:    "task-" + attemptID,
		AttemptID: attemptID,
		JobID:     "job-1",
	}
}

func newPub(tr *stubTransport) (*status.Publisher, *captureHandler) {
	h := newCaptureHandler()
	p := status.New(tr, status.Config{
		WorkerID:  testWorker,
		RetryBase: time.Millisecond,
		RetryMax:  5 * time.Millisecond,
	}, slog.New(h))
	return p, h
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// stable fails the test if f's value changes over d.
func stable(t *testing.T, what string, d time.Duration, f func() int) {
	t.Helper()
	before := f()
	time.Sleep(d)
	if after := f(); after != before {
		t.Errorf("%s changed from %d to %d", what, before, after)
	}
}

var errTimeout = errors.New("nats: timeout")

func succeeded(p *status.Publisher, attemptID string) {
	code := 0
	p.Terminal(assign(attemptID), "sess-1", "succeeded", &code, "", nil, time.Now())
}

// ── Message contents ──────────────────────────────────────────────────────────

func TestPublisher_RunningFields(t *testing.T) {
	tr := &stubTransport{}
	p, _ := newPub(tr)
	at := time.Now().UTC().Truncate(time.Second)

	p.Running(assign("a1"), "sess-1", at)

	calls := tr.snapshot()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	c := calls[0]
	if c.subj != "task.status."+testWorker+".job-1" {
		t.Errorf("subject = %q", c.subj)
	}
	m := c.msg
	if m.Status != "running" || m.TaskID != "task-a1" || m.AttemptID != "a1" || m.JobID != "job-1" ||
		m.SessionID != "sess-1" || m.WorkerID != testWorker || m.Type != protocol.TypeTaskStatus ||
		m.Version != protocol.ProtocolVersion {
		t.Errorf("running message = %+v", m)
	}
	if m.LastProgress != nil || m.ExitCode != nil || m.EndedAgoMs != 0 {
		t.Errorf("running carries progress %v, exit %v, age %d; want none", m.LastProgress, m.ExitCode, m.EndedAgoMs)
	}
	if !m.At.Equal(at) {
		t.Errorf("At = %v, want %v", m.At, at)
	}
}

func TestPublisher_TerminalFields(t *testing.T) {
	tr := &stubTransport{}
	p, _ := newPub(tr)
	code, progress := 2, 75
	endedAt := time.Now()

	p.Terminal(assign("a1"), "sess-1", "failed", &code, "render crashed", &progress, endedAt)

	m := tr.snapshot()[0].msg
	if m.Status != "failed" || m.Message != "render crashed" || m.WorkerID != testWorker {
		t.Errorf("terminal message = %+v", m)
	}
	if m.ExitCode == nil || *m.ExitCode != 2 {
		t.Errorf("ExitCode = %v, want 2", m.ExitCode)
	}
	if m.LastProgress == nil || *m.LastProgress != 75 {
		t.Errorf("LastProgress = %v, want 75", m.LastProgress)
	}
	if !m.At.Equal(endedAt.Round(0)) {
		t.Errorf("At = %v, want %v", m.At, endedAt)
	}
}

func TestPublisher_WireOffsetMovesOnlyAt(t *testing.T) {
	const offset = 10 * time.Minute
	tr := &stubTransport{}
	p := status.New(tr, status.Config{WorkerID: testWorker, WireOffset: offset}, slog.New(slog.DiscardHandler))
	now := time.Now()

	p.Running(assign("a1"), "", now)
	code := 0
	p.Terminal(assign("a1"), "", "succeeded", &code, "", nil, now.Add(-time.Second))

	calls := tr.snapshot()
	if got := calls[0].msg.At; !got.Equal(now.Add(offset).Round(0)) {
		t.Errorf("running At = %v, want %v", got, now.Add(offset))
	}
	term := calls[1].msg
	if !term.At.Equal(now.Add(offset - time.Second).Round(0)) {
		t.Errorf("terminal At = %v, want %v", term.At, now.Add(offset-time.Second))
	}
	if term.EndedAgoMs < 1000 || term.EndedAgoMs > 60_000 {
		t.Errorf("EndedAgoMs = %d, want about 1000 (unaffected by the offset)", term.EndedAgoMs)
	}
}

// ── Acknowledgement and retry ─────────────────────────────────────────────────

func TestPublisher_TerminalAckedFirstTry(t *testing.T) {
	tr := &stubTransport{}
	p, _ := newPub(tr)

	succeeded(p, "a1")

	calls := tr.snapshot()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].msgID != "a1:succeeded" || calls[0].msg.Status != "succeeded" {
		t.Errorf("call = %q %q, want a1:succeeded succeeded", calls[0].msgID, calls[0].msg.Status)
	}
	if ids := p.PendingAttemptIDs(); len(ids) != 0 {
		t.Errorf("PendingAttemptIDs = %v, want empty", ids)
	}
}

func TestPublisher_RetriesUntilAcked(t *testing.T) {
	release := make(chan struct{})
	tr := &stubTransport{fail: func(_ string, try int) error {
		if try == 3 {
			<-release // hold the third try so the pending state can be observed
		}
		if try <= 3 {
			return errTimeout
		}
		return nil
	}}
	p, _ := newPub(tr)

	succeeded(p, "a1")

	waitUntil(t, "third try", func() bool { return tr.count() >= 3 })
	if ids := p.PendingAttemptIDs(); !slices.Equal(ids, []string{"a1"}) {
		t.Errorf("PendingAttemptIDs while failing = %v, want [a1]", ids)
	}
	close(release)
	waitUntil(t, "nothing pending", func() bool { return len(p.PendingAttemptIDs()) == 0 })

	calls := tr.snapshot()
	if len(calls) != 4 {
		t.Fatalf("calls = %d, want 4", len(calls))
	}
	for i, c := range calls {
		if c.msgID != "a1:succeeded" {
			t.Errorf("call %d msgID = %q, want a1:succeeded", i, c.msgID)
		}
	}
}

func TestPublisher_AgeGrowsAcrossRetries(t *testing.T) {
	tr := &stubTransport{fail: func(_ string, try int) error {
		if try < 3 {
			return errTimeout
		}
		return nil
	}}
	p, _ := newPub(tr)
	var mu sync.Mutex
	n := 0
	status.SetSince(p, func(time.Time) time.Duration {
		mu.Lock()
		defer mu.Unlock()
		n++
		return time.Duration(n) * 100 * time.Millisecond
	})

	succeeded(p, "a1")
	waitUntil(t, "nothing pending", func() bool { return len(p.PendingAttemptIDs()) == 0 })

	var got []int64
	for _, c := range tr.snapshot() {
		got = append(got, c.msg.EndedAgoMs)
	}
	if !slices.Equal(got, []int64{100, 200, 300}) {
		t.Errorf("EndedAgoMs across tries = %v, want [100 200 300]", got)
	}
}

func TestPublisher_NegativeAgeIsZero(t *testing.T) {
	tr := &stubTransport{}
	p, _ := newPub(tr)
	status.SetSince(p, func(time.Time) time.Duration { return -time.Second })

	succeeded(p, "a1")

	if got := tr.snapshot()[0].msg.EndedAgoMs; got != 0 {
		t.Errorf("EndedAgoMs = %d, want 0", got)
	}
}

func TestPublisher_ZeroEndedAtIsNow(t *testing.T) {
	tr := &stubTransport{}
	p, _ := newPub(tr)
	before := time.Now()

	code := -1
	p.Terminal(assign("a1"), "", "failed", &code, "boom", nil, time.Time{})

	m := tr.snapshot()[0].msg
	if m.EndedAgoMs != 0 {
		t.Errorf("EndedAgoMs = %d, want 0 for a zero endedAt", m.EndedAgoMs)
	}
	if m.At.Before(before.Round(0).Truncate(time.Second)) {
		t.Errorf("At = %v, want about now", m.At)
	}
}

func TestPublisher_MsgIDPerKind(t *testing.T) {
	code := 1
	tests := []struct {
		name   string
		send   func(p *status.Publisher)
		wantID string
	}{
		{"running", func(p *status.Publisher) { p.Running(assign("a1"), "", time.Now()) }, "a1:running"},
		{"succeeded", func(p *status.Publisher) { succeeded(p, "a1") }, "a1:succeeded"},
		{"failed", func(p *status.Publisher) {
			p.Terminal(assign("a1"), "", "failed", &code, "boom", nil, time.Now())
		}, "a1:failed"},
		{"canceled", func(p *status.Publisher) {
			p.Terminal(assign("a1"), "", "canceled", nil, "", nil, time.Now())
		}, "a1:canceled"},
		{"shutdown", func(p *status.Publisher) {
			p.Terminal(assign("a1"), "", "failed", nil, protocol.MessageWorkerShutdown, nil, time.Now())
		}, "a1:shutdown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := &stubTransport{}
			p, _ := newPub(tr)
			status.SetSince(p, func(time.Time) time.Duration { return 42 * time.Millisecond })
			tc.send(p)
			calls := tr.snapshot()
			if len(calls) != 1 {
				t.Fatalf("calls = %d, want 1", len(calls))
			}
			if calls[0].msgID != tc.wantID {
				t.Errorf("msgID = %q, want %q", calls[0].msgID, tc.wantID)
			}
			wantAge := int64(42)
			if tc.name == "running" {
				wantAge = 0
			}
			if got := calls[0].msg.EndedAgoMs; got != wantAge {
				t.Errorf("EndedAgoMs = %d, want %d", got, wantAge)
			}
		})
	}
}

func TestPublisher_TerminalSupersedesPendingRunning(t *testing.T) {
	tr := &stubTransport{fail: func(msgID string, _ int) error {
		if msgID == "a1:running" {
			return errTimeout
		}
		return nil
	}}
	p, _ := newPub(tr)

	p.Running(assign("a1"), "", time.Now())
	waitUntil(t, "a running retry", func() bool { return tr.countID("a1:running") >= 3 })
	succeeded(p, "a1")

	if ids := p.PendingAttemptIDs(); len(ids) != 0 {
		t.Errorf("PendingAttemptIDs = %v, want empty", ids)
	}
	stable(t, "running calls", 50*time.Millisecond, func() int { return tr.countID("a1:running") })
}

func TestPublisher_RunTerminalAndShutdownBothSent(t *testing.T) {
	tr := &stubTransport{}
	p, _ := newPub(tr)
	code := 1

	p.Terminal(assign("a1"), "", "failed", &code, "boom", nil, time.Now())
	p.Terminal(assign("a1"), "", "failed", nil, protocol.MessageWorkerShutdown, nil, time.Now())

	if tr.countID("a1:failed") != 1 || tr.countID("a1:shutdown") != 1 {
		t.Errorf("calls = %+v, want one a1:failed and one a1:shutdown", tr.snapshot())
	}
}

func TestPublisher_PermanentErrorDropped(t *testing.T) {
	for _, perm := range []error{
		nats.ErrPermissionViolation,
		fmt.Errorf("wrapped: %w", nats.ErrMaxPayload),
	} {
		t.Run(perm.Error(), func(t *testing.T) {
			tr := &stubTransport{fail: func(string, int) error { return perm }}
			p, h := newPub(tr)

			succeeded(p, "a1")

			if ids := p.PendingAttemptIDs(); len(ids) != 0 {
				t.Errorf("PendingAttemptIDs = %v, want empty", ids)
			}
			stable(t, "calls", 20*time.Millisecond, tr.count)
			if n := tr.count(); n != 1 {
				t.Errorf("calls = %d, want 1", n)
			}
			if n := h.count(slog.LevelError, "a1", ""); n != 1 {
				t.Errorf("Error records = %d, want 1", n)
			}
		})
	}
}

func TestPublisher_FailureLoggingRateLimited(t *testing.T) {
	tr := &stubTransport{fail: func(_ string, try int) error {
		if try <= 20 {
			return errTimeout
		}
		return nil
	}}
	p, h := newPub(tr)

	succeeded(p, "a1")
	waitUntil(t, "nothing pending", func() bool { return len(p.PendingAttemptIDs()) == 0 })

	if n := tr.count(); n != 21 {
		t.Errorf("calls = %d, want 21", n)
	}
	if n := h.count(slog.LevelWarn, "a1", ""); n != 1 {
		t.Errorf("Warn records = %d, want 1", n)
	}
	if n := h.count(slog.LevelInfo, "a1", ""); n != 1 {
		t.Errorf("Info records = %d, want 1", n)
	}
}

// ── Flush ─────────────────────────────────────────────────────────────────────

func TestPublisher_FlushReturnsWhenAcked(t *testing.T) {
	tr := &stubTransport{fail: func(_ string, try int) error {
		if try < 3 {
			return errTimeout
		}
		return nil
	}}
	p, _ := newPub(tr)

	succeeded(p, "a1")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if dropped := p.Flush(ctx); dropped != 0 {
		t.Errorf("Flush dropped %d, want 0", dropped)
	}
	if ctx.Err() != nil {
		t.Error("Flush waited for its whole deadline")
	}
	if n := tr.count(); n != 3 {
		t.Errorf("calls = %d, want 3", n)
	}
}

func TestPublisher_FlushOnNothingPending(t *testing.T) {
	p, _ := newPub(&stubTransport{})
	if dropped := p.Flush(context.Background()); dropped != 0 {
		t.Errorf("Flush dropped %d, want 0", dropped)
	}
}

func TestPublisher_FlushDropsEveryPendingEntry(t *testing.T) {
	const n = 50
	tr := &stubTransport{fail: func(string, int) error { return errTimeout }}
	p, h := newPub(tr)

	for i := range n {
		succeeded(p, fmt.Sprintf("a%02d", i))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if dropped := p.Flush(ctx); dropped != n {
		t.Errorf("Flush dropped %d, want %d", dropped, n)
	}
	if got := h.count(slog.LevelWarn, "", status.MsgDropped); got != n {
		t.Errorf("drop Warn records = %d, want %d", got, n)
	}
	if ids := p.PendingAttemptIDs(); len(ids) != 0 {
		t.Errorf("PendingAttemptIDs = %v, want empty", ids)
	}
	stable(t, "calls after Flush", 30*time.Millisecond, tr.count)
}

func TestPublisher_PendingAttemptIDsSortedAndDeduplicated(t *testing.T) {
	tr := &stubTransport{fail: func(string, int) error { return errTimeout }}
	p, _ := newPub(tr)
	code := 1

	for _, id := range []string{"c", "a", "b"} {
		p.Terminal(assign(id), "", "failed", &code, "boom", nil, time.Now())
	}
	p.Terminal(assign("a"), "", "failed", nil, protocol.MessageWorkerShutdown, nil, time.Now())

	if ids := p.PendingAttemptIDs(); !slices.Equal(ids, []string{"a", "b", "c"}) {
		t.Errorf("PendingAttemptIDs = %v, want [a b c]", ids)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if dropped := p.Flush(ctx); dropped != 4 {
		t.Errorf("Flush dropped %d, want 4", dropped)
	}
}

func TestPublisher_DownBrokerCostsOneAckTimeout(t *testing.T) {
	const ackTimeout = 50 * time.Millisecond
	tr := &stubTransport{fail: func(string, int) error { return errTimeout }}
	blocking := &blockingTransport{inner: tr}
	p := status.New(blocking, status.Config{
		WorkerID: testWorker, AckTimeout: ackTimeout, RetryBase: time.Millisecond, RetryMax: 5 * time.Millisecond,
	}, slog.New(slog.DiscardHandler))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		p.Flush(ctx)
	})

	timed := func(what string, f func()) {
		t.Helper()
		start := time.Now()
		f()
		if d := time.Since(start); d < ackTimeout || d > 4*ackTimeout {
			t.Errorf("%s took %v, want about one ack timeout (%v)", what, d, ackTimeout)
		}
	}
	timed("Running", func() { p.Running(assign("a1"), "", time.Now()) })
	// a1's running is now retrying in the background, one blocked try at a
	// time; superseding it must not wait out that try.
	waitUntil(t, "a running retry", func() bool { return tr.countID("a1:running") >= 2 })
	timed("Terminal", func() { succeeded(p, "a1") })
	if ids := p.PendingAttemptIDs(); !slices.Equal(ids, []string{"a1"}) {
		t.Errorf("PendingAttemptIDs = %v, want [a1]", ids)
	}
}

// blockingTransport waits for each try's context to end, as a publish to an
// unreachable broker does, then reports the inner transport's verdict.
type blockingTransport struct {
	inner *stubTransport
}

func (b *blockingTransport) PublishAcked(ctx context.Context, subj string, data []byte, msgID string) error {
	<-ctx.Done()
	if err := b.inner.PublishAcked(ctx, subj, data, msgID); err != nil {
		return err
	}
	return ctx.Err()
}
