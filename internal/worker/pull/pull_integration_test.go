// SPDX-License-Identifier: AGPL-3.0-or-later

package pull

// Integration tests that exercise the pull loop against a live embedded NATS
// JetStream broker provisioned via internal/bus. These cover consumer setup
// (ensureConsumers / ensureQueueConsumer / ensureWildcardConsumer), the full
// Run → runConsumerLoop → drainBatch path, the real ack/nak semantics through
// wrapMsg, the empty-queue idle backoff, and the concurrency-slot gate.
//
// The unit-level handleMessage paths are covered with mocks in pull_test.go;
// these tests drive the real JetStream collaborator deterministically using
// channel+select with bounded timeouts (no bare sleeps for synchronization).

import (
	"context"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// ── harness ─────────────────────────────────────────────────────────────────

// freePort asks the OS for an available loopback TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("freePort: unexpected addr type %T", ln.Addr())
	}
	port := tcpAddr.Port
	if closeErr := ln.Close(); closeErr != nil {
		t.Fatalf("freePort: close: %v", closeErr)
	}
	return port
}

// newWorkBroker boots an embedded broker (with the SQI_WORK stream provisioned)
// and returns a JetStream context plus the underlying connection. All resources
// are cleaned up via t.Cleanup.
func newWorkBroker(t *testing.T) (jetstream.JetStream, *nats.Conn) {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	cfg := bus.BrokerConfig{
		Addr:       net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort(t))),
		DataDir:    t.TempDir() + "/nats",
		MaxStoreMB: 64,
	}
	b := bus.New(cfg, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("newWorkBroker: Start: %v", err)
	}
	t.Cleanup(b.Shutdown)

	nc, err := nats.Connect(b.ClientURL())
	if err != nil {
		t.Fatalf("newWorkBroker: connect: %v", err)
	}
	t.Cleanup(nc.Close)

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("newWorkBroker: jetstream: %v", err)
	}
	return js, nc
}

// publishAssign publishes data to the given work-assign subject through
// JetStream so it is captured by the SQI_WORK stream.
func publishAssign(t *testing.T, js jetstream.JetStream, subject string, data []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := js.Publish(ctx, subject, data); err != nil {
		t.Fatalf("publishAssign: %v", err)
	}
}

// fetchOne fetches a single message from the consumer, blocking up to wait.
// Returns (msg, true) on success or (nil, false) when the queue is empty.
func fetchOne(t *testing.T, c jetstream.Consumer, wait time.Duration) (jetstream.Msg, bool) {
	t.Helper()
	batch, err := c.Fetch(1, jetstream.FetchMaxWait(wait))
	if err != nil {
		t.Fatalf("fetchOne: %v", err)
	}
	for m := range batch.Messages() {
		return m, true
	}
	if err := batch.Error(); err != nil {
		t.Fatalf("fetchOne: batch error: %v", err)
	}
	return nil, false
}

// signalDispatcher is a concurrency-safe [TaskDispatcher] that records each
// dispatched assignment and (optionally) signals on a channel.
type signalDispatcher struct {
	mu  sync.Mutex
	got []*protocol.AssignMsg
	ch  chan *protocol.AssignMsg
	err error
}

func (d *signalDispatcher) Dispatch(_ context.Context, msg *protocol.AssignMsg) error {
	d.mu.Lock()
	d.got = append(d.got, msg)
	d.mu.Unlock()
	if d.ch != nil {
		select {
		case d.ch <- msg:
		default:
		}
	}
	return d.err
}

func (d *signalDispatcher) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.got)
}

// atomicState is a [StateSource] whose active-task count can be changed
// concurrently from a test.
type atomicState struct{ n atomic.Int64 }

func (s *atomicState) ActiveTaskCount() int { return int(s.n.Load()) }

// ── ensureConsumers ─────────────────────────────────────────────────────────

