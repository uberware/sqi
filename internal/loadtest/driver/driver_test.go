// SPDX-License-Identifier: AGPL-3.0-or-later

package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/simworker"
	"github.com/uberware/sqi/internal/ws"
)

// fakeTarget is a target.Target over an httptest server.
type fakeTarget struct {
	url    string
	farm   string
	queues map[string]string
}

func (f fakeTarget) HTTPURL() string                  { return f.url }
func (fakeTarget) NATSURL() string                    { return "" }
func (fakeTarget) APIKey() string                     { return "" }
func (f fakeTarget) FarmID() string                   { return f.farm }
func (f fakeTarget) QueueIDs() map[string]string      { return f.queues }
func (fakeTarget) DataDir() string                    { return "" }
func (fakeTarget) Credentials() simworker.Credentials { return nil }
func (fakeTarget) TLS() simworker.TLSFiles            { return simworker.TLSFiles{} }
func (fakeTarget) NoopCommand() scenario.Command {
	return scenario.Command{Path: "/bin/true"}
}
func (fakeTarget) Close(context.Context) error { return nil }
func (fakeTarget) PoolName(name string) string { return name }
func (fakeTarget) PostgresDSN() string         { return "" }
func (fakeTarget) StoreLabel() string          { return "" }

func newFakeTarget(url string) fakeTarget {
	return fakeTarget{url: url, farm: "farm-1", queues: map[string]string{"q": "queue-1", "other": "queue-2"}}
}

func oneClass(total int, rate float64) []scenario.JobClass {
	jc := scenario.JobClass{Name: "c", Total: total, Queue: "q", Steps: 1, TasksPerStep: 1}
	jc.Rate.PerSecond = rate
	return []scenario.JobClass{jc}
}

func TestPacer_OpenLoop(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		rate float64
		n    int
		want time.Duration
	}{
		{"50th at 10/s", 10, 50, 4900 * time.Millisecond},
		{"1st is start", 10, 1, 0},
		{"100th at 0.5/s", 0.5, 100, 198 * time.Second},
		{"11th at 1000/s", 1000, 11, 10 * time.Millisecond},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPacer(tc.rate, t0)
			var due time.Time
			for range tc.n {
				// The pacer must not look at the clock: call it late, in a
				// burst, and the schedule is unchanged.
				due = p.Next()
			}
			if got := due.Sub(t0); got != tc.want {
				t.Errorf("due #%d = t0+%v, want t0+%v", tc.n, got, tc.want)
			}
		})
	}
}

