// SPDX-License-Identifier: AGPL-3.0-or-later

package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/target"
	"github.com/uberware/sqi/internal/ws"
)

// Subscriptions a WebSocket client can ask for in scenario.WSSpec.Subscribe.
const (
	SubJobs        = "jobs"
	SubWorkers     = "workers"
	SubOwnJobTasks = "own-job-tasks"
	SubOwnTaskLogs = "own-task-logs"
)

const (
	// pollPageSize is the page size of the list endpoints a poller hits.
	pollPageSize = 50
	// ownJobWindow is how many of the most recent accepted jobs a client picks
	// its own job from.
	ownJobWindow = 20
	// ownRetryEvery is how often a client without its own job looks for one.
	ownRetryEvery = 500 * time.Millisecond
	// reconnectDelay is the pause before a lost connection is redialed.
	reconnectDelay = time.Second
	// maxParallelDial bounds the WebSocket dials in progress at once, so
	// starting a thousand clients is a ramp, not a thundering herd.
	maxParallelDial = 64
)

var standardSubscriptions = []string{SubJobs, SubWorkers, SubOwnJobTasks, SubOwnTaskLogs}

// WSStream is the part of a WebSocket connection a UI client uses.
// *client.WSConn implements it.
type WSStream interface {
	Subscribe(ctx context.Context, subject string, sinceSeq uint64) error
	Next(ctx context.Context) (ws.Envelope, error)
	Close() error
}

// DialFunc opens a WebSocket to the server at baseURL.
type DialFunc func(ctx context.Context, baseURL, apiKey string) (WSStream, error)

// dialWith returns a DialFunc that dials with hc, so the WebSocket trusts the
// same CA as the REST client.
func dialWith(hc *http.Client) DialFunc {
	return func(ctx context.Context, baseURL, apiKey string) (WSStream, error) {
		c, err := client.DialWS(ctx, baseURL, apiKey, client.WithWSHTTPClient(hc))
		if err != nil {
			return nil, err
		}
		return c, nil
	}
}

// UIClients is the read-side load: WebSocket subscribers and REST pollers,
// the way browsers and scripts watch a farm. WebSocket clients speak the real
// protocol, drain their connection continuously, measure the lag between a
// push's own timestamp and its arrival, and count holes in the server's
// per-subject sequence numbers as drops. Failures are counted in
// Metrics.WSErrors and Metrics.RESTErrors, never hidden.
type UIClients struct {
	baseURL, apiKey string
	spec            scenario.ClientsSpec
	jobs            *JobLedger
	m               *Metrics

	c      *client.Client
	farmID string
	dial   DialFunc
	rng    *syncRand
	// retryDelay is the wait before a lost connection is redialed.
	retryDelay time.Duration
	// initErr is why an option failed; Run returns it.
	initErr error
}

// UIOption configures NewUIClients.
type UIOption func(*UIClients)

// WithUIClient sets the REST client the pollers and the own-job lookups use,
// for example one that trusts the target's CA. By default one is built from
// the base URL and API key.
func WithUIClient(c *client.Client) UIOption { return func(u *UIClients) { u.c = c } }

// WithUITarget points the clients at t: its REST client (carrying the API key
// and trusting its CA, which the WebSocket dial reuses) and its farm. Use it
// rather than WithUIClient and WithUIFarm when a target is at hand, so TLS is
// never forgotten.
func WithUITarget(t target.Target) UIOption {
	return func(u *UIClients) {
		// No retries: a poller's throttled call is an error it counts.
		c, err := target.NewClient(t, client.WithRetry(0))
		if err != nil {
			u.initErr = err
			return
		}
		u.c = c
		u.farmID = t.FarmID()
	}
}

// WithUIFarm limits job, task and worker lists to one farm.
func WithUIFarm(farmID string) UIOption { return func(u *UIClients) { u.farmID = farmID } }

// WithDial replaces the WebSocket dialer (tests).
func WithDial(d DialFunc) UIOption { return func(u *UIClients) { u.dial = d } }

// WithUIRand seeds the clients' random choices; without it a fixed seed is used.
func WithUIRand(r *rand.Rand) UIOption { return func(u *UIClients) { u.rng = newSyncRand(r) } }

// NewUIClients returns UI clients for the server at baseURL.
func NewUIClients(baseURL, apiKey string, spec scenario.ClientsSpec, jobs *JobLedger, m *Metrics, opts ...UIOption) *UIClients {
	u := &UIClients{
		baseURL: baseURL, apiKey: apiKey, spec: spec, jobs: jobs, m: m,
		rng:        newSyncRand(nil),
		retryDelay: reconnectDelay,
	}
	for _, o := range opts {
		o(u)
	}
	if u.c == nil {
		var copts []client.Option
		if apiKey != "" {
			copts = append(copts, client.WithAPIKey(apiKey))
		}
		u.c = client.New(baseURL, copts...)
	}
	if u.dial == nil {
		u.dial = dialWith(u.c.HTTPClient())
	}
	return u
}

