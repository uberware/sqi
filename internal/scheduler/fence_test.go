// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Tests for what the scheduler does once a store write comes back fenced
// (store.ErrFenced: another server has taken the lease, so this one is no
// longer the leader): it stops the work in hand, acknowledges nothing, so
// JetStream redelivers each message to the new leader, and reports the loss
// once through Config.OnFenced.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/store/storetest"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/ws"
)

// fencedLog is the line the first fenced write logs: at WARN when OnFenced
// is set (the callback's owner reports the loss), at ERROR when it is not.
const fencedLog = "scheduler: this server is no longer the leader; stopping"

// lockedBuffer is a bytes.Buffer safe for a logger written from several
// goroutines while the test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fenceRecorder records the OnFenced calls and the log of a scheduler built by
// [newFenceScheduler].
type fenceRecorder struct {
	calls  atomic.Int32
	called chan error
	logs   *lockedBuffer
}

// newFenceScheduler is a metrics scheduler on st whose OnFenced is recorded.
func newFenceScheduler(t *testing.T, st store.Store, farmID string) (*Scheduler, *fenceRecorder) {
	t.Helper()
	rec := &fenceRecorder{called: make(chan error, 8), logs: &lockedBuffer{}}
	cfg := DefaultConfig()
	cfg.FarmID = farmID
	cfg.OnFenced = func(err error) {
		rec.calls.Add(1)
		rec.called <- err
	}
	s := New(cfg, st, &recordBus{}, metrics.New(), slog.New(slog.NewTextHandler(rec.logs, nil)), ws.NoopNotifier{}, nil)
	s.ctx = context.Background()
	return s, rec
}