func TestPacer_ConcurrentCallsNeverRepeatASlot(t *testing.T) {
	t0 := time.Unix(0, 0)
	p := NewPacer(1000, t0)
	const goroutines, each = 8, 250
	var mu sync.Mutex
	seen := map[time.Duration]bool{}
	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range each {
				d := p.Next().Sub(t0)
				mu.Lock()
				seen[d] = true
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(seen) != goroutines*each {
		t.Errorf("%d distinct due times from %d calls", len(seen), goroutines*each)
	}
}

// jobServer is a stub sqi-server answering POST /api/v1/jobs from a script.
type jobServer struct {
	mu       sync.Mutex
	requests []submitted
	statuses []int // answered in turn; the last repeats
}

type submitted struct {
	N         int
	DependsOn []string
	Queue     string
	Body      string
}

func (s *jobServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body) //nolint:errcheck // test handler
		s.mu.Lock()
		n := len(s.requests) + 1
		s.requests = append(s.requests, submitted{
			N: n, DependsOn: r.URL.Query()["depends_on"], Queue: r.URL.Query().Get("queue_id"), Body: string(body),
		})
		status := http.StatusCreated
		if len(s.statuses) > 0 {
			status = s.statuses[min(n-1, len(s.statuses)-1)]
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusCreated {
			fmt.Fprintf(w, `{"id":"job-%d"}`, n)
			return
		}
		fmt.Fprint(w, `{"detail":"no"}`)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func (s *jobServer) seen() []submitted {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

func newSubmitter(t *testing.T, srv *httptest.Server, sc *scenario.Scenario, opts ...SubmitterOption) (*Submitter, *JobLedger, *Metrics) {
	t.Helper()
	l, m := NewJobLedger(), NewMetrics(nil)
	s := NewSubmitter(client.New(srv.URL), newFakeTarget(srv.URL), sc, l, m, rand.New(rand.NewPCG(7, 7)), opts...)
	return s, l, m
}

func TestSubmitter_CountsRejectionsByStatus(t *testing.T) {
	js := &jobServer{statuses: []int{201, 422, 429, 503}}
	srv := js.start(t)
	sc := &scenario.Scenario{Jobs: oneClass(4, 0)}
	s, ledger, m := newSubmitter(t, srv, sc)

	if err := s.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := m.StatusCounts()
	want := map[int]int64{201: 1, 422: 1, 429: 1, 503: 1}
	for code, n := range want {
		if got[code] != n {
			t.Errorf("ByStatus[%d] = %d, want %d (all: %v)", code, got[code], n, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("ByStatus has %v, want only %v", got, want)
	}
	if n := len(js.seen()); n != 4 {
		t.Errorf("server saw %d requests, want 4: a rejection must not be retried", n)
	}
	if n := len(ledger.All()); n != 4 {
		t.Errorf("ledger has %d attempts, want 4", n)
	}
	if n := ledger.AcceptedCount(); n != 1 {
		t.Errorf("accepted = %d, want 1", n)
	}
	if got, want := m.SubmitLatency.Count(), int64(4); got != want {
		t.Errorf("SubmitLatency count = %d, want %d", got, want)
	}
}

// poolTarget is a target whose usage pools have names of their own on the
// server, as an attached run's do.
type poolTarget struct{ fakeTarget }

func (poolTarget) PoolName(name string) string { return "load_r1_" + name }

func TestSubmitter_ClaimsThePoolByItsServerName(t *testing.T) {
	js := &jobServer{}
	srv := js.start(t)
	sc := &scenario.Scenario{Jobs: oneClass(1, 0)}
	sc.Jobs[0].UsagePool = "arnold"
	s := NewSubmitter(client.New(srv.URL), poolTarget{newFakeTarget(srv.URL)}, sc, NewJobLedger(), NewMetrics(nil), nil)
	if err := s.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	seen := js.seen()
	if len(seen) != 1 {
		t.Fatalf("server saw %d submissions, want 1", len(seen))
	}
	if body := seen[0].Body; !strings.Contains(body, "amount.worker.usagepool.load_r1_arnold") || strings.Contains(body, "usagepool.arnold") {
		t.Errorf("template does not claim the pool by its server name load_r1_arnold:\n%s", body)
	}
	if sc.Jobs[0].UsagePool != "arnold" {
		t.Errorf("the scenario's class was changed: usage_pool %q", sc.Jobs[0].UsagePool)
	}
}

func TestSubmitter_TransportErrorIsCountedAsStatusZero(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens any more
	l, m := NewJobLedger(), NewMetrics(nil)
	s := NewSubmitter(client.New(url), newFakeTarget(url), &scenario.Scenario{Jobs: oneClass(3, 0)}, l, m, nil)
	if err := s.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := m.StatusCounts()[0]; got != 3 {
		t.Errorf("ByStatus[0] = %d, want 3", got)
	}
}

func TestSubmitter_JobDeps(t *testing.T) {
	js := &jobServer{}
	srv := js.start(t)
	sc := &scenario.Scenario{JobDeps: 1.0, Jobs: oneClass(6, 0)}
	s, _, _ := newSubmitter(t, srv, sc, WithConcurrency(1))

	if err := s.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	reqs := js.seen()
	if len(reqs) != 6 {
		t.Fatalf("server saw %d requests, want 6", len(reqs))
	}
	if len(reqs[0].DependsOn) != 0 {
		t.Errorf("first job depends on %v, want nothing", reqs[0].DependsOn)
	}
	for _, r := range reqs[1:] {
		if len(r.DependsOn) != 1 {
			t.Errorf("job %d has depends_on %v, want exactly one earlier job", r.N, r.DependsOn)
			continue
		}
		dep, err := strconv.Atoi(strings.TrimPrefix(r.DependsOn[0], "job-"))
		if err != nil || dep >= r.N || dep < 1 {
			t.Errorf("job %d depends on %q, want an earlier accepted job", r.N, r.DependsOn[0])
		}
	}
}

func TestSubmitter_NoDepsWhenFractionZero(t *testing.T) {
	js := &jobServer{}
	srv := js.start(t)
	s, _, _ := newSubmitter(t, srv, &scenario.Scenario{Jobs: oneClass(5, 0)}, WithConcurrency(1))
	if err := s.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, r := range js.seen() {
		if len(r.DependsOn) != 0 {
			t.Errorf("job %d has depends_on %v with job_deps 0", r.N, r.DependsOn)
		}
	}
}

func TestSubmitter_TotalAttemptsExactlyAndNamesArePrefixed(t *testing.T) {
	js := &jobServer{statuses: []int{201, 422}} // every attempt after the first is rejected
	srv := js.start(t)
	s, ledger, _ := newSubmitter(t, srv, &scenario.Scenario{Jobs: oneClass(40, 0)}, WithNamePrefix("s2"))
	if err := s.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := len(js.seen()); n != 40 {
		t.Errorf("%d attempts, want exactly the total of 40", n)
	}
	names := map[string]bool{}
	for _, j := range ledger.All() {
		if !strings.HasPrefix(j.Name, "s2-c-") {
			t.Errorf("job name %q lacks the s2-c- shard prefix", j.Name)
		}
		names[j.Name] = true
	}
	if len(names) != 40 {
		t.Errorf("%d distinct names, want 40", len(names))
	}
	for _, r := range js.seen() {
		if r.Queue != "queue-1" {
			t.Errorf("submitted to queue %q, want queue-1", r.Queue)
		}
		if !strings.Contains(r.Body, "--sqi-load-class=c") {
			t.Errorf("template lacks the class arg:\n%s", r.Body)
		}
	}
}

func TestSubmitter_PacedRecordsLagAndStopsWithContext(t *testing.T) {
	js := &jobServer{}
	srv := js.start(t)
	s, ledger, m := newSubmitter(t, srv, &scenario.Scenario{Jobs: oneClass(0, 200)})

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := s.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	n := ledger.AcceptedCount()
	// 200/s for 0.3 s is 60 slots, plus the one at time 0. A loaded host
	// fires the deadline late, and every 5 ms slot due by then still goes
	// out, so the ceiling follows the time the pacer actually ran; the floor
	// leaves wide slack for a slow host.
	ceiling := int(time.Since(start)/(5*time.Millisecond)) + 1
	if n < 20 || n > ceiling {
		t.Errorf("%d jobs accepted at 200/s over %s, want about 60 and at most %d", n, time.Since(start).Round(time.Millisecond), ceiling)
	}
	if got := m.SubmitLag.Count(); got != int64(n) {
		t.Errorf("SubmitLag has %d samples for %d submissions, want one each", got, n)
	}
}

func TestSubmitter_SlowServerShowsAsLagNotSkips(t *testing.T) {
	// Every answer takes 100 ms and one request may be in flight at a time,
	// so a 100/s schedule can only fall behind. Slots must still all be
	// issued (late), not dropped.
	var inflight atomic.Int32
	var peak atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cur := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"x"}`)
	}))
	defer ts.Close()
	s, _, m := newSubmitter(t, ts, &scenario.Scenario{Jobs: oneClass(0, 100)})

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 100/s for 0.5 s is 50 slots, all in flight together because the pacer
	// does not wait for answers: that is the open loop.
	if got := m.SubmitLatency.Count(); got < 20 {
		t.Errorf("only %d submissions completed; the open loop must not wait for each answer", got)
	}
	if slots, answered := m.SubmitLag.Count(), m.SubmitLatency.Count(); slots != answered {
		t.Errorf("%d slots issued but %d submissions answered; every slot must go out", slots, answered)
	}
	if m.SubmitLag.Max() <= 0 {
		t.Error("SubmitLag.Max() = 0; a 100 ms server on a 10 ms schedule must show lag")
	}
	if peak.Load() < 2 {
		t.Errorf("never more than %d request in flight; a slow server must not throttle the pacer", peak.Load())
	}
}

func TestSubmitter_UnknownQueueIsAnError(t *testing.T) {
	js := &jobServer{}
	srv := js.start(t)
	jc := oneClass(1, 0)
	jc[0].Queue = "missing"
	s, _, _ := newSubmitter(t, srv, &scenario.Scenario{Jobs: jc})
	if err := s.Run(t.Context()); err == nil {
		t.Fatal("Run succeeded for a class naming a queue the target lacks")
	}
	if n := len(js.seen()); n != 0 {
		t.Errorf("%d requests sent before the error", n)
	}
}

func TestSubmitter_ClassesRunIndependently(t *testing.T) {
	js := &jobServer{}
	srv := js.start(t)
	a := scenario.JobClass{Name: "a", Total: 4, Queue: "q", Steps: 1, TasksPerStep: 1}
	b := scenario.JobClass{Name: "b", Total: 6, Queue: "other", Steps: 1, TasksPerStep: 1}
	s, ledger, _ := newSubmitter(t, srv, &scenario.Scenario{Jobs: []scenario.JobClass{a, b}})
	if err := s.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	count := map[string]int{}
	for _, j := range ledger.All() {
		count[j.Class]++
	}
	if count["a"] != 4 || count["b"] != 6 {
		t.Errorf("per-class attempts = %v, want a:4 b:6", count)
	}
}

func TestJobLedger(t *testing.T) {
	l := NewJobLedger()
	l.Record(SubmittedJob{ID: "a", Status: 201})
	l.Record(SubmittedJob{Name: "shot-2", Class: "shot", Status: 422})
	l.Record(SubmittedJob{ID: "b", Status: 201})
	l.Record(SubmittedJob{Name: "shot-4", Class: "shot", Status: 0})
	l.Record(SubmittedJob{ID: "c", Status: 202})

	if got := l.Rejected(); len(got) != 2 || got[0].Name != "shot-2" || got[1].Name != "shot-4" || got[1].Class != "shot" {
		t.Errorf("Rejected = %+v, want shot-2 and shot-4 with their class", got)
	}

	var ids []string
	for _, j := range l.Accepted() {
		ids = append(ids, j.ID)
	}
	if !slices.Equal(ids, []string{"a", "b", "c"}) {
		t.Errorf("Accepted = %v, want [a b c]", ids)
	}
	if len(l.All()) != 5 {
		t.Errorf("All has %d, want 5", len(l.All()))
	}
	if got := l.Recent(2); len(got) != 2 || got[0].ID != "b" || got[1].ID != "c" {
		t.Errorf("Recent(2) = %v, want [b c]", got)
	}
	if got := l.Recent(10); len(got) != 3 {
		t.Errorf("Recent(10) has %d, want 3", len(got))
	}
	if _, ok := NewJobLedger().Pick(rand.New(rand.NewPCG(1, 1))); ok {
		t.Error("Pick on an empty ledger succeeded")
	}
	if j, ok := l.Pick(rand.New(rand.NewPCG(1, 1))); !ok || !j.Accepted() {
		t.Errorf("Pick = %v, %v, want an accepted job", j, ok)
	}
}

func TestMetrics_StatusIsSafeUnderConcurrency(t *testing.T) {
	m := NewMetrics([]string{"extra"})
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			for range 500 {
				m.Status(200 + i%4).Add(1)
			}
		})
	}
	wg.Wait()
	var total int64
	for _, n := range m.StatusCounts() {
		total += n
	}
	if total != 16*500 {
		t.Errorf("counted %d, want %d", total, 16*500)
	}
	if codes := slices.Sorted(maps.Keys(m.StatusCounts())); !slices.Equal(codes, []int{200, 201, 202, 203}) {
		t.Errorf("status codes = %v", codes)
	}
	for _, e := range append([]string{"extra"}, StandardEndpoints...) {
		if m.REST[e] == nil {
			t.Errorf("no REST histogram for %q", e)
		}
	}
}

// ── UI clients ───────────────────────────────────────────────────────────────

// fakeConn is a WSStream replaying scripted envelopes, then blocking.
type fakeConn struct {
	mu         sync.Mutex
	subscribed []string
	since      map[string]uint64
	script     []ws.Envelope
	closed     atomic.Bool
	// breakAfter makes Next fail once the script is used up, as a connection
	// that drops would.
	breakAfter bool
	// firstSub is closed by the first Subscribe. A real connection delivers no
	// push before the client subscribes, so Next waits for it; without the
	// wait the client's reader, which starts before it subscribes, could take
	// the whole script first and make the first subscribe look like a resume.
	firstSub     chan struct{}
	firstSubDone bool
}

// firstSubscribed returns the channel the first Subscribe closes. Caller
// holds f.mu.
func (f *fakeConn) firstSubscribed() chan struct{} {
	if f.firstSub == nil {
		f.firstSub = make(chan struct{})
	}
	return f.firstSub
}

func (f *fakeConn) Subscribe(_ context.Context, subject string, since uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subscribed = append(f.subscribed, subject)
	if f.since == nil {
		f.since = map[string]uint64{}
	}
	f.since[subject] = since
	if !f.firstSubDone {
		f.firstSubDone = true
		close(f.firstSubscribed())
	}
	return nil
}

func (f *fakeConn) Next(ctx context.Context) (ws.Envelope, error) {
	f.mu.Lock()
	ready := f.firstSubscribed()
	f.mu.Unlock()
	select {
	case <-ready:
	case <-ctx.Done():
		return ws.Envelope{}, ctx.Err()
	}
	f.mu.Lock()
	if len(f.script) > 0 {
		env := f.script[0]
		f.script = f.script[1:]
		f.mu.Unlock()
		return env, nil
	}
	f.mu.Unlock()
	if f.breakAfter {
		return ws.Envelope{}, errors.New("connection lost")
	}
	<-ctx.Done()
	return ws.Envelope{}, ctx.Err()
}

func (f *fakeConn) Close() error { f.closed.Store(true); return nil }

func (f *fakeConn) sinceOf(subject string) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.since[subject]
}

func (f *fakeConn) subjects() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.subscribed)
}

// newUIClients returns UI clients for the server at url, watching farm-1.
func newUIClients(url string, spec scenario.ClientsSpec, jobs *JobLedger, m *Metrics, opts ...UIOption) *UIClients {
	return NewUIClients(client.New(url), newFakeTarget(url), spec, jobs, m, opts...)
}

func jobPush(t *testing.T, seq uint64, updated time.Time) ws.Envelope {
	t.Helper()
	p, err := json.Marshal(ws.JobSummaryPush{JobID: "j", Status: "running", UpdatedAt: updated})
	if err != nil {
		t.Fatal(err)
	}
	return ws.Envelope{Type: ws.TypePush, Subject: ws.SubjectJobs, Seq: seq, Payload: p}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestUIClients_NegativeLagCounted: a push stamped after it arrived (a
// server clock ahead of the generator's) is counted, not recorded as a zero
// lag.
func TestUIClients_NegativeLagCounted(t *testing.T) {
	now := time.Now()
	conn := &fakeConn{script: []ws.Envelope{
		jobPush(t, 1, now.Add(-50*time.Millisecond)),
		jobPush(t, 2, now.Add(time.Hour)),
		jobPush(t, 3, now.Add(time.Hour)),
	}}
	m := NewMetrics(nil)
	spec := scenario.ClientsSpec{WebSocket: scenario.WSSpec{Count: 1, Subscribe: []string{SubJobs}}}
	u := newUIClients("http://unused", spec, NewJobLedger(), m,
		WithDial(func(context.Context, string, string) (WSStream, error) { return conn, nil }))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- u.Run(ctx) }()
	waitFor(t, "three events", func() bool { return m.WSEvents.Load() == 3 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := m.WSLagNegative.Load(); got != 2 {
		t.Errorf("WSLagNegative = %d, want 2", got)
	}
	if got := m.WSLag.Count(); got != 1 {
		t.Errorf("WSLag has %d samples, want only the one in the past", got)
	}
}

func TestUIClients_DetectsSeqGap(t *testing.T) {
	now := time.Now()
	conn := &fakeConn{script: []ws.Envelope{
		jobPush(t, 1, now.Add(-50*time.Millisecond)),
		jobPush(t, 2, now.Add(-50*time.Millisecond)),
		jobPush(t, 5, now.Add(-50*time.Millisecond)),
	}}
	m := NewMetrics(nil)
	spec := scenario.ClientsSpec{WebSocket: scenario.WSSpec{Count: 1, Subscribe: []string{SubJobs}}}
	u := newUIClients("http://unused", spec, NewJobLedger(), m,
		WithDial(func(context.Context, string, string) (WSStream, error) { return conn, nil }))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- u.Run(ctx) }()
	waitFor(t, "three events", func() bool { return m.WSEvents.Load() == 3 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := m.WSDrops.Load(); got != 2 {
		t.Errorf("WSDrops = %d, want 2 (seq 1,2,5 misses 3 and 4)", got)
	}
	if got := m.WSEvents.Load(); got != 3 {
		t.Errorf("WSEvents = %d, want 3", got)
	}
	if got := m.WSLag.Count(); got != 3 {
		t.Errorf("WSLag has %d samples, want 3", got)
	}
	if lag := m.WSLag.Min(); lag < 40*time.Millisecond {
		t.Errorf("lag min = %v, want at least the 50 ms the payload was old", lag)
	}
	if !conn.closed.Load() {
		t.Error("connection not closed when the run ended")
	}
	if got := conn.subjects(); !slices.Equal(got, []string{SubJobs}) {
		t.Errorf("subscribed to %v, want [jobs]", got)
	}
	if got := conn.sinceOf(SubJobs); got != noReplay {
		t.Errorf("first subscribe used since_seq %d, want the no-replay cursor", got)
	}
}

func TestSeqTracker(t *testing.T) {
	tests := []struct {
		name string
		seqs []uint64
		want int64
	}{
		{"contiguous", []uint64{1, 2, 3, 4}, 0},
		{"late joiner baseline", []uint64{500, 501, 502}, 0},
		{"one gap", []uint64{1, 2, 4}, 1},
		{"1,2,5", []uint64{1, 2, 5}, 2},
		{"two gaps add up", []uint64{1, 3, 7}, 1 + 3},
		{"duplicate ignored", []uint64{1, 2, 2, 3}, 0},
		{"a lower number within a session is late, not a restart", []uint64{10, 11, 1, 2}, 0},
		{"late push fills the hole it left", []uint64{1, 5, 3, 2, 4}, 0},
		{"late push fills only part of the hole", []uint64{1, 5, 3}, 3 - 1},
		{"a hole is filled once", []uint64{1, 5, 3, 3}, 3 - 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := newSeqTracker()
			var total int64
			for _, s := range tc.seqs {
				total += tr.observe("jobs", s)
			}
			if total != tc.want {
				t.Errorf("gaps = %d, want %d", total, tc.want)
			}
		})
	}
	t.Run("subjects are independent", func(t *testing.T) {
		tr := newSeqTracker()
		tr.observe("jobs", 1)
		tr.observe("workers", 7)
		if g := tr.observe("jobs", 2) + tr.observe("workers", 8); g != 0 {
			t.Errorf("gaps = %d across independent subjects", g)
		}
	})
}

func TestSeqTracker_SubscribeCursor(t *testing.T) {
	tr := newSeqTracker()
	if got := tr.subscribeCursor("jobs"); got != noReplay {
		t.Errorf("cursor for an unseen subject = %d, want noReplay", got)
	}
	tr.observe("jobs", 7)
	tr.observe("jobs", 8)
	if got := tr.subscribeCursor("jobs"); got != 8 {
		t.Errorf("cursor after seeing 8 = %d, want 8", got)
	}
	// Live 12 arrives before the replay of 9..11: net zero drops.
	var net int64
	for _, seq := range []uint64{12, 9, 10, 11} {
		net += tr.observe("jobs", seq)
	}
	if net != 0 {
		t.Errorf("live-before-replay left %d drops, want 0", net)
	}
	// A restarted server numbers from 1 again: at or below the cursor.
	tr2 := newSeqTracker()
	tr2.observe("jobs", 50)
	tr2.subscribeCursor("jobs")
	if d := tr2.observe("jobs", 1) + tr2.observe("jobs", 2) + tr2.observe("jobs", 4); d != 1 {
		t.Errorf("after a counter restart drops = %d, want only the one hole (3)", d)
	}
}

func TestEventTime(t *testing.T) {
	ts := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	enc := func(v any) json.RawMessage {
		b, _ := json.Marshal(v) //nolint:errcheck // test data
		return b
	}
	tests := []struct {
		name    string
		payload json.RawMessage
		want    time.Time
	}{
		{"job summary", enc(ws.JobSummaryPush{UpdatedAt: ts}), ts},
		{"task update", enc(ws.TaskUpdatePush{UpdatedAt: ts}), ts},
		{"log chunk", enc(ws.TaskLogPush{At: ts}), ts},
		{"log chunk quoting a key", enc(ws.TaskLogPush{Data: `{"at":"2020-01-01T00:00:00Z"}`, At: ts}), ts},
		{"zero time", enc(ws.JobSummaryPush{}), time.Time{}},
		{"worker status has none", enc(ws.WorkerStatusPush{WorkerID: "w"}), time.Time{}},
		{"garbage", json.RawMessage(`nope`), time.Time{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := eventTime(tc.payload); !got.Equal(tc.want) {
				t.Errorf("eventTime = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUIClients_SubscribesOwnJobAndTaskLogs(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/tasks") {
			fmt.Fprint(w, `{"items":[{"id":"task-9","status":"ready"}],"total":1}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	conn := &fakeConn{}
	ledger := NewJobLedger()
	ledger.Record(SubmittedJob{ID: "job-7", Status: 201})
	m := NewMetrics(nil)
	spec := scenario.ClientsSpec{WebSocket: scenario.WSSpec{Count: 1}} // every subscription
	u := newUIClients(ts.URL, spec, ledger, m,
		WithDial(func(context.Context, string, string) (WSStream, error) { return conn, nil }))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- u.Run(ctx) }()
	want := []string{"jobs", "workers", "jobs/job-7/tasks", "tasks/task-9/logs"}
	waitFor(t, "four subscriptions", func() bool { return slices.Equal(conn.subjects(), want) })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestUIClients_ReconnectReplayDoesNotInflateDropsOrLag loses a connection
// after pushes 1..10, redials, and receives a live push (14) ahead of the
// replay of 11..13, the order the server's subscribe-then-replay allows.
func TestUIClients_ReconnectReplayDoesNotInflateDropsOrLag(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	live := time.Now().Add(time.Minute) // after the resubscription, whatever the test's speed
	var first []ws.Envelope
	for seq := uint64(1); seq <= 10; seq++ {
		first = append(first, jobPush(t, seq, live))
	}
	conns := []*fakeConn{
		{script: first, breakAfter: true},
		{script: []ws.Envelope{jobPush(t, 14, live), jobPush(t, 11, old), jobPush(t, 12, old), jobPush(t, 13, old)}},
	}
	var dials atomic.Int32
	m := NewMetrics(nil)
	spec := scenario.ClientsSpec{WebSocket: scenario.WSSpec{Count: 1, Subscribe: []string{SubJobs}}}
	u := newUIClients("http://unused", spec, NewJobLedger(), m,
		WithDial(func(context.Context, string, string) (WSStream, error) {
			n := int(dials.Add(1))
			if n > len(conns) {
				return nil, errors.New("unexpected extra dial")
			}
			return conns[n-1], nil
		}))
	u.retryDelay = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- u.Run(ctx) }()
	waitFor(t, "fourteen events", func() bool { return m.WSEvents.Load() == 14 })
	cancel()
	<-done

	if got := conns[0].sinceOf(SubJobs); got != noReplay {
		t.Errorf("first connection subscribed with since_seq %d, want the no-replay cursor", got)
	}
	if got := conns[1].sinceOf(SubJobs); got != 10 {
		t.Errorf("reconnect subscribed with since_seq %d, want 10 (the last seen)", got)
	}
	if got := m.WSDrops.Load(); got != 0 {
		t.Errorf("WSDrops = %d, want 0: the replayed pushes fill the hole the live one opened", got)
	}
	// The live pushes are stamped in the future so they pass the resubscribe
	// cutoff whatever the test's speed, so they count as negative lags.
	if got := m.WSLag.Count() + m.WSLagNegative.Load(); got != 11 {
		t.Errorf("%d lag samples, want 11 (ten before the loss and the live push, none for the replay)", got)
	}
	if got := m.WSErrors.Load(); got != 1 {
		t.Errorf("WSErrors = %d, want 1 for the lost connection", got)
	}
}

func TestUIClients_RefusedDialIsCounted(t *testing.T) {
	var dials atomic.Int32
	m := NewMetrics(nil)
	spec := scenario.ClientsSpec{WebSocket: scenario.WSSpec{Count: 1, Subscribe: []string{SubJobs}}}
	u := newUIClients("http://unused", spec, NewJobLedger(), m,
		WithDial(func(context.Context, string, string) (WSStream, error) {
			if dials.Add(1) == 1 {
				return nil, errors.New("refused")
			}
			return &fakeConn{script: []ws.Envelope{jobPush(t, 4, time.Now())}}, nil
		}))
	u.retryDelay = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- u.Run(ctx) }()
	waitFor(t, "an event on the second connection", func() bool { return m.WSEvents.Load() == 1 })
	cancel()
	<-done
	if got := m.WSErrors.Load(); got != 1 {
		t.Errorf("WSErrors = %d, want 1 for the refused dial", got)
	}
}

// TestUIClients_DefaultDialUsesTheRESTClientsTLS connects over wss:// to a
// server only the REST client's http.Client trusts, without WithDial.
func TestUIClients_DefaultDialUsesTheRESTClientsTLS(t *testing.T) {
	var subscribed atomic.Bool
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow() //nolint:errcheck // test handler
		_, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var env ws.Envelope
		if json.Unmarshal(data, &env) != nil {
			return
		}
		payload, _ := json.Marshal(ws.AckPayload{ClientSeq: env.Seq})           //nolint:errcheck // test data
		out, _ := json.Marshal(ws.Envelope{Type: ws.TypeAck, Payload: payload}) //nolint:errcheck // test data
		_ = conn.Write(r.Context(), websocket.MessageText, out)                 //nolint:errcheck // test handler
		subscribed.Store(true)
		<-r.Context().Done()
	}))
	defer ts.Close()

	m := NewMetrics(nil)
	spec := scenario.ClientsSpec{WebSocket: scenario.WSSpec{Count: 1, Subscribe: []string{SubJobs}}}
	u := NewUIClients(client.New(ts.URL, client.WithHTTPClient(ts.Client())), newFakeTarget(ts.URL), spec, NewJobLedger(), m)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- u.Run(ctx) }()
	waitFor(t, "a subscription over TLS", subscribed.Load)
	cancel()
	<-done
	if got := m.WSErrors.Load(); got != 0 {
		t.Errorf("WSErrors = %d, want 0", got)
	}
}

