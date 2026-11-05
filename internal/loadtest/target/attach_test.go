// SPDX-License-Identifier: AGPL-3.0-or-later

package target

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/simworker"
)

// fakeAPI is a minimal in-memory sqi REST API: farms, queues, usage pools,
// jobs and workers, enough for an attached target's setup and teardown. With
// throttle set it answers every other /api/v1 request 429, as the server's
// rate limiter does to a client over its budget.
type fakeAPI struct {
	throttle bool

	mu      sync.Mutex
	next    int
	farms   map[string]string // id -> name
	queues  map[string]string
	pools   map[string]string
	jobs    map[string]bool
	workers map[string]bool

	calls, throttled atomic.Int64
}

func newFakeAPI(throttle bool) *fakeAPI {
	return &fakeAPI{
		throttle: throttle,
		farms:    map[string]string{}, queues: map[string]string{}, pools: map[string]string{},
		jobs: map[string]bool{}, workers: map[string]bool{},
	}
}

func (f *fakeAPI) start(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(ts.Close)
	return ts
}

func (f *fakeAPI) id(prefix string) string {
	f.next++
	return fmt.Sprintf("%s-%d", prefix, f.next)
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/readyz" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if n := f.calls.Add(1); f.throttle && n%2 == 1 {
		f.throttled.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	kind, id, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	stores := map[string]map[string]string{"farms": f.farms, "queues": f.queues, "usage-pools": f.pools}
	switch {
	case r.Method == http.MethodPost && id == "" && stores[kind] != nil:
		var body struct {
			Name string `json:"name"`
		}
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck // test server
		newID := f.id(kind)
		stores[kind][newID] = body.Name
		writeJSON(w, http.StatusCreated, map[string]string{"id": newID})
	case r.Method == http.MethodPost && path == "/workers/enroll":
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodDelete && stores[kind] != nil:
		delete(stores[kind], id)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && (kind == "jobs" || kind == "workers"):
		set := f.jobs
		if kind == "workers" {
			set = f.workers
		}
		items := []map[string]string{}
		for id := range set {
			items = append(items, map[string]string{"id": id})
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items)})
	case r.Method == http.MethodDelete && kind == "jobs":
		delete(f.jobs, id)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete && kind == "workers":
		delete(f.workers, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck // test server
}

func (f *fakeAPI) left() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.farms) + len(f.queues) + len(f.pools) + len(f.jobs) + len(f.workers)
}

// TestAttach_RidesOutTheRateLimiter attaches to a server whose rate limiter
// throttles every other call, as an attached server with the default limiter
// does to a generator over 20 requests per second: setup, a worker's
// enrollment and the teardown of a farm with jobs and workers in it must all
// succeed, leaving nothing behind.
func TestAttach_RidesOutTheRateLimiter(t *testing.T) {
	api := newFakeAPI(true)
	ts := api.start(t)
	sc := &scenario.Scenario{Farm: scenario.FarmSpec{
		Queues:     []scenario.QueueSpec{{Name: "render"}, {Name: "comp"}},
		UsagePools: []scenario.PoolSpec{{Name: "arnold", Limit: 2}},
	}}
	tgt, err := Attach(t.Context(), AttachConfig{
		HTTPURL: ts.URL, NATSURL: "nats://unused", JoinToken: "tok", Scenario: sc, RunID: "rl-1", Logger: testLogger(t),
	})
	if err != nil {
		t.Fatalf("Attach against a throttling server: %v", err)
	}
	if _, _, err := tgt.Credentials().For(t.Context(), "w-1"); err != nil {
		t.Errorf("enroll against a throttling server: %v", err)
	}
	api.mu.Lock()
	for i := range 12 {
		api.jobs[fmt.Sprintf("job-%d", i)] = true
	}
	for i := range 3 {
		api.workers[fmt.Sprintf("worker-%d", i)] = true
	}
	api.mu.Unlock()
	if err := tgt.Close(t.Context()); err != nil {
		t.Fatalf("Close against a throttling server: %v", err)
	}
	if n := api.left(); n != 0 {
		t.Errorf("%d farms, queues, pools, jobs and workers left after Close", n)
	}
	if api.throttled.Load() == 0 {
		t.Error("the fake server throttled nothing; the test proves nothing")
	}
}

// stuckAPI lists one job and one worker forever, whatever is deleted: a
// listing that never catches up.
func stuckAPI(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var lists atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			lists.Add(1)
			writeJSON(w, http.StatusOK, map[string]any{"items": []map[string]string{{"id": "x"}}, "total": 1})
		case strings.Contains(r.URL.Path, "/jobs/"):
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(ts.Close)
	return ts, &lists
}

// TestAttach_TeardownLoopsAreBounded checks that deleting the farm's jobs and
// removing its workers neither spin nor run forever when the listing never
// empties: each pass pauses, and each loop gives up at its deadline.
func TestAttach_TeardownLoopsAreBounded(t *testing.T) {
	oldJobs, oldWorkers, oldPause := jobDeletionTimeout, workerRemovalTimeout, removalPause
	jobDeletionTimeout, workerRemovalTimeout, removalPause = 500*time.Millisecond, 500*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { jobDeletionTimeout, workerRemovalTimeout, removalPause = oldJobs, oldWorkers, oldPause })

	for _, tt := range []struct {
		name string
		loop func(*attached) error
	}{
		{"jobs", func(a *attached) error { return a.deleteJobs(t.Context()) }},
		{"workers", func(a *attached) error { return a.removeWorkers(t.Context()) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ts, lists := stuckAPI(t)
			c, err := newClient(ts.URL, "", simworker.TLSFiles{})
			if err != nil {
				t.Fatal(err)
			}
			a := &attached{client: c, logger: testLogger(t), farm: farm{id: "f1"}}
			start := time.Now()
			err = tt.loop(a)
			if err == nil || !strings.Contains(err.Error(), "still listed") {
				t.Errorf("err = %v, want one saying the %s are still listed", err, tt.name)
			}
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Errorf("gave up after %s, want about its 500 ms deadline", elapsed)
			}
			if n := lists.Load(); n > 20 {
				t.Errorf("listed %d times in about 500 ms: the loop spins", n)
			}
		})
	}
}