// assertFencedOnce waits for the OnFenced call and checks that it carried
// store.ErrFenced, that no second call follows, and that the fence was logged
// once, at WARN: the callback's owner logs the loss's one ERROR.
func (r *fenceRecorder) assertFencedOnce(t *testing.T) {
	t.Helper()
	select {
	case err := <-r.called:
		if !errors.Is(err, store.ErrFenced) {
			t.Errorf("OnFenced(%v), want an error wrapping store.ErrFenced", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnFenced was never called")
	}
	time.Sleep(20 * time.Millisecond) // a second call, if one were coming
	if n := r.calls.Load(); n != 1 {
		t.Errorf("OnFenced called %d times, want once", n)
	}
	if n := strings.Count(r.logs.String(), fencedLog); n != 1 {
		t.Errorf("%q logged %d times, want once:\n%s", fencedLog, n, r.logs.String())
	}
	if !strings.Contains(r.logs.String(), "level=WARN msg=\""+fencedLog) || strings.Contains(r.logs.String(), "level=ERROR") {
		t.Errorf("the fenced line is not a lone WARN:\n%s", r.logs.String())
	}
}

// TestFenced_NoCallbackLogsError: with no OnFenced, nothing else reports the
// loss, so the scheduler logs it at ERROR, once.
func TestFenced_NoCallbackLogsError(t *testing.T) {
	logs := &lockedBuffer{}
	s := New(DefaultConfig(), fake.New(), &recordBus{}, metrics.New(), slog.New(slog.NewTextHandler(logs, nil)), ws.NoopNotifier{}, nil)
	ctx := context.Background()
	for range 2 {
		if !s.stopIfFenced(ctx, fmt.Errorf("write: %w", store.ErrFenced)) {
			t.Fatal("stopIfFenced did not recognize store.ErrFenced")
		}
	}
	if n := strings.Count(logs.String(), "level=ERROR msg=\""+fencedLog); n != 1 {
		t.Fatalf("logged %d ERROR fence lines, want 1:\n%s", n, logs.String())
	}
}

// TestWarnUnlessStopped: a store error is logged at WARN with its attributes
// and then the error; nil, the context's cancellation and a fenced write are
// not, and only the fenced one deposes the scheduler.
func TestWarnUnlessStopped(t *testing.T) {
	const msg = "scheduler: test sweep failed"
	tests := []struct {
		name        string
		err         error
		wantWarn    bool
		wantDeposed bool
	}{
		{"nil", nil, false, false},
		{"canceled", fmt.Errorf("sweep: %w", context.Canceled), false, false},
		{"fenced", fmt.Errorf("sweep: %w", store.ErrFenced), false, true},
		{"store error", errors.New("disk full"), true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logs := &lockedBuffer{}
			s := New(DefaultConfig(), fake.New(), &recordBus{}, metrics.New(), slog.New(slog.NewTextHandler(logs, nil)), ws.NoopNotifier{}, nil)
			s.warnUnlessStopped(context.Background(), tc.err, msg, slog.String("task_id", "t1"))
			want := `level=WARN msg="` + msg + `" task_id=t1 error="disk full"`
			if got := strings.Contains(logs.String(), want); got != tc.wantWarn {
				t.Errorf("WARN %q logged = %v, want %v:\n%s", want, got, tc.wantWarn, logs.String())
			}
			if !tc.wantWarn && strings.Contains(logs.String(), msg) {
				t.Errorf("logged %q, want nothing of it:\n%s", msg, logs.String())
			}
			if got := s.deposed(); got != tc.wantDeposed {
				t.Errorf("deposed = %v, want %v", got, tc.wantDeposed)
			}
		})
	}
}

// assertNotFenced checks OnFenced was not called.
func (r *fenceRecorder) assertNotFenced(t *testing.T) {
	t.Helper()
	time.Sleep(20 * time.Millisecond)
	if n := r.calls.Load(); n != 0 {
		t.Errorf("OnFenced called %d times, want none", n)
	}
}

// leaderCtx makes "a" the leader of st at epoch 1 and returns a context
// carrying that epoch, under which writes commit, and depose, which hands the
// lease to "b" at epoch 2 so that every later write made under the context is
// fenced. The lease clock is st's injected one, so no test sleeps.
func leaderCtx(t *testing.T, st *fake.Store) (ctx context.Context, depose func()) {
	t.Helper()
	var now atomic.Int64
	now.Store(1_000_000)
	st.SetClock(now.Load)
	epoch, ok, err := st.AcquireLeadership(t.Context(), "a", 10*time.Second)
	if err != nil || !ok || epoch != 1 {
		t.Fatalf("AcquireLeadership(a) = (%d, %v, %v), want (1, true, nil)", epoch, ok, err)
	}
	depose = func() {
		now.Add(11_000_000) // past a's ten-second lease
		if e, ok, err := st.AcquireLeadership(context.Background(), "b", time.Hour); err != nil || !ok || e != 2 {
			t.Errorf("AcquireLeadership(b) = (%d, %v, %v), want (2, true, nil)", e, ok, err)
		}
	}
	return store.WithLeaderEpoch(t.Context(), epoch), depose
}

// deposedCtx returns a context carrying an epoch st's lease has already moved
// past: every write made under it is fenced.
func deposedCtx(t *testing.T, st *fake.Store) context.Context {
	t.Helper()
	ctx, depose := leaderCtx(t, st)
	depose()
	return ctx
}

// ── consumers ────────────────────────────────────────────────────────────────

func TestTaskStatus_FencedNotAcked(t *testing.T) {
	st := newCheckedFake(t)
	_, taskID, attemptID := seedLogAttempt(t, st)
	task, attempt := mustTaskOf(t, st, taskID), mustAttemptOf(t, st, attemptID)
	s, rec := newFenceScheduler(t, st, "farm-1")
	s.ctx = deposedCtx(t, st)

	first := terminalReport(t, task, attempt, "succeeded", "")
	s.handleTaskStatusMessage(first)
	if first.acked || first.nacked {
		t.Fatalf("fenced report: acked=%v nacked=%v, want neither (left for redelivery to the new leader)",
			first.acked, first.nacked)
	}
	if got := mustTaskOf(t, st, taskID).Status; got != store.TaskStatusRunning {
		t.Errorf("task = %s, want running (the fenced write changed nothing)", got)
	}
	if got := mustAttemptOf(t, st, attemptID); got.EndedAt != nil {
		t.Errorf("attempt closed at %v, want still open", got.EndedAt)
	}

	// A second report, and a malformed one that would be acked away on a
	// leader: once fenced, this server acknowledges nothing.
	second := terminalReport(t, task, attempt, "failed", "boom")
	s.handleTaskStatusMessage(second)
	malformed := &fakeJSMsg{subject: statusTestSubject, data: []byte("{bad")}
	s.handleTaskStatusMessage(malformed)
	for name, m := range map[string]*fakeJSMsg{"second report": second, "malformed": malformed} {
		if m.acked || m.nacked {
			t.Errorf("%s after the fence: acked=%v nacked=%v, want neither", name, m.acked, m.nacked)
		}
	}
	rec.assertFencedOnce(t)
}

func TestWorkerMessage_FencedNotAcked(t *testing.T) {
	const workerID = "w-1"
	cases := []struct {
		name    string
		subject string
		payload any
	}{
		{"register", bus.WorkerRegisterSubject(workerID), protocol.RegisterMsg{
			Version: protocol.ProtocolVersion, Type: protocol.TypeRegister,
			WorkerID: workerID, FarmID: "farm-1", Hostname: "node-1", ComputeLocation: "site-a",
		}},
		{"heartbeat", bus.WorkerHeartbeatSubject(workerID), protocol.HeartbeatMsg{
			Version: protocol.ProtocolVersion, WorkerID: workerID, At: time.Now().UTC(),
		}},
		{"deregister", bus.WorkerDeregisterSubject(workerID), map[string]string{
			"worker_id": workerID, "reason": "shutdown",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newCheckedFake(t)
			seen := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
			if _, _, err := st.RegisterWorker(t.Context(), store.Worker{
				ID: workerID, FarmID: "farm-1", Hostname: "node-0",
				Status: store.WorkerStatusOnline, LastHeartbeatAt: &seen,
			}); err != nil {
				t.Fatalf("RegisterWorker: %v", err)
			}
			s, rec := newFenceScheduler(t, st, "farm-1")
			s.ctx = deposedCtx(t, st)

			msg := &fakeJSMsg{subject: tc.subject, data: workerMsgJSON(t, tc.payload)}
			s.handleWorkerMessage(msg)

			if msg.acked || msg.nacked {
				t.Errorf("acked=%v nacked=%v, want neither (left for redelivery to the new leader)", msg.acked, msg.nacked)
			}
			w, err := st.GetWorker(t.Context(), workerID)
			if err != nil {
				t.Fatalf("GetWorker: %v", err)
			}
			if w.Status != store.WorkerStatusOnline || w.Hostname != "node-0" ||
				w.LastHeartbeatAt == nil || !w.LastHeartbeatAt.Equal(seen) {
				t.Errorf("worker = %s %q heartbeat %v, want online %q heartbeat %v (unchanged)",
					w.Status, w.Hostname, w.LastHeartbeatAt, "node-0", seen)
			}
			if _, err := st.GetComputeLocationByName(t.Context(), "site-a"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("compute location lookup = %v, want not found (nothing written)", err)
			}
			rec.assertFencedOnce(t)
		})
	}
}

func TestTaskLogs_FencedNotAcked(t *testing.T) {
	st := newCheckedFake(t)
	_, taskID, attemptID := seedLogAttempt(t, st)
	s, rec := newFenceScheduler(t, st, "farm-1")
	s.ctx = deposedCtx(t, st)

	chunk := func(seq int64) *fakeJSMsg {
		return &fakeJSMsg{
			subject: bus.TaskLogsSubject(logTestWorkerID, taskID),
			natsSeq: uint64(seq),
			data: msgJSON(t, protocol.LogChunkMsg{
				TaskID: taskID, AttemptID: attemptID, SeqNum: seq,
				At: time.Now().UTC(), Stream: "stdout", Data: fmt.Sprintf("line %d", seq),
			}),
		}
	}
	msgs := []*fakeJSMsg{chunk(1), chunk(2)}
	for _, m := range msgs {
		s.handleLogChunk(m)
	}

	for i, m := range msgs {
		if m.acked || m.nacked {
			t.Errorf("chunk %d: acked=%v nacked=%v, want neither (left for redelivery to the new leader)",
				i+1, m.acked, m.nacked)
		}
	}
	logs, err := st.ListTaskLogs(t.Context(), attemptID, 0, 100)
	if err != nil {
		t.Fatalf("ListTaskLogs: %v", err)
	}
	if len(logs) != 0 {
		t.Errorf("%d log rows stored, want none", len(logs))
	}
	rec.assertFencedOnce(t)
}

// The three tests below fence a best-effort write that follows the message's
// main one: the main write commits, the leadership moves, and the follow-up
// write is fenced. The handler logs and carries on past a failed follow-up
// write, but must still not acknowledge the message.

// deposeAfterRegisterStore hands the lease to another server once
// RegisterWorker has committed.
type deposeAfterRegisterStore struct {
	store.Store

	depose func()
}

func (s *deposeAfterRegisterStore) RegisterWorker(ctx context.Context, w store.Worker) (store.Worker, store.Registration, error) {
	out, reclaimed, err := s.Store.RegisterWorker(ctx, w)
	s.depose()
	return out, reclaimed, err
}

func TestWorkerRegister_FencedFollowUpWriteNotAcked(t *testing.T) {
	st := newCheckedFake(t)
	ctx, depose := leaderCtx(t, st)
	s, rec := newFenceScheduler(t, &deposeAfterRegisterStore{Store: st, depose: depose}, "farm-1")
	s.ctx = ctx

	msg := &fakeJSMsg{subject: bus.WorkerRegisterSubject("w-1"), data: workerMsgJSON(t, protocol.RegisterMsg{
		Version: protocol.ProtocolVersion, Type: protocol.TypeRegister,
		WorkerID: "w-1", FarmID: "farm-1", ComputeLocation: "site-a",
	})}
	s.handleWorkerMessage(msg)

	if msg.acked || msg.nacked {
		t.Errorf("acked=%v nacked=%v, want neither: the compute-location write was fenced", msg.acked, msg.nacked)
	}
	if _, err := st.GetComputeLocationByName(t.Context(), "site-a"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("compute location lookup = %v, want not found", err)
	}
	rec.assertFencedOnce(t)
}

// deposeAfterOfflineStore hands the lease to another server once OfflineWorker
// has committed.
type deposeAfterOfflineStore struct {
	store.Store

	depose func()
}

func (s *deposeAfterOfflineStore) OfflineWorker(ctx context.Context, id, instanceID string, now time.Time) ([]store.Task, bool, error) {
	reclaimed, offlined, err := s.Store.OfflineWorker(ctx, id, instanceID, now)
	s.depose()
	return reclaimed, offlined, err
}

// A preempted ephemeral member's deregister is taken offline, then the
// leadership moves before its row is deleted: the fenced delete must leave the
// message for the new leader, which deletes it on redelivery, and must neither
// revoke nor announce anything here.
func TestWorkerDeregister_FencedEphemeralDeleteNotAcked(t *testing.T) {
	st := newCheckedFake(t)
	if _, err := st.CreateFarm(t.Context(), store.Farm{ID: "farm-1", Name: "farm"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	g := createGroup(t, st, sizedGroup("cloud", func(g *store.CapacityGroup) { g.FarmID = "farm-1" }))
	addMember(t, st, g.ID, store.Worker{ID: "w-1", FarmID: "farm-1", InstanceID: "inst-1"})
	ctx, depose := leaderCtx(t, st)
	s, rec := newFenceScheduler(t, &deposeAfterOfflineStore{Store: st, depose: depose}, "farm-1")
	s.ctx = ctx
	revoked := &revokeRecorder{}
	s.cfg.RevokeWorker = revoked.revoke
	notes := &workerRecordingNotifier{}
	s.notifier = notes

	msg := &fakeJSMsg{subject: bus.WorkerDeregisterSubject("w-1"), data: workerMsgJSON(t, protocol.DeregisterMsg{
		Version: protocol.ProtocolVersion, Type: protocol.TypeDeregister,
		WorkerID: "w-1", InstanceID: "inst-1", Kind: protocol.DeregisterPreempted,
	})}
	s.handleWorkerMessage(msg)
	s.revokes.Wait()

	if msg.acked || msg.nacked {
		t.Errorf("acked=%v nacked=%v, want neither: the ephemeral delete was fenced", msg.acked, msg.nacked)
	}
	if _, err := st.GetWorker(t.Context(), "w-1"); err != nil {
		t.Errorf("GetWorker = %v, want the member kept for the new leader to delete", err)
	}
	if got := revoked.calls(); len(got) != 0 {
		t.Errorf("RevokeWorker calls = %v, want none", got)
	}
	if len(notes.workers) != 0 {
		t.Errorf("worker events = %+v, want none: the handler must stop at the fence", notes.workers)
	}
	rec.assertFencedOnce(t)
}

// deposeAfterStartStore hands the lease to another server once
// StartTaskAttempt has committed.
type deposeAfterStartStore struct {
	store.Store

	depose func()
}

func (s *deposeAfterStartStore) StartTaskAttempt(ctx context.Context, attemptID, taskID, sessionID string, now time.Time) (bool, error) {
	started, err := s.Store.StartTaskAttempt(ctx, attemptID, taskID, sessionID, now)
	s.depose()
	return started, err
}

func TestTaskStatus_FencedJobPromotionNotAcked(t *testing.T) {
	st := newCheckedFake(t)
	g := seedStatusJob(t, st, statusJob{steps: []statusStep{{
		name: "s1", status: store.StepStatusRunning, tasks: []store.TaskStatus{store.TaskStatusAssigned},
	}}})
	task := g.tasks[0][0]
	ctx, depose := leaderCtx(t, st)
	s, rec := newFenceScheduler(t, &deposeAfterStartStore{Store: st, depose: depose}, "farm-1")
	s.ctx = ctx

	msg := runningReport(t, task, g.attempts[task.ID])
	s.handleTaskStatusMessage(msg)

	if msg.acked || msg.nacked {
		t.Errorf("acked=%v nacked=%v, want neither: the job promotion was fenced", msg.acked, msg.nacked)
	}
	rec.assertFencedOnce(t)
}

// deposeAfterFinalizeJobStore hands the lease to another server once
// FinalizeJob has committed.
type deposeAfterFinalizeJobStore struct {
	store.Store

	depose func()
}

func (s *deposeAfterFinalizeJobStore) FinalizeJob(ctx context.Context, id string, now time.Time) (store.JobStatus, bool, error) {
	status, changed, err := s.Store.FinalizeJob(ctx, id, now)
	s.depose()
	return status, changed, err
}

// TestTaskStatus_FencedDependentReleaseNotAcked fences the release of a job
// blocked on the one a report just completed. Releasing dependents is
// best-effort on a leader (the blocked-job sweep is its backstop), but a
// fenced release must still leave the report unacknowledged.
func TestTaskStatus_FencedDependentReleaseNotAcked(t *testing.T) {
	st := newCheckedFake(t)
	job, taskID, attemptID := seedLogAttempt(t, st)
	blocked := storetest.Submit(t, st, store.JobSubmission{
		Job: store.Job{
			ID: "job-blocked", FarmID: job.FarmID, QueueID: job.QueueID, Name: "down",
			Status: store.JobStatusBlocked, TemplateFormat: store.TemplateFormatJSON,
		},
		DependsOn: []string{job.ID},
		Steps:     []store.Step{{ID: "step-blocked", JobID: "job-blocked", Name: "s1", Status: store.StepStatusPending}},
		Tasks: []store.Task{{
			ID: "task-blocked", JobID: "job-blocked", StepID: "step-blocked", Name: "t1", Status: store.TaskStatusPending,
		}},
	}).Job
	ctx, depose := leaderCtx(t, st)
	s, rec := newFenceScheduler(t, &deposeAfterFinalizeJobStore{Store: st, depose: depose}, "farm-1")
	s.ctx = ctx

	msg := terminalReport(t, mustTaskOf(t, st, taskID), mustAttemptOf(t, st, attemptID), "succeeded", "")
	s.handleTaskStatusMessage(msg)

	if msg.acked || msg.nacked {
		t.Errorf("acked=%v nacked=%v, want neither: the dependent's release was fenced", msg.acked, msg.nacked)
	}
	if got := mustJob(t, st, job.ID).Status; got != store.JobStatusCompleted {
		t.Fatalf("fixture: upstream job = %s, want completed before the fence", got)
	}
	if got := mustJob(t, st, blocked.ID).Status; got != store.JobStatusBlocked {
		t.Errorf("dependent job = %s, want blocked (its release was fenced)", got)
	}
	rec.assertFencedOnce(t)
}

// ── lease handler ────────────────────────────────────────────────────────────

// leaseReplyOf runs handler on its own goroutine and returns the reply's
// assignments, failing if it does not answer within limit.
func leaseReplyOf(t *testing.T, limit time.Duration, handler func() []byte) []protocol.AssignMsg {
	t.Helper()
	done := make(chan []byte, 1)
	go func() { done <- handler() }()
	var raw []byte
	select {
	case raw = <-done:
	case <-time.After(limit):
		t.Fatalf("the lease request was not answered within %v", limit)
	}
	var reply leaseReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatalf("unmarshal reply %s: %v", raw, err)
	}
	out := make([]protocol.AssignMsg, 0, len(reply.Assignments))
	for _, a := range reply.Assignments {
		var m protocol.AssignMsg
		if err := json.Unmarshal(a, &m); err != nil {
			t.Fatalf("unmarshal assignment %s: %v", a, err)
		}
		out = append(out, m)
	}
	return out
}

// leaseRequestBody marshals w's lease request.
func leaseRequestBody(t *testing.T, workerID string) []byte {
	t.Helper()
	b, err := json.Marshal(leaseRequest{WorkerID: workerID})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return b
}

// deposeAfterLeaseStore hands the lease to another server right after its
// first committed LeaseTask, so the second lease of the same batch is fenced.
type deposeAfterLeaseStore struct {
	store.Store

	depose func()
	leases atomic.Int32
}

func (s *deposeAfterLeaseStore) LeaseTask(ctx context.Context, req store.LeaseRequest) (store.LeaseResult, error) {
	res, err := s.Store.LeaseTask(ctx, req)
	if s.leases.Add(1) == 1 {
		s.depose()
	}
	return res, err
}

func TestLease_FencedRepliesEmpty(t *testing.T) {
	st := newCheckedFake(t)
	one := 1
	w, taskIDs := seedLeaseFixture(t, st, []*int{&one, &one})
	s, rec := newFenceScheduler(t, st, "f1")
	s.ctx = deposedCtx(t, st)

	// A request that leases nothing would park for leaseHoldTimeout (30 s); a
	// fenced one must not.
	got := leaseReplyOf(t, 5*time.Second, func() []byte {
		return s.handleLeaseRequest(w.ID, bus.WildcardQueueToken, leaseRequestBody(t, w.ID))
	})

	if len(got) != 0 {
		t.Errorf("fenced lease replied %d assignments, want none", len(got))
	}
	for _, id := range taskIDs {
		if status := mustTaskOf(t, st, id).Status; status != store.TaskStatusReady {
			t.Errorf("task %s = %s, want ready (nothing leased)", id, status)
		}
	}
	rec.assertFencedOnce(t)
}

// TestLease_FencedMidBatchRepliesCommitted pins that a lease fenced part way
// through a batch still delivers the tasks leased before it: they are
// committed as assigned, real leases a worker must run, and the batch stops
// there.
func TestLease_FencedMidBatchRepliesCommitted(t *testing.T) {
	st := newCheckedFake(t)
	one := 1
	w, taskIDs := seedLeaseFixture(t, st, []*int{&one, &one, &one})
	ctx, depose := leaderCtx(t, st)
	spy := &deposeAfterLeaseStore{Store: st, depose: depose}
	s, rec := newFenceScheduler(t, spy, "f1")
	s.ctx = ctx

	got := leaseReplyOf(t, 5*time.Second, func() []byte {
		return s.handleLeaseRequest(w.ID, bus.WildcardQueueToken, leaseRequestBody(t, w.ID))
	})

	if len(got) != 1 {
		t.Fatalf("replied %d assignments, want exactly the one committed before the fence", len(got))
	}
	if n := spy.leases.Load(); n != 2 {
		t.Errorf("LeaseTask called %d times, want 2 (the batch stops at the fenced one)", n)
	}
	for _, id := range taskIDs {
		want := store.TaskStatusReady
		if id == got[0].TaskID {
			want = store.TaskStatusAssigned
		}
		if status := mustTaskOf(t, st, id).Status; status != want {
			t.Errorf("task %s = %s, want %s", id, status, want)
		}
	}
	rec.assertFencedOnce(t)
}

// TestLease_FencedAfterParkRepliesEmpty drives the one retry a parked request
// makes: the work that woke it is leased under a deposed epoch, so the reply
// is empty and the loss is reported.
func TestLease_FencedAfterParkRepliesEmpty(t *testing.T) {
	st := newCheckedFake(t)
	w, _ := seedLeaseFixture(t, st, nil) // no ready task: the request parks
	ctx, depose := leaderCtx(t, st)
	s, rec := newFenceScheduler(t, st, "f1")
	s.ctx = ctx

	done := make(chan []protocol.AssignMsg, 1)
	go func() {
		done <- leaseReplyOf(t, 10*time.Second, func() []byte {
			return s.handleLeaseRequest(w.ID, bus.WildcardQueueToken, leaseRequestBody(t, w.ID))
		})
	}()
	waitParked(t, s, bus.WildcardQueueToken)

	one := 1
	seedLeaseTaskInQueue(t, st, &one)
	depose()
	s.WakeQueue("q1")

	select {
	case got := <-done:
		if len(got) != 0 {
			t.Errorf("replied %d assignments, want none (the lease was fenced)", len(got))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the parked request was not answered")
	}
	rec.assertFencedOnce(t)
}

// TestLease_RefusedOnceFenced pins that a server that has been fenced leases
// nothing more: a later request is answered empty without a lease attempt.
func TestLease_RefusedOnceFenced(t *testing.T) {
	st := newCheckedFake(t)
	one := 1
	w, _ := seedLeaseFixture(t, st, []*int{&one})
	ctx, depose := leaderCtx(t, st)
	spy := &deposeAfterLeaseStore{Store: st, depose: func() {}}
	s, rec := newFenceScheduler(t, spy, "f1")
	s.ctx = ctx
	s.leaseRefusalDelay = 10 * time.Millisecond

	depose()
	s.fenced(ctx, fmt.Errorf("elsewhere: %w", store.ErrFenced))
	rec.assertFencedOnce(t)

	got := leaseReplyOf(t, 5*time.Second, func() []byte {
		return s.handleLeaseRequest(w.ID, bus.WildcardQueueToken, leaseRequestBody(t, w.ID))
	})
	if len(got) != 0 {
		t.Errorf("replied %d assignments, want none", len(got))
	}
	if n := spy.leases.Load(); n != 0 {
		t.Errorf("LeaseTask called %d times after the fence, want none", n)
	}
}

// waitParked waits until a lease request is parked on queueID.
func waitParked(t *testing.T, s *Scheduler, queueID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.waiters.mu.Lock()
		parked := len(s.waiters.waiters[queueID])
		s.waiters.mu.Unlock()
		if parked > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the lease request never parked")
		}
		time.Sleep(time.Millisecond)
	}
}

// seedLeaseTaskInQueue submits one more running job to seedLeaseFixture's
// farm f1 and queue q1, with one ready task needing cores.
func seedLeaseTaskInQueue(t *testing.T, st store.Store, cores *int) string {
	t.Helper()
	sub := store.JobSubmission{Job: store.Job{
		ID: "job-late", FarmID: "f1", QueueID: "q1", Name: "late",
		Status: store.JobStatusRunning, TemplateFormat: store.TemplateFormatJSON,
		RawTemplate: minimalRenderJSON,
	}}
	sub.Steps = []store.Step{{ID: "step-late", JobID: sub.Job.ID, Name: "render", Status: store.StepStatusReady}}
	sub.Tasks = []store.Task{{
		ID: "task-late", JobID: sub.Job.ID, StepID: "step-late", Name: "t",
		Status: store.TaskStatusReady, Parameters: map[string]string{}, RequiredCores: cores,
	}}
	storetest.Submit(t, st, sub)
	return "task-late"
}

// ── sweeps ───────────────────────────────────────────────────────────────────

// sweepEntries are the first store call of each part of a sweep tick, in the
// order sweepOnce makes them: the stale-worker sweep, the two retention
// sweeps, the assigned-task reap, attempt reconciliation, the stalled-job
// demotion, the unschedulable sweep, the blocked-job sweep and the three
// gauges.
var sweepEntries = []string{
	"ListStaleWorkers", "DeleteOfflineWorkersBefore", "DeleteTerminalJobsBefore",
	"ReclaimStaleAssignedTasks", "ListOpenAttempts", "DemoteStalledJobs", "ListReadyTasks",
	"ListBlockedJobs", "CountReadyTasksByQueue", "CountIdleWorkers", "ListUsagePools",
}

// sweepSpy records the sweep entries a tick reaches and, for the one named
// failAt, returns a fenced error in place of the call.
type sweepSpy struct {
	store.Store

	failAt string

	mu    sync.Mutex
	calls []string
}

func (s *sweepSpy) enter(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, name)
	if name == s.failAt {
		return fmt.Errorf("store: %s: %w", name, store.ErrFenced)
	}
	return nil
}