// Run starts every client and returns once ctx has ended and all of them have
// stopped. It fails up front only for an unknown subscription or endpoint.
func (u *UIClients) Run(ctx context.Context) error {
	if u.initErr != nil {
		return u.initErr
	}
	subs, err := chooseNames("subscription", u.spec.WebSocket.Subscribe, standardSubscriptions)
	if err != nil {
		return err
	}
	endpoints, err := chooseNames("endpoint", u.spec.RESTPollers.Endpoints, StandardEndpoints)
	if err != nil {
		return err
	}

	// Each goroutine gets its own generator, drawn here in order, so the
	// choices a run makes do not depend on goroutine scheduling.
	dialSem := make(chan struct{}, maxParallelDial)
	var wg sync.WaitGroup
	for range max(u.spec.WebSocket.Count, 0) {
		rng := u.rng.child()
		wg.Go(func() { u.wsClient(ctx, rng, subs, dialSem) })
	}
	for range max(u.spec.RESTPollers.Count, 0) {
		rng := u.rng.child()
		wg.Go(func() { u.poller(ctx, rng, endpoints) })
	}
	wg.Wait()
	return nil
}

// chooseNames validates names against known, and returns known when names is
// empty.
func chooseNames(kind string, names, known []string) ([]string, error) {
	if len(names) == 0 {
		return known, nil
	}
	for _, n := range names {
		if !slices.Contains(known, n) {
			return nil, fmt.Errorf("driver: unknown %s %q (known: %v)", kind, n, known)
		}
	}
	return names, nil
}

// sleepFor waits d or until ctx ends, and reports whether d elapsed.
func sleepFor(ctx context.Context, d time.Duration) bool {
	return sleepUntil(ctx, time.Now().Add(d))
}

// ── WebSocket clients ────────────────────────────────────────────────────────

// noReplay is the since_seq that subscribes without replaying the hub's
// buffered pushes (the hub treats it as past every sequence number). A client
// that has seen nothing on a subject asks for it: the buffer holds up to 256
// old pushes, which the client's 64-slot queue would drop, and they would show
// up as drops and as lag that never happened.
const noReplay = ^uint64(0)

// maxTrackedHoles bounds the missing sequence numbers remembered per subject.
const maxTrackedHoles = 1024

// subjectSeqs is the sequence state of one subject.
type subjectSeqs struct {
	last uint64
	// cursor is the since_seq the current connection subscribed with; 0 when
	// it asked for no replay or has already seen the server's counter restart.
	cursor  uint64
	missing map[uint64]struct{}
}

// seqTracker finds holes in the per-subject sequence numbers a client sees.
// It is safe for concurrent use.
type seqTracker struct {
	mu    sync.Mutex
	state map[string]*subjectSeqs
}

func newSeqTracker() *seqTracker { return &seqTracker{state: map[string]*subjectSeqs{}} }

func (t *seqTracker) get(subject string) *subjectSeqs {
	st := t.state[subject]
	if st == nil {
		st = &subjectSeqs{missing: map[uint64]struct{}{}}
		t.state[subject] = st
	}
	return st
}

// subscribeCursor returns the since_seq a new connection should subscribe to
// subject with, and records it as the connection's cursor: noReplay for a
// subject never seen, else the last sequence number seen, so the server
// replays only what the client missed.
func (t *seqTracker) subscribeCursor(subject string) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.get(subject)
	if st.last == 0 {
		st.cursor = 0
		return noReplay
	}
	st.cursor = st.last
	return st.last
}

// observe records seq as received on subject and returns the change in the
// number of missed pushes: positive for a hole it opens, -1 for a late push
// that fills one, 0 otherwise. The first push on a subject sets the baseline:
// the server numbers a subject across every client, so a late subscriber starts
// anywhere. The server registers a subscription before it replays, so a live
// push can arrive ahead of replayed ones; the replayed ones then fill the hole
// the live one opened, and the net count is zero. A push at or below the
// cursor the connection subscribed with cannot be a replay, so the server's
// counter restarted and that push is the new baseline.
func (t *seqTracker) observe(subject string, seq uint64) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.get(subject)
	switch {
	case st.last == 0:
		st.last = seq
		return 0
	case st.cursor > 0 && seq <= st.cursor:
		st.last, st.cursor = seq, 0
		clear(st.missing)
		return 0
	case seq > st.last:
		gap := seq - st.last - 1
		if gap > 0 && gap <= maxTrackedHoles && len(st.missing)+int(gap) <= maxTrackedHoles {
			for m := st.last + 1; m < seq; m++ {
				st.missing[m] = struct{}{}
			}
		}
		st.last = seq
		return safeInt64(gap)
	}
	if _, ok := st.missing[seq]; ok {
		delete(st.missing, seq)
		return -1
	}
	return 0
}