func TestUIClients_UnknownNamesFailUpFront(t *testing.T) {
	m := NewMetrics(nil)
	for _, spec := range []scenario.ClientsSpec{
		{WebSocket: scenario.WSSpec{Count: 1, Subscribe: []string{"bogus"}}},
		{RESTPollers: scenario.PollSpec{Count: 1, Endpoints: []string{"bogus"}}},
	} {
		if err := newUIClients("http://unused", spec, NewJobLedger(), m).Run(t.Context()); err == nil {
			t.Errorf("Run accepted %+v", spec)
		}
	}
}

func TestUIClients_RESTPollers(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	var failJobs atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/jobs" && failJobs.Load():
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"detail":"busy"}`)
		case r.URL.Path == "/api/v1/jobs", r.URL.Path == "/api/v1/workers":
			fmt.Fprint(w, `{"items":[],"total":0}`)
		case strings.HasSuffix(r.URL.Path, "/tasks"):
			fmt.Fprint(w, `{"items":[],"total":0}`)
		default:
			fmt.Fprint(w, `{"id":"job-1","status":"running"}`)
		}
	}))
	defer ts.Close()

	count := func(path string) int {
		mu.Lock()
		defer mu.Unlock()
		return hits[path]
	}
	ledger := NewJobLedger()
	m := NewMetrics(nil)
	spec := scenario.ClientsSpec{RESTPollers: scenario.PollSpec{
		Count: 2, Interval: scenario.Duration{Duration: 20 * time.Millisecond},
	}}
	u := newUIClients(ts.URL, spec, ledger, m)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- u.Run(ctx) }()

	// Before any job is accepted only the two list endpoints can be polled.
	waitFor(t, "list polls", func() bool { return count("/api/v1/jobs") >= 3 && count("/api/v1/workers") >= 3 })
	if n := count("/api/v1/jobs/job-1"); n != 0 {
		t.Errorf("job-detail polled %d times before a job was accepted", n)
	}
	ledger.Record(SubmittedJob{ID: "job-1", Status: 201})
	waitFor(t, "job polls", func() bool { return count("/api/v1/jobs/job-1") >= 2 && count("/api/v1/jobs/job-1/tasks") >= 2 })
	failJobs.Store(true)
	waitFor(t, "errors", func() bool { return m.RESTErrors.Load() >= 2 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, e := range StandardEndpoints {
		if m.REST[e].Count() == 0 {
			t.Errorf("no latency recorded for %s", e)
		}
	}
}

// ── Actions ──────────────────────────────────────────────────────────────────

// opServer records the mutating calls an action makes.
type opServer struct {
	mu    sync.Mutex
	calls []string
	jobs  string // JSON for GET /jobs
	works string // JSON for GET /workers
}

func (o *opServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			switch r.URL.Path {
			case "/api/v1/jobs":
				fmt.Fprint(w, o.jobs)
			case "/api/v1/workers":
				fmt.Fprint(w, o.works)
			default:
				if strings.HasPrefix(r.URL.Path, "/api/v1/queues/") {
					fmt.Fprint(w, `{"name":"q"}`) // PauseQueue reads the queue, then puts it back paused
					return
				}
				http.NotFound(w, r)
			}
			return
		}
		call := r.Method + " " + strings.TrimPrefix(r.URL.Path, "/api/v1/")
		if r.Method == http.MethodPut {
			var q struct {
				Paused bool `json:"paused"`
			}
			json.NewDecoder(r.Body).Decode(&q) //nolint:errcheck // test server
			call += fmt.Sprintf(" paused=%v", q.Paused)
		}
		o.mu.Lock()
		o.calls = append(o.calls, call)
		o.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func (o *opServer) made() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.calls)
}

func ledgerOf(n int) *JobLedger {
	l := NewJobLedger()
	for i := range n {
		l.Record(SubmittedJob{ID: fmt.Sprintf("job-%d", i), Status: 201})
	}
	return l
}

func runActions(t *testing.T, ts *httptest.Server, acts []scenario.Action, l *JobLedger, fleet *simworker.Fleet) *Actions {
	t.Helper()
	a := NewActions(client.New(ts.URL), newFakeTarget(ts.URL), acts, l, fleet, WithActionsRand(rand.New(rand.NewPCG(3, 4))))
	if err := a.Run(t.Context(), time.Now()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return a
}

func fraction(f float64) *scenario.Fraction { return &scenario.Fraction{Fraction: f} }

func TestActions_CancelFraction(t *testing.T) {
	tests := []struct {
		name     string
		fraction float64
		jobs     int
		want     int
	}{
		{"half of ten", 0.5, 10, 5},
		{"floor", 0.35, 10, 3},
		{"binary rounding must not lose one", 0.29, 100, 29},
		{"none", 0, 10, 0},
		{"all", 1, 10, 10},
		{"no jobs", 0.5, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := &opServer{}
			ts := o.start(t)
			a := runActions(t, ts, []scenario.Action{{CancelJobs: fraction(tc.fraction)}}, ledgerOf(tc.jobs), nil)

			seen := map[string]bool{}
			for _, c := range o.made() {
				if !strings.HasSuffix(c, "/cancel") || !strings.HasPrefix(c, "POST jobs/") {
					t.Errorf("unexpected call %q", c)
				}
				if seen[c] {
					t.Errorf("%q made twice", c)
				}
				seen[c] = true
			}
			if len(seen) != tc.want {
				t.Errorf("canceled %d distinct jobs, want %d", len(seen), tc.want)
			}
			recs := a.Records()
			if len(recs) != 1 || recs[0].Kind != ActionCancelJobs || recs[0].Targeted != tc.want || recs[0].Succeeded != tc.want {
				t.Fatalf("records = %+v", recs)
			}
			// The record names every job it set out to cancel, so the verifier
			// can tell a job the run canceled from one the server canceled.
			if len(recs[0].JobIDs) != tc.want {
				t.Errorf("record names %d jobs, want %d", len(recs[0].JobIDs), tc.want)
			}
			for _, id := range recs[0].JobIDs {
				if !seen["POST jobs/"+id+"/cancel"] {
					t.Errorf("record names job %q, which was not canceled", id)
				}
			}
		})
	}
}

func TestActions_CancelFractionIsSeeded(t *testing.T) {
	pick := func(seed uint64) []string {
		o := &opServer{}
		ts := o.start(t)
		a := NewActions(client.New(ts.URL), newFakeTarget(ts.URL),
			[]scenario.Action{{CancelJobs: fraction(0.5)}}, ledgerOf(20), nil, WithActionsRand(rand.New(rand.NewPCG(seed, 1))))
		if err := a.Run(t.Context(), time.Now()); err != nil {
			t.Fatal(err)
		}
		calls := o.made()
		slices.Sort(calls)
		return calls
	}
	if a, b := pick(11), pick(11); !slices.Equal(a, b) {
		t.Errorf("same seed chose different jobs:\n%v\n%v", a, b)
	}
	if a, b := pick(11), pick(12); slices.Equal(a, b) {
		t.Error("different seeds chose the same jobs")
	}
}

func TestActions_RetryFailedRetriesOnlyOwnFailedJobs(t *testing.T) {
	o := &opServer{jobs: `{"items":[
		{"id":"job-0","status":"failed"},{"id":"job-1","status":"completed"},
		{"id":"job-2","status":"failed"},{"id":"stranger","status":"failed"}],"total":4}`}
	ts := o.start(t)
	a := runActions(t, ts, []scenario.Action{{RetryFailed: true}}, ledgerOf(3), nil)

	calls := o.made()
	slices.Sort(calls)
	want := []string{"POST jobs/job-0/retry", "POST jobs/job-2/retry"}
	if !slices.Equal(calls, want) {
		t.Errorf("calls = %v, want %v", calls, want)
	}
	if recs := a.Records(); len(recs) != 1 || recs[0].Succeeded != 2 {
		t.Errorf("records = %+v", recs)
	}
}

func TestActions_PauseQueue(t *testing.T) {
	o := &opServer{}
	ts := o.start(t)
	a := runActions(t, ts, []scenario.Action{{PauseQueue: "other"}}, NewJobLedger(), nil)
	calls := o.made()
	if len(calls) != 1 || !strings.Contains(calls[0], "queues/queue-2") {
		t.Errorf("calls = %v, want one call on queue-2", calls)
	}
	if recs := a.Records(); len(recs) != 1 || recs[0].Kind != "pause_queue" || !strings.Contains(recs[0].Detail, "other") {
		t.Errorf("records = %+v", recs)
	}
}

func TestActions_PauseUnknownQueueFailsBeforeWaiting(t *testing.T) {
	o := &opServer{}
	ts := o.start(t)
	a := NewActions(client.New(ts.URL), newFakeTarget(ts.URL), []scenario.Action{{PauseQueue: "nope"}}, NewJobLedger(), nil)
	if err := a.Run(t.Context(), time.Now().Add(time.Hour)); err == nil {
		t.Fatal("Run accepted an unknown queue")
	}
}

func TestActions_DisableWorkersFraction(t *testing.T) {
	o := &opServer{works: `{"items":[{"id":"w1"},{"id":"w2"},{"id":"w3"},{"id":"w4"}],"total":4}`}
	ts := o.start(t)
	a := runActions(t, ts, []scenario.Action{{DisableWorkers: fraction(0.5)}}, NewJobLedger(), nil)
	calls := o.made()
	if len(calls) != 2 {
		t.Fatalf("calls = %v, want 2 disables", calls)
	}
	for _, c := range calls {
		if !strings.HasPrefix(c, "POST workers/w") || !strings.HasSuffix(c, "/disable") {
			t.Errorf("unexpected call %q", c)
		}
	}
	if recs := a.Records(); len(recs) != 1 || recs[0].Succeeded != 2 || recs[0].Failed != 0 {
		t.Errorf("records = %+v", recs)
	}
}

func TestActions_RefusedCallsAreCountedNotFatal(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"detail":"already finished"}`)
	}))
	defer ts.Close()
	a := runActions(t, ts, []scenario.Action{{CancelJobs: fraction(1)}}, ledgerOf(3), nil)
	recs := a.Records()
	if len(recs) != 1 || recs[0].Failed != 3 || recs[0].Succeeded != 0 || !strings.Contains(recs[0].Detail, "already finished") {
		t.Errorf("records = %+v, want 3 failures naming the server's reason", recs)
	}
}

