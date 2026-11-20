// SPDX-License-Identifier: AGPL-3.0-or-later

package logstreamer_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/worker/logstreamer"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// ── fakeTransport ─────────────────────────────────────────────────────────────

// fakeTransport stores what it is sent. It fails the first failFirst tries of
// each message ID and, while block is open, holds the ack of every try until
// block is closed or the wait's context ends.
type fakeTransport struct {
	mu        sync.Mutex
	failFirst int
	refuse    map[string]int  // sends of a message ID refused before it is sent
	tooBig    map[string]bool // message IDs refused as over the payload limit
	block     chan struct{}
	tries     map[string]int
	tried     []string // the message ID of every try, in order
	stored    []protocol.LogChunkMsg
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{tries: map[string]int{}}
}

func (f *fakeTransport) blockAll() { f.mu.Lock(); f.block = make(chan struct{}); f.mu.Unlock() }

// releaseAndBlock lets every held ack through and holds the acks of later
// tries.
func (f *fakeTransport) releaseAndBlock() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.block != nil {
		close(f.block)
	}
	f.block = make(chan struct{})
}

// waitTried waits until n tries have been made.
func (f *fakeTransport) waitTried(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(f.triedIDs()) < n && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := f.triedIDs(); len(got) < n {
		t.Fatalf("tries = %v, want %d", got, n)
	}
}

func (f *fakeTransport) release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.block != nil {
		close(f.block)
		f.block = nil
	}
}

func (f *fakeTransport) Publish(_ string, data []byte, msgID string) (logstreamer.AckWait, error) {
	f.mu.Lock()
	if f.tooBig[msgID] {
		f.mu.Unlock()
		return nil, nats.ErrMaxPayload
	}
	if f.refuse[msgID] > 0 {
		f.refuse[msgID]--
		f.mu.Unlock()
		return nil, errors.New("nats: connection reconnecting")
	}
	f.tried = append(f.tried, msgID)
	block := f.block
	f.mu.Unlock()
	return func(ctx context.Context) error {
		if block != nil {
			select {
			case <-block:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.tries[msgID]++
		if f.tries[msgID] <= f.failFirst {
			return errors.New("nats: timeout")
		}
		var c protocol.LogChunkMsg
		if err := json.Unmarshal(data, &c); err != nil {
			return err
		}
		f.stored = append(f.stored, c)
		return nil
	}, nil
}

func (f *fakeTransport) storedChunks() []protocol.LogChunkMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]protocol.LogChunkMsg(nil), f.stored...)
}

func (f *fakeTransport) triedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.tried...)
}

// ── capturing logger ──────────────────────────────────────────────────────────

type captured struct {
	mu   sync.Mutex
	msgs []string
}

func (*captured) Enabled(context.Context, slog.Level) bool { return true }

func (c *captured) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, r.Message)
	return nil
}

func (c *captured) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *captured) WithGroup(string) slog.Handler      { return c }

func (c *captured) count(sub string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, m := range c.msgs {
		if strings.Contains(m, sub) {
			n++
		}
	}
	return n
}

// fastKnobs keeps every delivery wait short.
func fastKnobs() logstreamer.Knobs {
	return logstreamer.Knobs{
		AckTimeout: 200 * time.Millisecond,
		RetryBase:  5 * time.Millisecond,
		RetryMax:   20 * time.Millisecond,
		FlushWait:  2 * time.Second,
	}
}

func waitIdle(t *testing.T, p *logstreamer.Publisher) {
	t.Helper()
	select {
	case <-p.SenderIdle():
	case <-time.After(2 * time.Second):
		t.Fatal("the sender goroutine did not exit")
	}
}

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestPublisher_RetriesUntilStored(t *testing.T) {
	ft := newFakeTransport()
	ft.failFirst = 3
	p := logstreamer.New(ft, testWorkerID, logstreamer.WithKnobs(slowFlushCfg(1000, 1<<20), fastKnobs()), discard())
	p.HandleLine(context.Background(), "task-r", "attempt-r", "", "stdout", "hello")
	if err := p.FlushLogs(context.Background(), "task-r", "attempt-r"); err != nil {
		t.Fatal(err)
	}
	if got := ft.storedChunks(); len(got) != 1 || got[0].Data != "hello" {
		t.Fatalf("stored = %+v, want the one chunk", got)
	}
	if got := len(ft.triedIDs()); got != 4 {
		t.Errorf("tries = %d, want 4 (3 failed, 1 stored)", got)
	}
}