// wsState is what one UI client keeps across reconnects.
type wsState struct {
	seqs *seqTracker

	mu       sync.Mutex
	subjects []string // subscribed on every connection, in order
	ownJob   bool
	ownTask  bool
	// replayCutoff is, per resubscribed subject, the time the resubscription
	// was sent: a push whose own timestamp is older was buffered before it, so
	// it is a replay, and its age says nothing about the server's latency.
	replayCutoff map[string]time.Time
}

func (s *wsState) setCutoff(subject string, t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.replayCutoff == nil {
		s.replayCutoff = map[string]time.Time{}
	}
	s.replayCutoff[subject] = t
}

func (s *wsState) cutoff(subject string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.replayCutoff[subject]
}

func (s *wsState) add(subject string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Contains(s.subjects, subject) {
		s.subjects = append(s.subjects, subject)
	}
}

func (s *wsState) snapshot() (subjects []string, ownJob, ownTask bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.subjects), s.ownJob, s.ownTask
}

// wsClient keeps one connection alive for the life of the run, redialing with
// a pause after each loss. A lost connection counts in WSErrors.
func (u *UIClients) wsClient(ctx context.Context, rng *rand.Rand, subs []string, dialSem chan struct{}) {
	st := &wsState{seqs: newSeqTracker()}
	for _, s := range subs {
		switch s {
		case SubJobs, SubWorkers:
			st.add(s)
		}
	}
	for ctx.Err() == nil {
		err := u.wsSession(ctx, rng, st, subs, dialSem)
		if ctx.Err() != nil {
			return
		}
		u.m.WSErrors.Add(1)
		_ = err // the count is the record; a per-error log line would drown the run at 1,000 clients
		if !sleepFor(ctx, u.retryDelay) {
			return
		}
	}
}

// wsSession is one connection: dial, subscribe, drain until it breaks or ctx
// ends. The drain starts before the first subscribe, because the connection's
// reader stalls (no pongs, no acks) once its inbox is full.
func (u *UIClients) wsSession(ctx context.Context, rng *rand.Rand, st *wsState, subs []string, dialSem chan struct{}) error {
	conn, err := u.dialLimited(ctx, dialSem)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	drained := make(chan error, 1)
	go func() { drained <- u.drain(ctx, conn, st) }()

	known, _, _ := st.snapshot()
	for _, subject := range known {
		since := st.seqs.subscribeCursor(subject)
		if since != noReplay {
			st.setCutoff(subject, time.Now())
		}
		if err := conn.Subscribe(ctx, subject, since); err != nil {
			cancel()
			<-drained
			return err
		}
	}

	if !wantsOwn(subs) {
		return <-drained
	}
	tick := time.NewTicker(ownRetryEvery)
	defer tick.Stop()
	for {
		select {
		case err := <-drained:
			return err
		case <-ctx.Done():
			return <-drained
		case <-tick.C:
			if err := u.subscribeOwn(ctx, conn, rng, st, subs); err != nil {
				cancel()
				<-drained
				return err
			}
		}
	}
}

func (u *UIClients) dialLimited(ctx context.Context, sem chan struct{}) (WSStream, error) {
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-sem }()
	return u.dial(ctx, u.baseURL, u.apiKey)
}

func wantsOwn(subs []string) bool {
	return slices.Contains(subs, SubOwnJobTasks) || slices.Contains(subs, SubOwnTaskLogs)
}

