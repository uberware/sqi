// SPDX-License-Identifier: AGPL-3.0-or-later

package driver

import (
	"slices"
	"sync"
	"sync/atomic"

	"github.com/uberware/sqi/internal/loadtest/measure"
)

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
	// FirstLease, LeaseReply, TaskE2E and JobE2E are filled by the runner and
	// the verifier from the worker ledger; the driver only declares them.
	FirstLease, LeaseReply, TaskE2E, JobE2E *measure.Histogram
	// WSLag is receive time minus the event time the push payload carries.
	WSLag *measure.Histogram
	// REST is the latency of each poller endpoint's successful calls, keyed by
	// endpoint name. The set of keys is fixed by NewMetrics.
	REST map[string]*measure.Histogram
	// ByStatus counts submissions by HTTP status, 0 being a transport error.
	// Count through Status and read through StatusCounts while the driver
	// runs; the map itself is only safe to read once it has stopped.
	ByStatus map[int]*measure.Counter
	// WSDrops is the number of pushes the server's sequence numbers show a
	// client missed. WSEvents is every push received.
	WSDrops, WSEvents *measure.Counter
	// WSErrors counts failed WebSocket dials, subscriptions and broken
	// connections; RESTErrors counts failed poller calls.
	WSErrors, RESTErrors *measure.Counter
	// RateLimited is set when any submission was answered 429: the run was
	// throttled by the server's rate limiter, not measured.
	RateLimited atomic.Bool

	mu sync.Mutex // guards ByStatus
}

// NewMetrics returns Metrics with a latency histogram for each of endpoints
// and for every standard endpoint.
func NewMetrics(endpoints []string) *Metrics {
	m := &Metrics{
		SubmitLatency: measure.NewHistogram(),
		SubmitLag:     measure.NewHistogram(),
		FirstLease:    measure.NewHistogram(),
		LeaseReply:    measure.NewHistogram(),
		TaskE2E:       measure.NewHistogram(),
		JobE2E:        measure.NewHistogram(),
		WSLag:         measure.NewHistogram(),
		REST:          make(map[string]*measure.Histogram),
		ByStatus:      make(map[int]*measure.Counter),
		WSDrops:       &measure.Counter{},
		WSEvents:      &measure.Counter{},
		WSErrors:      &measure.Counter{},
		RESTErrors:    &measure.Counter{},
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
	c := m.ByStatus[code]
	if c == nil {
		c = &measure.Counter{}
		m.ByStatus[code] = c
	}
	return c
}

// StatusCounts returns a snapshot of ByStatus.
func (m *Metrics) StatusCounts() map[int]int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[int]int64, len(m.ByStatus))
	for code, c := range m.ByStatus {
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