func TestPublisher_RetrySendsTheSameMessageID(t *testing.T) {
	ft := newFakeTransport()
	ft.failFirst = 2
	p := logstreamer.New(ft, testWorkerID, logstreamer.WithKnobs(slowFlushCfg(1000, 1<<20), fastKnobs()), discard())
	p.HandleLine(context.Background(), "task-id", "attempt-id", "", "stdout", "hello")
	if err := p.FlushLogs(context.Background(), "task-id", "attempt-id"); err != nil {
		t.Fatal(err)
	}
	ids := ft.triedIDs()
	if len(ids) != 3 {
		t.Fatalf("tries = %v, want 3", ids)
	}
	for _, id := range ids {
		if id != "attempt-id/1" {
			t.Errorf("message ID = %q, want %q", id, "attempt-id/1")
		}
	}
}

func TestPublisher_HandleLineDoesNotWaitForTheBroker(t *testing.T) {
	ft := newFakeTransport()
	ft.blockAll()
	p := logstreamer.New(ft, testWorkerID, logstreamer.WithKnobs(slowFlushCfg(1, 1<<20), fastKnobs()), discard())
	start := time.Now()
	for i := range 100 {
		p.HandleLine(context.Background(), "task-b", "attempt-b", "", "stdout", fmt.Sprintf("line %d", i))
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("100 HandleLine calls took %v against a broker that never acks, want under 1s", d)
	}
	p.Abandon()
}

// TestPublisher_KeepsSeveralChunksInFlight pins that delivery does not wait
// for one chunk's ack before sending the next: a worker far from the broker
// would otherwise deliver one chunk per round trip. Chunks queued behind a
// send go out together, up to the window, in order.
func TestPublisher_KeepsSeveralChunksInFlight(t *testing.T) {
	ft := newFakeTransport()
	ft.blockAll()
	k := fastKnobs()
	k.Window = 4
	k.AckTimeout = 5 * time.Second // no try times out while the test looks
	p := logstreamer.New(ft, testWorkerID, logstreamer.WithKnobs(slowFlushCfg(1, 1<<20), k), discard())
	line := func(i int) {
		p.HandleLine(context.Background(), "task-f", "attempt-f", "", "stdout", fmt.Sprintf("line %d", i))
	}
	line(1)
	ft.waitTried(t, 1)
	for i := 2; i <= 10; i++ {
		line(i)
	}
	ft.releaseAndBlock() // chunk 1 is acked; the next send's acks are held
	ft.waitTried(t, 5)
	time.Sleep(20 * time.Millisecond)
	want := []string{"attempt-f/1", "attempt-f/2", "attempt-f/3", "attempt-f/4", "attempt-f/5"}
	if got := ft.triedIDs(); !slices.Equal(got, want) {
		t.Fatalf("sent = %v, want %v (chunk 1, then a window of four in order)", got, want)
	}
	ft.release()
	if dropped := p.Flush(contextWithTimeout(t, 5*time.Second)); dropped != 0 {
		t.Errorf("Flush dropped %d, want 0", dropped)
	}
	if got := seqs(ft.storedChunks()); !slices.Equal(got, []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}) {
		t.Errorf("stored seqs = %v, want 1 to 10 in order", got)
	}
}