func (s *sweepSpy) called() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

func (s *sweepSpy) ListStaleWorkers(ctx context.Context, before time.Time) ([]store.Worker, error) {
	if err := s.enter("ListStaleWorkers"); err != nil {
		return nil, err
	}
	return s.Store.ListStaleWorkers(ctx, before)
}

func (s *sweepSpy) DeleteOfflineWorkersBefore(ctx context.Context, before time.Time) ([]store.Worker, error) {
	if err := s.enter("DeleteOfflineWorkersBefore"); err != nil {
		return nil, err
	}
	return s.Store.DeleteOfflineWorkersBefore(ctx, before)
}

func (s *sweepSpy) DeleteTerminalJobsBefore(ctx context.Context, before time.Time, includeFailed bool) ([]store.DeletedJob, error) {
	if err := s.enter("DeleteTerminalJobsBefore"); err != nil {
		return nil, err
	}
	return s.Store.DeleteTerminalJobsBefore(ctx, before, includeFailed)
}

func (s *sweepSpy) ReclaimStaleAssignedTasks(ctx context.Context, cutoff time.Time) ([]store.Task, error) {
	if err := s.enter("ReclaimStaleAssignedTasks"); err != nil {
		return nil, err
	}
	return s.Store.ReclaimStaleAssignedTasks(ctx, cutoff)
}

