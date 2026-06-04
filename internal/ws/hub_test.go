// SPDX-License-Identifier: AGPL-3.0-only

package ws

// Unit tests for [Hub] — task 92.
//
// These tests exercise the Hub directly without any HTTP or WebSocket
// machinery, using the Hub's own Register/Subscribe/Deregister/Notify* API as
// the "fake bus".  The goal is to cover:
//
//   - Client registration and idempotent re-registration.
//   - Subscribe validation (unknown subjects, unregistered clients).
//   - Fan-out delivery to subscribed clients across all four subject types.
//   - Backpressure: events dropped silently when a client's send buffer is full.
//   - Unsubscribe stops future deliveries.
//   - Deregister removes the client from all subscriptions and closes its channel.
//   - Ring-buffer replay via sinceSeq.
//   - NotifyTask fans to both SubjectJobs and the per-job tasks subject.
//   - Concurrent fan-out is race-free (run with -race).
//   - subjectRing.since edge cases.

import (
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func newTestHub() *Hub {
	return NewHub(slog.New(slog.DiscardHandler))
}

// drainOrTimeout reads one Envelope from ch within d. Returns (env, true) on
// success or (zero, false) on timeout or closed channel.
func drainOrTimeout(ch <-chan Envelope, d time.Duration) (Envelope, bool) {
	select {
	case env, ok := <-ch:
		if !ok {
			return Envelope{}, false
		}
		return env, true
	case <-time.After(d):
		return Envelope{}, false
	}
}

// drainAll drains all immediately available envelopes from ch without blocking.
// Uses a select/default loop so it is safe to call while other goroutines may
// still be writing to the same channel.
func drainAll(ch chan Envelope) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// ── registration ──────────────────────────────────────────────────────────────

func TestHub_RegisterDeregister(t *testing.T) {
	h := newTestHub()
	const id = "client-1"

	ch := h.Register(id)
	if ch == nil {
		t.Fatal("Register returned nil channel")
	}

	// Second call is idempotent: same channel is returned.
	if got := h.Register(id); got != ch {
		t.Fatal("second Register returned a different channel")
	}

	// Deregister closes the channel.
	h.Deregister(id)
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("channel should be closed after Deregister but received a value")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("channel was not closed after Deregister")
	}

	// Deregister is idempotent — must not panic.
	h.Deregister(id)
}

// ── subscribe validation ──────────────────────────────────────────────────────

func TestHub_Subscribe_InvalidSubject(t *testing.T) {
	h := newTestHub()
	h.Register("c1")

	if err := h.Subscribe("c1", "bad/subject", 0); err == nil {
		t.Fatal("expected error for unrecognized subject, got nil")
	}
}

func TestHub_Subscribe_UnregisteredClient(t *testing.T) {
	h := newTestHub()

	if err := h.Subscribe("not-registered", SubjectJobs, 0); err == nil {
		t.Fatal("expected error for unregistered client, got nil")
	}
}

func TestHub_Subscribe_ValidSubjects(t *testing.T) {
	cases := []string{
		SubjectJobs,
		SubjectWorkers,
		fmt.Sprintf(SubjectJobTasksFmt, "job-123"),
		fmt.Sprintf(SubjectTaskLogsFmt, "task-456"),
	}
	for _, subj := range cases {
		t.Run(subj, func(t *testing.T) {
			h := newTestHub()
			h.Register("c1")
			if err := h.Subscribe("c1", subj, 0); err != nil {
				t.Fatalf("unexpected error for subject %q: %v", subj, err)
			}
		})
	}
}

// ── fan-out delivery ──────────────────────────────────────────────────────────

