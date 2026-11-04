// SPDX-License-Identifier: AGPL-3.0-or-later

package driver

import (
	"slices"
	"sync"
	"sync/atomic"

	"github.com/uberware/sqi/internal/loadtest/measure"
)

// StatusUndecodable is the status key of a submission the server answered 2xx
// but whose body could not be read as a job id. The job may exist on the
// server, but the harness cannot name it, so it is not in the ledger's accepted
// set; the ledger and the metrics both carry this key rather than the 2xx.
const StatusUndecodable = -1

// Names of the REST endpoints a poller can hit; scenario.PollSpec.Endpoints
// lists them.
const (
	EndpointJobsList    = "jobs-list"
	EndpointJobDetail   = "job-detail"
	EndpointTaskList    = "task-list"
	EndpointWorkersList = "workers-list"
)

// StandardEndpoints are the REST endpoints a poller supports.
var StandardEndpoints = []string{EndpointJobsList, EndpointJobDetail, EndpointTaskList, EndpointWorkersList}

// Metrics is what the driver measures. The histograms and counters are safe
// for concurrent use. Use it through a pointer: it holds locks and atomics.
type Metrics struct {
	// SubmitLatency is the HTTP round trip of each job submission.
	SubmitLatency *measure.Histogram
	// SubmitLag is how late each paced submission left the generator, measured
	// against its due time. Bursts (total) have no schedule and record none.
	SubmitLag *measure.Histogram
	// FirstLease, LeaseReply, TaskE2E, TerminalIngest and JobE2E are filled
	// by the runner from the worker ledger and the server's task and job
	// lists; the driver only declares them.
	FirstLease, LeaseReply, TaskE2E, TerminalIngest, JobE2E *measure.Histogram
	// WSLag is receive time minus the event time the push payload carries.
	// A push stamped after it was received (clock skew between hosts) is not
	// recorded; it is counted in WSLagNegative instead.
	WSLag *measure.Histogram
	// WSLagNegative counts pushes whose event time was later than their
	// arrival.
	WSLagNegative *measure.Counter
	// REST is the latency of each poller endpoint's successful calls, keyed by
	// endpoint name. The set of keys is fixed by NewMetrics.
	REST map[string]*measure.Histogram
	// byStatus counts submissions by HTTP status: 0 is a transport error and
	// StatusUndecodable a 2xx whose body held no job id. Count through Status
	// and read through StatusCounts.
	byStatus map[int]*measure.Counter
	// WSDrops is the number of pushes the server's sequence numbers show a
	// client missed. WSEvents is every push received.
	WSDrops, WSEvents *measure.Counter
	// WSErrors counts failed WebSocket dials, subscriptions and broken
	// connections; RESTErrors counts failed poller calls.
	WSErrors, RESTErrors *measure.Counter
	// RateLimited is set when any submission was answered 429: the run was
	// throttled by the server's rate limiter, not measured.
	RateLimited atomic.Bool

	mu sync.Mutex // guards byStatus
}

// NewMetrics returns Metrics with a latency histogram for each of endpoints
// and for every standard endpoint.
func NewMetrics(endpoints []string) *Metrics {
	m := &Metrics{
		SubmitLatency:  measure.NewHistogram(),
		SubmitLag:      measure.NewHistogram(),
		FirstLease:     measure.NewHistogram(),
		LeaseReply:     measure.NewHistogram(),
		TaskE2E:        measure.NewHistogram(),
		TerminalIngest: measure.NewHistogram(),
		JobE2E:         measure.NewHistogram(),
		WSLag:          measure.NewHistogram(),
		WSLagNegative:  &measure.Counter{},
		REST:           make(map[string]*measure.Histogram),
		byStatus:       make(map[int]*measure.Counter),
		WSDrops:        &measure.Counter{},
		WSEvents:       &measure.Counter{},
		WSErrors:       &measure.Counter{},
		RESTErrors:     &measure.Counter{},
	}
	for _, e := range StandardEndpoints {
		m.REST[e] = measure.NewHistogram()
	}
	for _, e := range endpoints {
		if m.REST[e] == nil {
			m.REST[e] = measure.NewHistogram()
		}
	}
	return m
}

// Status returns the counter of submissions answered with HTTP status code,
// creating it on first use.
func (m *Metrics) Status(code int) *measure.Counter {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.byStatus[code]
	if c == nil {
		c = &measure.Counter{}
		m.byStatus[code] = c
	}
	return c
}

// StatusCounts returns a snapshot of the per-status counts.
func (m *Metrics) StatusCounts() map[int]int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[int]int64, len(m.byStatus))
	for code, c := range m.byStatus {
		out[code] = c.Load()
	}
	return out
}

// StatusCodes returns the statuses seen so far in ascending order.
func (m *Metrics) StatusCodes() []int {
	counts := m.StatusCounts()
	codes := make([]int, 0, len(counts))
	for code := range counts {
		codes = append(codes, code)
	}
	slices.Sort(codes)
	return codes
}