func (s *sweepSpy) ListOpenAttempts(ctx context.Context) ([]store.OpenAttempt, error) {
	if err := s.enter("ListOpenAttempts"); err != nil {
		return nil, err
	}
	return s.Store.ListOpenAttempts(ctx)
}

func (s *sweepSpy) DemoteStalledJobs(ctx context.Context, now time.Time) ([]string, error) {
	if err := s.enter("DemoteStalledJobs"); err != nil {
		return nil, err
	}
	return s.Store.DemoteStalledJobs(ctx, now)
}

func (s *sweepSpy) ListReadyTasks(ctx context.Context, farmID string, now time.Time, limit int) ([]store.Task, error) {
	if err := s.enter("ListReadyTasks"); err != nil {
		return nil, err
	}
	return s.Store.ListReadyTasks(ctx, farmID, now, limit)
}

func (s *sweepSpy) ListBlockedJobs(ctx context.Context) ([]store.Job, error) {
	if err := s.enter("ListBlockedJobs"); err != nil {
		return nil, err
	}
	return s.Store.ListBlockedJobs(ctx)
}

func (s *sweepSpy) CountReadyTasksByQueue(ctx context.Context, farmID string, now time.Time) (map[string]int, error) {
	if err := s.enter("CountReadyTasksByQueue"); err != nil {
		return nil, err
	}
	return s.Store.CountReadyTasksByQueue(ctx, farmID, now)
}