func TestActions_FireAtTheirOffsetsAndStopWithContext(t *testing.T) {
	o := &opServer{}
	ts := o.start(t)
	start := time.Now()
	acts := []scenario.Action{
		{At: scenario.Duration{Duration: 50 * time.Millisecond}, CancelJobs: fraction(1)},
		{At: scenario.Duration{Duration: time.Hour}, CancelJobs: fraction(1)},
	}
	a := NewActions(client.New(ts.URL), newFakeTarget(ts.URL), acts, ledgerOf(2), nil)
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	if err := a.Run(ctx, start); err != nil {
		t.Fatalf("Run: %v", err)
	}
	recs := a.Records()
	if len(recs) != 1 {
		t.Fatalf("%d actions fired, want only the first", len(recs))
	}
	if d := recs[0].Started.Sub(start); d < 50*time.Millisecond {
		t.Errorf("fired %v after start, want at least 50 ms", d)
	}
}

// TestSubmitter_LiveRequestHasNoTimeLimit answers slower than finishTimeout
// while the run's ctx is live: the submission must still be accepted.
func TestSubmitter_LiveRequestHasNoTimeLimit(t *testing.T) {
	defer func(old time.Duration) { finishTimeout = old }(finishTimeout)
	finishTimeout = 50 * time.Millisecond
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"slow-1"}`)
	}))
	defer ts.Close()
	s, ledger, m := newSubmitter(t, ts, &scenario.Scenario{Jobs: oneClass(1, 0)})
	if err := s.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := m.StatusCounts(); got[201] != 1 || len(got) != 1 {
		t.Errorf("statuses = %v, want one 201 (a slow answer in a live run is not a failure)", got)
	}
	if ledger.AcceptedCount() != 1 {
		t.Errorf("accepted = %d, want 1: the job exists on the server", ledger.AcceptedCount())
	}
}

// TestSubmitter_AfterTheRunEndsARequestGetsFinishTimeout hangs the server and
// ends the run: the request is abandoned after finishTimeout and counted as a
// transport failure, so the run does not hang.
func TestSubmitter_AfterTheRunEndsARequestGetsFinishTimeout(t *testing.T) {
	defer func(old time.Duration) { finishTimeout = old }(finishTimeout)
	finishTimeout = 150 * time.Millisecond
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer ts.Close()
	defer close(release)
	s, _, m := newSubmitter(t, ts, &scenario.Scenario{Jobs: oneClass(0, 50)})

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := s.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("Run took %v, want about the run plus finishTimeout", took)
	}
	if m.StatusCounts()[0] == 0 {
		t.Errorf("statuses = %v, want the abandoned requests counted under 0", m.StatusCounts())
	}
}

func TestSubmitter_UndecodableSuccessIsCountedUnderItsOwnKey(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `not json`)
	}))
	defer ts.Close()
	s, ledger, m := newSubmitter(t, ts, &scenario.Scenario{Jobs: oneClass(2, 0)})
	if err := s.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := m.StatusCounts(); got[StatusUndecodable] != 2 || got[201] != 0 {
		t.Errorf("statuses = %v, want two under StatusUndecodable and none under 201", got)
	}
	for _, j := range ledger.All() {
		if j.Status != StatusUndecodable || j.Accepted() {
			t.Errorf("ledger entry %+v, want Status StatusUndecodable and not accepted", j)
		}
	}
}

func TestActions_ResumeQueue(t *testing.T) {
	o := &opServer{}
	ts := o.start(t)
	a := runActions(t, ts, []scenario.Action{{PauseQueue: "other"}, {At: scenario.Duration{Duration: 10 * time.Millisecond}, ResumeQueue: "other"}}, NewJobLedger(), nil)
	if calls := o.made(); !slices.Equal(calls, []string{"PUT queues/queue-2 paused=true", "PUT queues/queue-2 paused=false"}) {
		t.Errorf("calls = %v, want a pause and then a resume of queue-2", calls)
	}
	if got := a.ResumePaused(t.Context()); len(got) != 0 {
		t.Errorf("ResumePaused resumed %v, want nothing: the scenario resumed its queue itself", got)
	}
	if n := len(o.made()); n != 2 {
		t.Errorf("%d calls after ResumePaused, want still 2", n)
	}
}

// TestActions_ResumePaused checks that a queue the scenario paused and never
// resumed is resumed when ResumePaused is called, as the runner does before
// the drain: a paused queue leases nothing, so its jobs could never finish.
func TestActions_ResumePaused(t *testing.T) {
	o := &opServer{}
	ts := o.start(t)
	a := runActions(t, ts, []scenario.Action{{PauseQueue: "other"}, {PauseQueue: "q"}}, NewJobLedger(), nil)
	got := a.ResumePaused(t.Context())
	if !slices.Equal(got, []string{"other", "q"}) {
		t.Errorf("ResumePaused = %v, want both paused queues", got)
	}
	calls := o.made()
	for _, want := range []string{"PUT queues/queue-1 paused=false", "PUT queues/queue-2 paused=false"} {
		if !slices.Contains(calls, want) {
			t.Errorf("calls %v lack %q", calls, want)
		}
	}
	var resumes int
	for _, r := range a.Records() {
		if r.Kind == "resume_queue" && strings.Contains(r.Detail, "before the drain") && r.Succeeded == 1 {
			resumes++
		}
	}
	if resumes != 2 {
		t.Errorf("records %+v, want two automatic resume_queue records", a.Records())
	}
	if again := a.ResumePaused(t.Context()); len(again) != 0 {
		t.Errorf("a second ResumePaused resumed %v again", again)
	}
}