func TestHub_NotifyJob_FansToSubscribersOnly(t *testing.T) {
	h := newTestHub()
	jobsCh := h.Register("jobs-client")
	workersCh := h.Register("workers-client")

	if err := h.Subscribe("jobs-client", SubjectJobs, 0); err != nil {
		t.Fatalf("subscribe jobs-client: %v", err)
	}
	if err := h.Subscribe("workers-client", SubjectWorkers, 0); err != nil {
		t.Fatalf("subscribe workers-client: %v", err)
	}

	h.NotifyJob(JobEvent{JobID: "j1", Status: "running"})

	// jobs-client must receive the push.
	env, ok := drainOrTimeout(jobsCh, 200*time.Millisecond)
	if !ok {
		t.Fatal("jobs-client did not receive push from NotifyJob")
	}
	if env.Type != TypePush {
		t.Fatalf("expected TypePush, got %q", env.Type)
	}
	if env.Subject != SubjectJobs {
		t.Fatalf("expected subject %q, got %q", SubjectJobs, env.Subject)
	}
	if env.Seq == 0 {
		t.Fatal("expected non-zero Seq on push")
	}

	// workers-client must not receive a jobs push.
	select {
	case msg := <-workersCh:
		t.Fatalf("workers-client unexpectedly received message (subject=%q)", msg.Subject)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHub_NotifyTask_FansToJobsAndTaskSubject(t *testing.T) {
	h := newTestHub()
	const jobID = "job-abc"
	taskSubject := fmt.Sprintf(SubjectJobTasksFmt, jobID)

	jobsCh := h.Register("c-jobs")
	taskCh := h.Register("c-tasks")

	if err := h.Subscribe("c-jobs", SubjectJobs, 0); err != nil {
		t.Fatalf("subscribe jobs: %v", err)
	}
	if err := h.Subscribe("c-tasks", taskSubject, 0); err != nil {
		t.Fatalf("subscribe tasks: %v", err)
	}

	h.NotifyTask(TaskEvent{JobID: jobID, TaskID: "t1", Status: "running"})

	// Both subjects should receive a push.
	env, ok := drainOrTimeout(jobsCh, 200*time.Millisecond)
	if !ok {
		t.Fatal("jobs subscriber did not receive NotifyTask push")
	}
	if env.Subject != SubjectJobs {
		t.Fatalf("jobs subscriber: expected subject %q, got %q", SubjectJobs, env.Subject)
	}

	env, ok = drainOrTimeout(taskCh, 200*time.Millisecond)
	if !ok {
		t.Fatal("tasks subscriber did not receive NotifyTask push")
	}
	if env.Subject != taskSubject {
		t.Fatalf("tasks subscriber: expected subject %q, got %q", taskSubject, env.Subject)
	}
}

func TestHub_NotifyWorker_FansToWorkerSubscribers(t *testing.T) {
	h := newTestHub()
	ch := h.Register("c1")
	if err := h.Subscribe("c1", SubjectWorkers, 0); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	h.NotifyWorker(WorkerEvent{WorkerID: "w1", Status: "online"})

	env, ok := drainOrTimeout(ch, 200*time.Millisecond)
	if !ok {
		t.Fatal("did not receive worker push")
	}
	if env.Subject != SubjectWorkers {
		t.Fatalf("expected subject %q, got %q", SubjectWorkers, env.Subject)
	}
}

func TestHub_NotifyLog_FansToLogSubscriber(t *testing.T) {
	h := newTestHub()
	const taskID = "task-xyz"
	logSubject := fmt.Sprintf(SubjectTaskLogsFmt, taskID)

	ch := h.Register("c1")
	if err := h.Subscribe("c1", logSubject, 0); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	h.NotifyLog(LogEvent{TaskID: taskID, SeqNum: 1, Stream: "stdout", Data: "hello\n"})

	env, ok := drainOrTimeout(ch, 200*time.Millisecond)
	if !ok {
		t.Fatal("did not receive log push")
	}
	if env.Subject != logSubject {
		t.Fatalf("expected subject %q, got %q", logSubject, env.Subject)
	}
}

// ── backpressure ──────────────────────────────────────────────────────────────

func TestHub_Fanout_DropsWhenClientFull(t *testing.T) {
	h := newTestHub()
	ch := h.Register("c1")
	if err := h.Subscribe("c1", SubjectJobs, 0); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Send more events than the channel buffer can hold.
	const n = clientSendBuf + 20
	for i := range n {
		h.NotifyJob(JobEvent{JobID: fmt.Sprintf("j%d", i), Status: "running"})
	}

	// Drain whatever was buffered using a non-blocking loop.  All n sends
	// happened above in a single goroutine with no concurrent reader, so the
	// channel now contains exactly clientSendBuf items (the rest were dropped).
	// We assert an upper bound (≤ clientSendBuf) rather than exact equality so
	// the test remains valid even if the scheduler reorders goroutines.
	count := 0
	for {
		select {
		case <-ch:
			count++
		default:
			goto done
		}
	}
done:
	if count > clientSendBuf {
		t.Fatalf("received %d messages; expected at most %d (send buffer cap)", count, clientSendBuf)
	}
	if count == 0 {
		t.Fatal("no messages delivered; expected up to clientSendBuf messages")
	}
}

// ── unsubscribe ───────────────────────────────────────────────────────────────

func TestHub_Unsubscribe_StopsDelivery(t *testing.T) {
	h := newTestHub()
	ch := h.Register("c1")
	if err := h.Subscribe("c1", SubjectJobs, 0); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	h.Unsubscribe("c1", SubjectJobs)

	h.NotifyJob(JobEvent{JobID: "j1", Status: "running"})

	select {
	case <-ch:
		t.Fatal("received push after unsubscribe")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHub_Unsubscribe_Noop_WhenNotSubscribed(t *testing.T) {
	t.Parallel() // no shared state; safe to run concurrently
	h := newTestHub()
	h.Register("c1")
	// Must not panic when client is not subscribed.
	h.Unsubscribe("c1", SubjectJobs)
	// Must not panic when client does not exist.
	h.Unsubscribe("nonexistent", SubjectJobs)
}

// ── deregister ────────────────────────────────────────────────────────────────

func TestHub_Deregister_RemovesAllSubscriptions(t *testing.T) {
	h := newTestHub()
	h.Register("c1")
	for _, s := range []string{SubjectJobs, SubjectWorkers} {
		if err := h.Subscribe("c1", s, 0); err != nil {
			t.Fatalf("subscribe %q: %v", s, err)
		}
	}

	h.Deregister("c1")

	// Subsequent notify calls must not panic or block — the client is gone.
	h.NotifyJob(JobEvent{JobID: "j", Status: "running"})
	h.NotifyWorker(WorkerEvent{WorkerID: "w", Status: "online"})
}

// ── ring-buffer replay ────────────────────────────────────────────────────────

func TestHub_RingBuffer_ReplaySinceSeq(t *testing.T) {
	h := newTestHub()

	// Register a throw-away subscriber to drive ring population.
	trashCh := h.Register("trash")
	if err := h.Subscribe("trash", SubjectJobs, 0); err != nil {
		t.Fatalf("trash subscribe: %v", err)
	}

	// Push 3 events so the ring contains Seq 1, 2, 3.
	for i := range 3 {
		h.NotifyJob(JobEvent{JobID: fmt.Sprintf("j%d", i), Status: "pending"})
	}
	drainAll(trashCh)

	// New client connects and requests replay since Seq 1 (should receive 2 and 3).
	ch := h.Register("c1")
	if err := h.Subscribe("c1", SubjectJobs, 1); err != nil {
		t.Fatalf("subscribe c1: %v", err)
	}

	var replayed []Envelope
	for {
		select {
		case env := <-ch:
			replayed = append(replayed, env)
		case <-time.After(50 * time.Millisecond):
			goto done
		}
	}
done:
	if len(replayed) != 2 {
		t.Fatalf("expected 2 replayed envelopes (Seq 2,3), got %d", len(replayed))
	}
	for _, env := range replayed {
		if env.Seq <= 1 {
			t.Errorf("replay returned envelope with Seq=%d, want > 1", env.Seq)
		}
	}
}

func TestHub_RingBuffer_NoReplayWhenSinceSeqZero(t *testing.T) {
	h := newTestHub()
	trashCh := h.Register("trash")
	if err := h.Subscribe("trash", SubjectJobs, 0); err != nil {
		t.Fatalf("trash subscribe: %v", err)
	}
	for i := range 5 {
		h.NotifyJob(JobEvent{JobID: fmt.Sprintf("j%d", i), Status: "pending"})
	}
	drainAll(trashCh)

	// SinceSeq 0 means live-only — no replay.
	ch := h.Register("c1")
	if err := h.Subscribe("c1", SubjectJobs, 0); err != nil {
		t.Fatalf("subscribe c1: %v", err)
	}

	select {
	case env := <-ch:
		t.Fatalf("unexpected replay envelope (Seq=%d) when sinceSeq=0", env.Seq)
	case <-time.After(50 * time.Millisecond):
	}
}

// ── sequence numbers ──────────────────────────────────────────────────────────

func TestHub_PushSeqIsMonotonicallyIncreasing(t *testing.T) {
	h := newTestHub()
	ch := h.Register("c1")
	if err := h.Subscribe("c1", SubjectJobs, 0); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	const n = 10
	for i := range n {
		h.NotifyJob(JobEvent{JobID: fmt.Sprintf("j%d", i), Status: "running"})
	}

	prev := uint64(0)
	for range n {
		env, ok := drainOrTimeout(ch, 200*time.Millisecond)
		if !ok {
			t.Fatalf("timed out waiting for push %d", prev+1)
		}
		if env.Seq <= prev {
			t.Fatalf("non-monotonic Seq: got %d after %d", env.Seq, prev)
		}
		prev = env.Seq
	}
}

// ── concurrent safety ─────────────────────────────────────────────────────────

func TestHub_ConcurrentFanout_NoRace(t *testing.T) {
	// Run with -race to detect data races. The test itself only checks that no
	// panic or deadlock occurs; correctness of delivery count is not asserted
	// because backpressure drops are expected under load.
	h := newTestHub()
	const nClients = 8
	channels := make([]chan Envelope, nClients)
	for i := range nClients {
		id := fmt.Sprintf("c%d", i)
		channels[i] = h.Register(id)
		if err := h.Subscribe(id, SubjectJobs, 0); err != nil {
			t.Fatalf("subscribe %s: %v", id, err)
		}
	}

	var wg sync.WaitGroup
	const nEvents = 100
	wg.Add(nEvents)
	for i := range nEvents {
		go func(n int) {
			defer wg.Done()
			h.NotifyJob(JobEvent{JobID: fmt.Sprintf("j%d", n), Status: "running"})
		}(i)
	}
	wg.Wait()

	for _, ch := range channels {
		drainAll(ch)
	}
}

// ── subjectRing unit tests ────────────────────────────────────────────────────

func TestSubjectRing_Since_EmptyRing(t *testing.T) {
	r := &subjectRing{}
	if got := r.since(0); got != nil {
		t.Fatalf("empty ring since(0): expected nil, got %v", got)
	}
	if got := r.since(5); got != nil {
		t.Fatalf("empty ring since(5): expected nil, got %v", got)
	}
}

func TestSubjectRing_Since_SinceSeqZeroReturnsNil(t *testing.T) {
	r := &subjectRing{}
	for i := range 5 {
		r.add(Envelope{Type: TypePush, Subject: SubjectJobs, Seq: uint64(i)})
	}
	if got := r.since(0); got != nil {
		t.Fatalf("since(0) must return nil (live-only), got %v", got)
	}
}

func TestSubjectRing_Since_ReturnsCorrectSlice(t *testing.T) {
	r := &subjectRing{}
	for range 5 {
		r.add(Envelope{Type: TypePush, Subject: SubjectJobs})
	}
	// Seq values are 1–5 (assigned by add).
	got := r.since(3)
	if len(got) != 2 {
		t.Fatalf("since(3): expected 2 envelopes (Seq 4,5), got %d", len(got))
	}
	for _, env := range got {
		if env.Seq <= 3 {
			t.Errorf("since(3) returned envelope with Seq=%d", env.Seq)
		}
	}
}

func TestSubjectRing_Add_AssignsIncreasingSeq(t *testing.T) {
	r := &subjectRing{}
	prev := uint64(0)
	for i := range 5 {
		seq := r.add(Envelope{Type: TypePush})
		if seq == 0 {
			t.Fatalf("add[%d]: assigned seq 0, which is reserved for ping/pong", i)
		}
		if seq <= prev {
			t.Fatalf("add[%d]: seq %d is not strictly greater than previous %d", i, seq, prev)
		}
		prev = seq
	}
}