func (s *sweepSpy) CountIdleWorkers(ctx context.Context, farmID string) (int, error) {
	if err := s.enter("CountIdleWorkers"); err != nil {
		return 0, err
	}
	return s.Store.CountIdleWorkers(ctx, farmID)
}

func (s *sweepSpy) ListUsagePools(ctx context.Context) ([]store.UsagePool, error) {
	if err := s.enter("ListUsagePools"); err != nil {
		return nil, err
	}
	return s.Store.ListUsagePools(ctx)
}

// TestSweep_FencedStopsTick drives a tick whose first write, taking a stale
// worker offline, is fenced by the real fake: nothing else in that tick runs,
// and no later tick does either.
func TestSweep_FencedStopsTick(t *testing.T) {
	st := newCheckedFake(t)
	workerID, taskID, _ := seedStaleWorkerWithTask(t, st, time.Hour)
	spy := &sweepSpy{Store: st}
	s, rec := newFenceScheduler(t, spy, "farm-1")
	ctx := deposedCtx(t, st)

	s.sweepOnce(ctx)

	if got := spy.called(); !slices.Equal(got, []string{"ListStaleWorkers"}) {
		t.Errorf("sweep entries reached = %v, want only ListStaleWorkers (the tick stops at the fenced write)", got)
	}
	if got := workerStatus(t, st, workerID); got != store.WorkerStatusOnline {
		t.Errorf("worker = %s, want online (the fenced write changed nothing)", got)
	}
	if got := taskStatus(t, st, taskID); got != store.TaskStatusRunning {
		t.Errorf("task = %s, want running", got)
	}
	rec.assertFencedOnce(t)

	s.sweepOnce(ctx)
	if got := spy.called(); len(got) != 1 {
		t.Errorf("a tick after the fence reached %v, want nothing", got[1:])
	}
}