// TestPublisher_StopsASendAtARefusal pins that a send stops at the first
// chunk the transport refuses, as it does while reconnecting, so a later
// chunk is not stored ahead of it.
func TestPublisher_StopsASendAtARefusal(t *testing.T) {
	ft := newFakeTransport()
	ft.refuse = map[string]int{"attempt-s/2": 1}
	ft.blockAll()
	k := fastKnobs()
	k.Window = 4
	p := logstreamer.New(ft, testWorkerID, logstreamer.WithKnobs(slowFlushCfg(1, 1<<20), k), discard())
	p.HandleLine(context.Background(), "task-s", "attempt-s", "", "stdout", "line 1")
	ft.waitTried(t, 1)
	for i := 2; i <= 4; i++ {
		p.HandleLine(context.Background(), "task-s", "attempt-s", "", "stdout", fmt.Sprintf("line %d", i))
	}
	ft.release()
	if dropped := p.Flush(contextWithTimeout(t, 5*time.Second)); dropped != 0 {
		t.Errorf("Flush dropped %d, want 0", dropped)
	}
	want := []string{"attempt-s/1", "attempt-s/2", "attempt-s/3", "attempt-s/4"}
	if got := ft.triedIDs(); !slices.Equal(got, want) {
		t.Errorf("sent = %v, want %v (nothing after the refused chunk until it is sent)", got, want)
	}
	if got := seqs(ft.storedChunks()); !slices.Equal(got, []int64{1, 2, 3, 4}) {
		t.Errorf("stored seqs = %v, want 1 to 4 in order", got)
	}
}

// TestPublisher_AnOversizedChunkDropsOnlyItself pins that a chunk no retry
// can send is dropped alone: the chunks after it in the same send go out.
func TestPublisher_AnOversizedChunkDropsOnlyItself(t *testing.T) {
	ft := newFakeTransport()
	ft.tooBig = map[string]bool{"attempt-m/2": true}
	ft.blockAll()
	k := fastKnobs()
	k.Window = 4
	p := logstreamer.New(ft, testWorkerID, logstreamer.WithKnobs(slowFlushCfg(1, 1<<20), k), discard())
	p.HandleLine(context.Background(), "task-m", "attempt-m", "", "stdout", "line 1")
	ft.waitTried(t, 1)
	for i := 2; i <= 4; i++ {
		p.HandleLine(context.Background(), "task-m", "attempt-m", "", "stdout", fmt.Sprintf("line %d", i))
	}
	ft.release()
	if dropped := p.Flush(contextWithTimeout(t, 5*time.Second)); dropped != 0 {
		t.Errorf("Flush dropped %d, want 0", dropped)
	}
	if got := seqs(ft.storedChunks()); !slices.Equal(got, []int64{1, 3, 4}) {
		t.Errorf("stored seqs = %v, want 1, 3 and 4", got)
	}
}

func TestFlushLogs_WaitsForItsAttempt(t *testing.T) {
	ft := newFakeTransport()
	ft.blockAll()
	p := logstreamer.New(ft, testWorkerID, logstreamer.WithKnobs(slowFlushCfg(1000, 1<<20), fastKnobs()), discard())
	p.HandleLine(context.Background(), "task-w", "attempt-w", "", "stdout", "hello")
	time.AfterFunc(200*time.Millisecond, ft.release)
	start := time.Now()
	if err := p.FlushLogs(context.Background(), "task-w", "attempt-w"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Errorf("FlushLogs returned after %v, before the broker acked", d)
	}
	if got := ft.storedChunks(); len(got) != 1 {
		t.Errorf("stored %d chunks when FlushLogs returned, want 1", len(got))
	}
}

func TestFlushLogs_ReturnsAfterTheWaitAndDeliversLater(t *testing.T) {
	ft := newFakeTransport()
	ft.blockAll()
	k := fastKnobs()
	k.FlushWait = 100 * time.Millisecond
	p := logstreamer.New(ft, testWorkerID, logstreamer.WithKnobs(slowFlushCfg(1000, 1<<20), k), discard())
	p.HandleLine(context.Background(), "task-l", "attempt-l", "", "stdout", "hello")
	start := time.Now()
	if err := p.FlushLogs(context.Background(), "task-l", "attempt-l"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("FlushLogs took %v on a broker that never acks, want about the 100ms wait", d)
	}
	if got := ft.storedChunks(); len(got) != 0 {
		t.Fatalf("stored %d chunks before the broker acked", len(got))
	}
	ft.release()
	if dropped := p.Flush(contextWithTimeout(t, 5*time.Second)); dropped != 0 {
		t.Errorf("Flush dropped %d, want 0", dropped)
	}
	if got := ft.storedChunks(); len(got) != 1 {
		t.Errorf("stored %d chunks after the broker came back, want 1", len(got))
	}
}