// subscribeOwn subscribes to the task updates of a recently accepted job and
// to the logs of one of its tasks, once each, as soon as a job exists. A
// failed lookup is retried on the next tick; a failed subscribe ends the
// session.
func (u *UIClients) subscribeOwn(ctx context.Context, conn WSStream, rng *rand.Rand, st *wsState, subs []string) error {
	_, haveJob, haveTask := st.snapshot()
	wantJob := slices.Contains(subs, SubOwnJobTasks) && !haveJob
	wantTask := slices.Contains(subs, SubOwnTaskLogs) && !haveTask
	if !wantJob && !wantTask {
		return nil
	}
	recent := u.jobs.Recent(ownJobWindow)
	if len(recent) == 0 {
		return nil
	}
	job := recent[rng.IntN(len(recent))]

	if wantJob {
		subject := fmt.Sprintf(ws.SubjectJobTasksFmt, job.ID)
		if err := conn.Subscribe(ctx, subject, st.seqs.subscribeCursor(subject)); err != nil {
			return err
		}
		st.add(subject)
		st.mu.Lock()
		st.ownJob = true
		st.mu.Unlock()
	}
	if wantTask {
		tasks, err := u.c.ListTasks(ctx, job.ID, 1, 0)
		if err != nil {
			return nil //nolint:nilerr // a failed lookup is retried on the next tick
		}
		if len(tasks.Items) == 0 {
			return nil
		}
		subject := fmt.Sprintf(ws.SubjectTaskLogsFmt, tasks.Items[0].ID)
		if err := conn.Subscribe(ctx, subject, st.seqs.subscribeCursor(subject)); err != nil {
			return err
		}
		st.add(subject)
		st.mu.Lock()
		st.ownTask = true
		st.mu.Unlock()
	}
	return nil
}

// drain reads pushes until the connection ends or ctx does.
func (u *UIClients) drain(ctx context.Context, conn WSStream, st *wsState) error {
	for {
		env, err := conn.Next(ctx)
		if err != nil {
			return err
		}
		u.observe(env, st)
	}
}

// observe accounts for one received envelope.
func (u *UIClients) observe(env ws.Envelope, st *wsState) {
	switch env.Type {
	case ws.TypePush:
	case ws.TypeError:
		u.m.WSErrors.Add(1)
		return
	default:
		return
	}
	u.m.WSEvents.Add(1)
	if delta := st.seqs.observe(env.Subject, env.Seq); delta != 0 {
		u.m.WSDrops.Add(delta)
	}
	if ts := eventTime(env.Payload); !ts.IsZero() && !ts.Before(st.cutoff(env.Subject)) {
		if lag := time.Since(ts); lag >= 0 {
			u.m.WSLag.Record(lag)
		} else {
			u.m.WSLagNegative.Add(1)
		}
	}
}

func safeInt64(v uint64) int64 {
	const maxInt64 = 1<<63 - 1
	if v > maxInt64 {
		return maxInt64
	}
	return int64(v)
}

// eventTime returns the server-side time a push payload carries: updated_at
// for job and task updates, at for log chunks, zero for a payload with
// neither (worker status).
func eventTime(payload json.RawMessage) time.Time {
	var p struct {
		UpdatedAt time.Time `json:"updated_at"`
		At        time.Time `json:"at"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return time.Time{}
	}
	if !p.UpdatedAt.IsZero() {
		return p.UpdatedAt
	}
	return p.At
}

// ── REST pollers ─────────────────────────────────────────────────────────────

// poller hits each endpoint once per interval until ctx ends. Its first round
// is offset by a random fraction of the interval so pollers do not arrive in
// step. A round that takes longer than the interval delays the next one: a
// poller is a client, and does not queue requests behind a slow server.
func (u *UIClients) poller(ctx context.Context, rng *rand.Rand, endpoints []string) {
	interval := u.spec.RESTPollers.Interval.Duration
	if interval <= 0 {
		return
	}
	if !sleepFor(ctx, time.Duration(rng.Float64()*float64(interval))) {
		return
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		for _, name := range endpoints {
			u.pollOnce(ctx, rng, name)
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return
		}
	}
}

// pollOnce makes one call to endpoint name and records it. An endpoint that
// needs a job is skipped until one has been accepted.
func (u *UIClients) pollOnce(ctx context.Context, rng *rand.Rand, name string) {
	var call func(context.Context) error
	switch name {
	case EndpointJobsList:
		call = func(ctx context.Context) error {
			_, err := u.c.ListJobs(ctx, u.farmID, pollPageSize, 0)
			return err
		}
	case EndpointWorkersList:
		call = func(ctx context.Context) error {
			_, err := u.c.ListWorkers(ctx, u.farmID, pollPageSize, 0)
			return err
		}
	case EndpointJobDetail, EndpointTaskList:
		job, ok := u.jobs.Pick(rng)
		if !ok {
			return
		}
		if name == EndpointJobDetail {
			call = func(ctx context.Context) error {
				_, err := u.c.GetJob(ctx, job.ID)
				return err
			}
		} else {
			call = func(ctx context.Context) error {
				_, err := u.c.ListTasks(ctx, job.ID, pollPageSize, 0)
				return err
			}
		}
	default:
		return
	}
	start := time.Now()
	err := call(ctx)
	elapsed := time.Since(start)
	switch {
	case err == nil:
		u.m.REST[name].Record(elapsed)
	case ctx.Err() == nil:
		u.m.RESTErrors.Add(1)
	}
}