// TestSweep_FencedAtEachStageStopsTick fences each sweep whose first store
// call is a write, and the blocked-job sweep, whose fenced error reaches
// sweepOnce as its return value (TestSweepBlockedJobs_FencedStops), one at a
// time: the tick reaches that call and nothing after it. The unfenced row
// pins the order sweepEntries assumes.
func TestSweep_FencedAtEachStageStopsTick(t *testing.T) {
	for _, failAt := range []string{
		"", "DeleteOfflineWorkersBefore", "DeleteTerminalJobsBefore", "ReclaimStaleAssignedTasks",
		"DemoteStalledJobs", "ListBlockedJobs",
	} {
		name := failAt
		if name == "" {
			name = "unfenced"
		}
		t.Run(name, func(t *testing.T) {
			spy := &sweepSpy{Store: newCheckedFake(t), failAt: failAt}
			s, rec := newFenceScheduler(t, spy, "farm-1")

			s.sweepOnce(t.Context())

			want := sweepEntries
			if failAt != "" {
				want = sweepEntries[:slices.Index(sweepEntries, failAt)+1]
			}
			if got := spy.called(); !slices.Equal(got, want) {
				t.Errorf("sweep entries reached = %v, want %v", got, want)
			}
			if failAt == "" {
				rec.assertNotFenced(t)
				return
			}
			rec.assertFencedOnce(t)
		})
	}
}

