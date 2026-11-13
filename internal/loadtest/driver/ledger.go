// SPDX-License-Identifier: AGPL-3.0-or-later

package driver

import (
	"math/rand/v2"
	"net/http"
	"sync"
	"time"
)

// SubmittedJob is one submission attempt.
type SubmittedJob struct {
	// ID is the server's job id, empty when the submission was rejected.
	ID string
	// Name is the job's name in its template.
	Name string
	// Class is the scenario job class.
	Class string
	// SubmittedAt is when the request left the generator; AcceptedAt is when
	// the 2xx answer arrived, and the zero time for a rejection.
	SubmittedAt, AcceptedAt time.Time
	// Status is the HTTP status, 0 for a transport error.
	Status int
}

// Accepted reports whether the server took the job.
func (j SubmittedJob) Accepted() bool {
	return j.ID != "" && j.Status >= http.StatusOK && j.Status < http.StatusMultipleChoices
}

// JobLedger records every submission attempt of a run, rejected ones too. It
// is safe for concurrent use.
type JobLedger struct {
	mu       sync.Mutex
	all      []SubmittedJob
	accepted []int // indexes into all
}

// NewJobLedger returns an empty ledger.
func NewJobLedger() *JobLedger { return &JobLedger{} }

// Record adds one attempt.
func (l *JobLedger) Record(j SubmittedJob) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if j.Accepted() {
		l.accepted = append(l.accepted, len(l.all))
	}
	l.all = append(l.all, j)
}

// Accepted returns the accepted jobs in the order they were recorded.
func (l *JobLedger) Accepted() []SubmittedJob {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]SubmittedJob, len(l.accepted))
	for i, idx := range l.accepted {
		out[i] = l.all[idx]
	}
	return out
}

// Rejected returns the attempts the server did not accept (an error status,
// a timeout, a dropped connection) in the order they were recorded. Each
// keeps its Name and Class, so a job the server created anyway can be matched
// to it by name.
func (l *JobLedger) Rejected() []SubmittedJob {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []SubmittedJob
	for _, j := range l.all {
		if !j.Accepted() {
			out = append(out, j)
		}
	}
	return out
}

// AcceptedCount returns how many jobs the server accepted.
func (l *JobLedger) AcceptedCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.accepted)
}

// Chooser picks a uniform integer in [0, n). *rand.Rand satisfies it.
type Chooser interface{ IntN(n int) int }

// Pick returns a uniformly chosen accepted job, or false when there is none.
func (l *JobLedger) Pick(r Chooser) (SubmittedJob, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.accepted) == 0 {
		return SubmittedJob{}, false
	}
	return l.all[l.accepted[r.IntN(len(l.accepted))]], true
}

// Recent returns up to n of the most recently accepted jobs, oldest first.
func (l *JobLedger) Recent(n int) []SubmittedJob {
	l.mu.Lock()
	defer l.mu.Unlock()
	idx := l.accepted
	if len(idx) > n {
		idx = idx[len(idx)-n:]
	}
	out := make([]SubmittedJob, len(idx))
	for i, k := range idx {
		out[i] = l.all[k]
	}
	return out
}

// syncRand is a *rand.Rand shared between goroutines.
type syncRand struct {
	mu sync.Mutex
	r  *rand.Rand
}

// newSyncRand wraps r, or a fixed-seed generator when r is nil, so an
// unseeded caller is still reproducible.
func newSyncRand(r *rand.Rand) *syncRand {
	if r == nil {
		r = rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: choices must be reproducible
	}
	return &syncRand{r: r}
}

func (s *syncRand) IntN(n int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.r.IntN(n)
}

func (s *syncRand) Float64() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.r.Float64()
}

func (s *syncRand) Perm(n int) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.r.Perm(n)
}

// child returns an independent generator derived from s, for a goroutine that
// wants its own stream. Children are drawn in call order, so deriving them
// before starting goroutines keeps a run reproducible.
func (s *syncRand) child() *rand.Rand {
	s.mu.Lock()
	defer s.mu.Unlock()
	return rand.New(rand.NewPCG(s.r.Uint64(), s.r.Uint64())) //nolint:gosec // G404: choices must be reproducible
}