func TestPublisher_BoundDropsTheOldest(t *testing.T) {
	ft := newFakeTransport()
	ft.blockAll()
	log := &captured{}
	k := fastKnobs()
	// Each encoded chunk is about 200 bytes; the bound holds a few. One
	// chunk in flight keeps the rest evictable.
	k.MaxPendingBytes = 700
	k.Window = 1
	p := logstreamer.New(ft, testWorkerID, logstreamer.WithKnobs(slowFlushCfg(1, 1<<20), k), slog.New(log))
	for i := 1; i <= 10; i++ {
		p.HandleLine(context.Background(), "task-o", "attempt-o", "", "stdout", fmt.Sprintf("line %02d", i))
	}
	ft.release()
	if dropped := p.Flush(contextWithTimeout(t, 5*time.Second)); dropped != 0 {
		t.Errorf("Flush dropped %d, want 0", dropped)
	}
	got := ft.storedChunks()
	if len(got) == 0 || len(got) >= 10 {
		t.Fatalf("stored %d chunks, want some but not all 10", len(got))
	}
	// The newest chunk is kept; whatever was dropped was older than every
	// chunk still queued, so the stored seq nums after the first one in
	// flight are the newest run.
	if last := got[len(got)-1].SeqNum; last != 10 {
		t.Errorf("last stored seq = %d, want 10 (the newest is kept)", last)
	}
	for i := 2; i < len(got); i++ {
		if got[i].SeqNum != got[i-1].SeqNum+1 {
			t.Errorf("stored seqs %v are not the newest run after the first", seqs(got))
			break
		}
	}
	if n := log.count("dropped log chunks the broker has not stored"); n != 1 {
		t.Errorf("drop warnings = %d, want 1", n)
	}
}

func TestPublisher_FlushDropsWhatTheDeadlineLeaves(t *testing.T) {
	ft := newFakeTransport()
	ft.blockAll()
	p := logstreamer.New(ft, testWorkerID, logstreamer.WithKnobs(slowFlushCfg(1, 1<<20), fastKnobs()), discard())
	for i := range 3 {
		p.HandleLine(context.Background(), "task-d", "attempt-d", "", "stdout", fmt.Sprintf("line %d", i))
	}
	if dropped := p.Flush(contextWithTimeout(t, 100*time.Millisecond)); dropped != 3 {
		t.Errorf("Flush dropped %d, want 3", dropped)
	}
	waitIdle(t, p)
}

func TestPublisher_AbandonStopsTheSender(t *testing.T) {
	ft := newFakeTransport()
	ft.blockAll()
	p := logstreamer.New(ft, testWorkerID, logstreamer.WithKnobs(slowFlushCfg(1, 1<<20), fastKnobs()), discard())
	for i := range 2 {
		p.HandleLine(context.Background(), "task-a", "attempt-a", "", "stdout", fmt.Sprintf("line %d", i))
	}
	if dropped := p.Abandon(); dropped != 2 {
		t.Errorf("Abandon dropped %d, want 2", dropped)
	}
	waitIdle(t, p)
	ft.release()
	if got := ft.storedChunks(); len(got) != 0 {
		t.Errorf("stored %d chunks after Abandon, want 0", len(got))
	}
}

func contextWithTimeout(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func seqs(cs []protocol.LogChunkMsg) []int64 {
	out := make([]int64, len(cs))
	for i, c := range cs {
		out[i] = c.SeqNum
	}
	return out
}