// TestSweepBlockedJobs_FencedStops fences the blocked-job sweep's release of
// a satisfied job: the sweep stops there, leaving the second blocked job
// unvisited, and returns the fenced error for sweepOnce to act on.
func TestSweepBlockedJobs_FencedStops(t *testing.T) {
	h := newJobDepsHarness(t)
	up := h.seedJob(t, store.JobStatusCompleted)
	first := h.seedBlockedJobDependingOn(t, up.ID)
	second := h.seedBlockedJobDependingOn(t, up.ID)
	spy := &releaseCountingStore{sweepSpy: sweepSpy{Store: h.store}}
	s, rec := newFenceScheduler(t, spy, h.farmID)

	err := s.sweepBlockedJobs(deposedCtx(t, h.store))

	if !errors.Is(err, store.ErrFenced) {
		t.Errorf("sweepBlockedJobs = %v, want store.ErrFenced", err)
	}
	if n := spy.releases.Load(); n != 1 {
		t.Errorf("ReleaseBlockedJob called %d times, want 1 (the sweep stops at the fenced release)", n)
	}
	for _, id := range []string{first.ID, second.ID} {
		if got := mustJob(t, h.store, id).Status; got != store.JobStatusBlocked {
			t.Errorf("job %s = %s, want blocked", id, got)
		}
	}
	rec.assertNotFenced(t) // reported by sweepOnce, which the stage table covers
}

// releaseCountingStore is a sweepSpy that also counts ReleaseBlockedJob.
type releaseCountingStore struct {
	sweepSpy

	releases atomic.Int32
}

func (s *releaseCountingStore) ReleaseBlockedJob(ctx context.Context, id string, now time.Time) (bool, error) {
	s.releases.Add(1)
	return s.Store.ReleaseBlockedJob(ctx, id, now)
}

// reclaimCountingStore counts ReclaimTaskAttempt.
type reclaimCountingStore struct {
	store.Store

	reclaims atomic.Int32
}

func (s *reclaimCountingStore) ReclaimTaskAttempt(ctx context.Context, attemptID, taskID string, now time.Time) (bool, error) {
	s.reclaims.Add(1)
	return s.Store.ReclaimTaskAttempt(ctx, attemptID, taskID, now)
}