func TestEnsureConsumers_Wildcard(t *testing.T) {
	js, _ := newWorkBroker(t)
	p := New(js, Config{QueueIDs: nil, MaxConcurrentTasks: 2}, &mockStateSource{}, &signalDispatcher{}, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	consumers, err := p.ensureConsumers(ctx)
	if err != nil {
		t.Fatalf("ensureConsumers: %v", err)
	}
	if len(consumers) != 1 {
		t.Fatalf("expected 1 consumer, got %d", len(consumers))
	}
	if _, ok := consumers["*"]; !ok {
		t.Errorf("expected wildcard consumer keyed %q, got keys %v", "*", keys(consumers))
	}
}

func TestEnsureConsumers_PerQueue(t *testing.T) {
	js, _ := newWorkBroker(t)
	p := New(js, Config{QueueIDs: []string{"q1", "q2"}, MaxConcurrentTasks: 2}, &mockStateSource{}, &signalDispatcher{}, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	consumers, err := p.ensureConsumers(ctx)
	if err != nil {
		t.Fatalf("ensureConsumers: %v", err)
	}
	if len(consumers) != 2 {
		t.Fatalf("expected 2 consumers, got %d (%v)", len(consumers), keys(consumers))
	}
	for _, q := range []string{"q1", "q2"} {
		if _, ok := consumers[q]; !ok {
			t.Errorf("missing consumer for queue %q (keys %v)", q, keys(consumers))
		}
	}
}

func keys(m map[string]jetstream.Consumer) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ── wrapMsg: real Ack ───────────────────────────────────────────────────────

// TestHandleMessage_RealAck asserts that a valid assignment is dispatched and
// acked through the real jetstream.Msg, so the message is removed from the
// work-queue stream and is not redelivered.
func TestHandleMessage_RealAck(t *testing.T) {
	js, _ := newWorkBroker(t)
	disp := &signalDispatcher{}
	p := New(js, Config{QueueIDs: []string{"q1"}, MaxConcurrentTasks: 4, NackDelay: 200 * time.Millisecond}, &mockStateSource{}, disp, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := p.ensureQueueConsumer(ctx, "q1")
	if err != nil {
		t.Fatalf("ensureQueueConsumer: %v", err)
	}

	publishAssign(t, js, bus.WorkAssignSubject("q1"), validAssignMsg("task-ack", ""))

	msg, ok := fetchOne(t, c, 2*time.Second)
	if !ok {
		t.Fatal("expected to fetch the published assignment")
	}
	p.handleMessage(ctx, wrapMsg(msg), "q1")

	if disp.count() != 1 {
		t.Fatalf("expected 1 dispatch, got %d", disp.count())
	}

	// WorkQueuePolicy removes acked messages: a second fetch must be empty.
	if _, ok := fetchOne(t, c, 500*time.Millisecond); ok {
		t.Error("expected no redelivery after a successful ack")
	}
}

// ── wrapMsg: real Nak / redelivery ──────────────────────────────────────────

// TestHandleMessage_RealNak_Redelivers asserts a malformed payload is nacked
// through the real jetstream.Msg and subsequently redelivered.
func TestHandleMessage_RealNak_Redelivers(t *testing.T) {
	js, _ := newWorkBroker(t)
	disp := &signalDispatcher{}
	p := New(js, Config{QueueIDs: []string{"q1"}, MaxConcurrentTasks: 4, NackDelay: 200 * time.Millisecond}, &mockStateSource{}, disp, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := p.ensureQueueConsumer(ctx, "q1")
	if err != nil {
		t.Fatalf("ensureQueueConsumer: %v", err)
	}

	publishAssign(t, js, bus.WorkAssignSubject("q1"), []byte("not a valid assign payload"))

	msg, ok := fetchOne(t, c, 2*time.Second)
	if !ok {
		t.Fatal("expected to fetch the published (malformed) message")
	}
	p.handleMessage(ctx, wrapMsg(msg), "q1")

	if disp.count() != 0 {
		t.Errorf("malformed payload must not be dispatched, got %d", disp.count())
	}

	// Nacked with a 200ms delay → redelivered within the fetch window.
	msg2, ok := fetchOne(t, c, 3*time.Second)
	if !ok {
		t.Fatal("expected the nacked message to be redelivered")
	}
	meta, err := msg2.Metadata()
	if err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	if meta.NumDelivered < 2 {
		t.Errorf("NumDelivered = %d, want >= 2 (redelivery)", meta.NumDelivered)
	}
	if err := msg2.Ack(); err != nil { // tidy up so the broker can shut down cleanly
		t.Fatalf("Ack: %v", err)
	}
}

// ── Full Run loop ───────────────────────────────────────────────────────────

// TestRun_DispatchesAndAcks drives the full Run → runConsumerLoop → drainBatch
// path against a live consumer. The dispatcher signals receipt on a channel;
// the test then cancels the context and waits for Run to return.
func TestRun_DispatchesAndAcks(t *testing.T) {
	js, _ := newWorkBroker(t)
	disp := &signalDispatcher{ch: make(chan *protocol.AssignMsg, 8)}
	p := New(js, Config{
		QueueIDs:           []string{"q1"},
		MaxConcurrentTasks: 2,
		IdleBackoff:        100 * time.Millisecond,
		NackDelay:          200 * time.Millisecond,
	}, &mockStateSource{}, disp, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()

	publishAssign(t, js, bus.WorkAssignSubject("q1"), validAssignMsg("task-run", ""))

	select {
	case got := <-disp.ch:
		if got.TaskID != "task-run" {
			t.Errorf("dispatched TaskID = %q, want %q", got.TaskID, "task-run")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the assignment to be dispatched")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

// TestRun_WildcardConsumer drives Run with no queue IDs (wildcard consumer) to
// confirm the work.assign.> filter receives an assignment on any queue.
func TestRun_WildcardConsumer(t *testing.T) {
	js, _ := newWorkBroker(t)
	disp := &signalDispatcher{ch: make(chan *protocol.AssignMsg, 8)}
	p := New(js, Config{
		QueueIDs:           nil,
		MaxConcurrentTasks: 2,
		IdleBackoff:        100 * time.Millisecond,
	}, &mockStateSource{}, disp, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()

	publishAssign(t, js, bus.WorkAssignSubject("any-queue"), validAssignMsg("task-wild", ""))

	select {
	case got := <-disp.ch:
		if got.TaskID != "task-wild" {
			t.Errorf("dispatched TaskID = %q, want %q", got.TaskID, "task-wild")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for wildcard dispatch")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

// ── Concurrency slot gate ───────────────────────────────────────────────────

// TestRun_AtCapacity_NoFetchUntilSlotOpens asserts the loop does not fetch while
// active == MaxConcurrentTasks (slots <= 0), and resumes once a slot frees up.
func TestRun_AtCapacity_NoFetchUntilSlotOpens(t *testing.T) {
	js, _ := newWorkBroker(t)
	disp := &signalDispatcher{ch: make(chan *protocol.AssignMsg, 8)}
	state := &atomicState{}
	state.n.Store(2) // fully occupied

	p := New(js, Config{
		QueueIDs:           []string{"q1"},
		MaxConcurrentTasks: 2,
		IdleBackoff:        100 * time.Millisecond,
	}, state, disp, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()

	publishAssign(t, js, bus.WorkAssignSubject("q1"), validAssignMsg("task-cap", ""))

	// While at capacity, no dispatch should occur within this window.
	select {
	case got := <-disp.ch:
		t.Fatalf("dispatched %q while at capacity; loop should be gated", got.TaskID)
	case <-time.After(600 * time.Millisecond):
		// expected: no dispatch
	}

	// Free a slot; the loop should now fetch and dispatch.
	state.n.Store(0)
	select {
	case got := <-disp.ch:
		if got.TaskID != "task-cap" {
			t.Errorf("dispatched TaskID = %q, want %q", got.TaskID, "task-cap")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("assignment not dispatched after a slot opened")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}