func TestReconcileAttempts_FencedStops(t *testing.T) {
	st := newCheckedFake(t)
	ctx := t.Context()
	if _, err := st.CreateFarm(ctx, store.Farm{ID: "f1", Name: "f1"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "q1", FarmID: "f1", Name: "q1"}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	now := time.Now().UTC()
	if _, _, err := st.RegisterWorker(ctx, store.Worker{
		ID: reconcileWorker, FarmID: "f1", Hostname: reconcileWorker,
		Status: store.WorkerStatusOnline, CPUCount: 4, LastHeartbeatAt: &now,
	}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	_, attempts := storetest.NewJob("j1", "f1", "q1").
		Step("s1", store.StepStatusReady).
		Task("t1", "s1", store.TaskStatusRunning).
		Task("t2", "s1", store.TaskStatusRunning).
		SubmitLeasing(t, st, reconcileWorker)
	spy := &reclaimCountingStore{Store: st}
	s, rec := newFenceScheduler(t, spy, "f1")
	created := attempts["t1"].CreatedAt
	armReconciler(s, created)
	at := created.Add(2 * time.Minute)
	s.reconciler.record(reconcileWorker, at, at, nil) // the worker reports holding nothing

	s.reconcileAttempts(deposedCtx(t, st), at)

	if n := spy.reclaims.Load(); n != 1 {
		t.Errorf("ReclaimTaskAttempt called %d times, want 1 (reconciliation stops at the fenced reclaim)", n)
	}
	for _, id := range []string{"t1", "t2"} {
		if got := mustTaskOf(t, st, id).Status; got != store.TaskStatusRunning {
			t.Errorf("task %s = %s, want running", id, got)
		}
	}
	if got := reconciledTotal(t, s); got != 0 {
		t.Errorf("sqi_scheduler_tasks_reconciled_total = %v, want 0", got)
	}
	rec.assertFencedOnce(t)
}

// reasonCountingStore counts SetTaskUnschedulableReason.
type reasonCountingStore struct {
	store.Store

	writes atomic.Int32
}

func (s *reasonCountingStore) SetTaskUnschedulableReason(ctx context.Context, taskID, reason string) (bool, error) {
	s.writes.Add(1)
	return s.Store.SetTaskUnschedulableReason(ctx, taskID, reason)
}

func TestSweepUnschedulable_FencedStops(t *testing.T) {
	st := newCheckedFake(t)
	one := 1
	w, taskIDs := seedLeaseFixture(t, st, []*int{&one, &one})
	if _, _, err := st.OfflineWorker(t.Context(), w.ID, "", time.Now().UTC()); err != nil {
		t.Fatalf("OfflineWorker: %v", err) // no online worker: both tasks are unschedulable
	}
	spy := &reasonCountingStore{Store: st}
	s, rec := newFenceScheduler(t, spy, "f1")
	s.cfg.UnschedulableGrace = time.Nanosecond
	time.Sleep(time.Millisecond) // both tasks past the grace

	s.sweepUnschedulable(deposedCtx(t, st))

	if n := spy.writes.Load(); n != 1 {
		t.Errorf("SetTaskUnschedulableReason called %d times, want 1 (the sweep stops at the fenced write)", n)
	}
	for _, id := range taskIDs {
		if got := mustTaskOf(t, st, id).UnschedulableReason; got != "" {
			t.Errorf("task %s reason = %q, want none written", id, got)
		}
	}
	rec.assertFencedOnce(t)
}

// ── start-up repairs ─────────────────────────────────────────────────────────

// repairCountingStore counts the first write of each start-up repair.
type repairCountingStore struct {
	store.Store

	finalizeStep atomic.Int32
	releaseStep  atomic.Int32
}

func (s *repairCountingStore) FinalizeStep(ctx context.Context, id string, now time.Time) (store.StepStatus, bool, error) {
	s.finalizeStep.Add(1)
	return s.Store.FinalizeStep(ctx, id, now)
}

func (s *repairCountingStore) ReleaseStep(ctx context.Context, id string, now time.Time) (bool, []store.Task, error) {
	s.releaseStep.Add(1)
	return s.Store.ReleaseStep(ctx, id, now)
}

func TestReconcileStuckSteps_FencedStops(t *testing.T) {
	st := newCheckedFake(t)
	stuck := statusJob{steps: []statusStep{{
		name: "Step1", status: store.StepStatusRunning, tasks: []store.TaskStatus{store.TaskStatusSucceeded},
	}}}
	a, b := seedStatusJob(t, st, stuck), seedStatusJob(t, st, stuck)
	spy := &repairCountingStore{Store: st}
	s, rec := newFenceScheduler(t, spy, "farm-1")

	s.reconcileStuckSteps(deposedCtx(t, st))

	if n := spy.finalizeStep.Load(); n != 1 {
		t.Errorf("FinalizeStep called %d times, want 1 (the repair stops at the fenced write)", n)
	}
	for _, g := range []statusGraph{a, b} {
		if got := mustStep(t, st, g.steps[0].ID).Status; got != store.StepStatusRunning {
			t.Errorf("step %s = %s, want running (unrepaired)", g.steps[0].ID, got)
		}
	}
	rec.assertFencedOnce(t)
}

func TestReconcilePendingSteps_FencedStops(t *testing.T) {
	st := newCheckedFake(t)
	stranded := statusJob{steps: []statusStep{{
		name: "Step1", status: store.StepStatusPending, tasks: []store.TaskStatus{store.TaskStatusPending},
	}}}
	a, b := seedStatusJob(t, st, stranded), seedStatusJob(t, st, stranded)
	spy := &repairCountingStore{Store: st}
	s, rec := newFenceScheduler(t, spy, "farm-1")

	s.reconcilePendingSteps(deposedCtx(t, st))

	if n := spy.releaseStep.Load(); n != 1 {
		t.Errorf("ReleaseStep called %d times, want 1 (the repair stops at the fenced write)", n)
	}
	for _, g := range []statusGraph{a, b} {
		if got := mustStep(t, st, g.steps[0].ID).Status; got != store.StepStatusPending {
			t.Errorf("step %s = %s, want pending (unreleased)", g.steps[0].ID, got)
		}
	}
	rec.assertFencedOnce(t)
}

// ── OnFenced ─────────────────────────────────────────────────────────────────

// TestFenced_OnFencedMayStopTheScheduler pins that the callback runs off the
// goroutine that hit the fence, so a callback that stops the scheduler and
// waits for Run to return cannot deadlock against the sweep goroutine Run is
// waiting for.
func TestFenced_OnFencedMayStopTheScheduler(t *testing.T) {
	st := newCheckedFake(t)
	seedStaleWorkerWithTask(t, st, time.Hour)
	cfg := DefaultConfig()
	cfg.FarmID = "farm-1"
	cfg.HeartbeatSweepInterval = time.Millisecond
	runReturned, callbackDone := make(chan struct{}), make(chan struct{})
	var s *Scheduler
	cfg.OnFenced = func(error) {
		s.Stop()
		<-runReturned // Run waits for the sweep goroutine that hit the fence
		close(callbackDone)
	}
	s = New(cfg, st, &recordBus{}, metrics.New(), slog.New(slog.DiscardHandler), ws.NoopNotifier{}, nil)
	ctx := deposedCtx(t, st)

	go func() {
		if err := s.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
		close(runReturned)
	}()

	select {
	case <-callbackDone:
	case <-time.After(5 * time.Second):
		t.Fatal("OnFenced stopping the scheduler did not let Run return")
	}
}

func TestFenced_NilOnFencedOnlyLogs(t *testing.T) {
	logs := &lockedBuffer{}
	s := New(DefaultConfig(), newCheckedFake(t), &recordBus{}, metrics.New(),
		slog.New(slog.NewTextHandler(logs, nil)), ws.NoopNotifier{}, nil)

	s.fenced(t.Context(), store.ErrFenced)
	s.fenced(t.Context(), store.ErrFenced)

	if n := strings.Count(logs.String(), fencedLog); n != 1 {
		t.Errorf("%q logged %d times, want once:\n%s", fencedLog, n, logs.String())
	}
}

// TestFenced_OtherErrorsAreNotAFence pins that only store.ErrFenced counts:
// any other store error is handled as an ordinary failure.
func TestFenced_OtherErrorsAreNotAFence(t *testing.T) {
	st := &registerErrSt{Store: newCheckedFake(t)}
	s, rec := newFenceScheduler(t, st, "")

	msg := &fakeJSMsg{
		subject: bus.WorkerRegisterSubject("w-1"),
		data:    workerMsgJSON(t, protocol.RegisterMsg{Version: protocol.ProtocolVersion, WorkerID: "w-1", FarmID: "farm-1"}),
	}
	s.handleWorkerMessage(msg)

	if !msg.nacked {
		t.Error("an ordinary store error must nak for redelivery")
	}
	if s.stopIfFenced(t.Context(), store.ErrNotFound) || s.stopIfFenced(t.Context(), nil) {
		t.Error("stopIfFenced is true for an error that is not store.ErrFenced")
	}
	rec.assertNotFenced(t)
}
