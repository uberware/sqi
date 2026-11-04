// SPDX-License-Identifier: AGPL-3.0-or-later

package observe

import (
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/measure"
)

// The queue and pool in testdata/metrics.txt, a /metrics body captured from an
// in-process sqi-server that had one queue, one usage pool and one ready task.
const capturedQueueID = "68217730-1642-4621-88a3-af59439ab1bc"

func loadCapture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/metrics.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// lastValue returns the last point of the named series.
func lastValue(t *testing.T, rec *measure.Recorder, name string) float64 {
	t.Helper()
	for _, s := range rec.Series() {
		if s.Name == name {
			if len(s.Points) == 0 {
				t.Fatalf("series %q has no points", name)
			}
			return s.Points[len(s.Points)-1].V
		}
	}
	names := []string{}
	for _, s := range rec.Series() {
		names = append(names, s.Name)
	}
	t.Fatalf("series %q not recorded; have %v", name, names)
	return 0
}

func hasSeries(rec *measure.Recorder, name string) bool {
	for _, s := range rec.Series() {
		if s.Name == name {
			return true
		}
	}
	return false
}

func pointCount(rec *measure.Recorder, name string) int {
	for _, s := range rec.Series() {
		if s.Name == name {
			return len(s.Points)
		}
	}
	return 0
}

var (
	cpuLine    = regexp.MustCompile(`(?m)^process_cpu_seconds_total (\S+)$`)
	bucketLine = regexp.MustCompile(`(?m)^(sqi_http_request_duration_seconds_(?:bucket|count)\{[^}]*\}) (\S+)$`)
)

// advance returns body as the same server would report it later: the process
// has used extraCPU more CPU seconds and every HTTP request histogram holds
// extra observations equal to those it held before (so the interval's
// distribution is the captured one).
func advance(t *testing.T, body string, extraCPU float64) string {
	t.Helper()
	body = cpuLine.ReplaceAllStringFunc(body, func(line string) string {
		v, err := strconv.ParseFloat(cpuLine.FindStringSubmatch(line)[1], 64)
		if err != nil {
			t.Fatal(err)
		}
		return "process_cpu_seconds_total " + strconv.FormatFloat(v+extraCPU, 'g', -1, 64)
	})
	return bucketLine.ReplaceAllStringFunc(body, func(line string) string {
		m := bucketLine.FindStringSubmatch(line)
		v, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			t.Fatal(err)
		}
		return m[1] + " " + strconv.FormatFloat(v*2, 'g', -1, 64)
	})
}

func TestScrape_ParsesCapturedMetrics(t *testing.T) {
	body := loadCapture(t)
	rec := measure.NewRecorder()
	st := newScrapeState(nil)
	t0 := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	if err := st.apply(rec, t0, []byte(body)); err != nil {
		t.Fatalf("apply: %v", err)
	}

	gauges := []struct {
		series string
		want   float64
	}{
		{"server.rss_bytes", 43225088},
		{"server.goroutines", 83},
		{"server.open_fds", 274},
		{"server.heap_bytes", 36076968},
		{"server.gc_pause_p99_s", 0.0005274},
		{"sched.queue_depth{" + capturedQueueID + "}", 1},
		{"sched.idle_workers", 0},
		{"workers.online", 0},
		{"usage.claims{gpu}", 0},
	}
	for _, g := range gauges {
		t.Run(g.series, func(t *testing.T) {
			if got := lastValue(t, rec, g.series); math.Abs(got-g.want) > 1e-9 {
				t.Fatalf("%s = %v, want %v", g.series, got, g.want)
			}
		})
	}

	// A rate needs two samples: the first one records neither the CPU rate nor
	// an HTTP latency, because both are deltas.
	for _, name := range []string{"server.cpu_cores", "http.p95_s{/api/v1/jobs/{id}/tasks}"} {
		if hasSeries(rec, name) {
			t.Fatalf("first sample recorded %q; want it only from the second", name)
		}
	}

	t1 := t0.Add(10 * time.Second)
	if err := st.apply(rec, t1, []byte(advance(t, body, 5))); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if got := lastValue(t, rec, "server.cpu_cores"); math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("server.cpu_cores = %v, want 0.5 (5 CPU seconds over 10 s)", got)
	}
	// Two raw job-tasks paths (different ids) merge into one route; both
	// requests landed in the le=0.005 bucket, so p95 interpolates to
	// 0.95 * 0.005.
	if got := lastValue(t, rec, "http.p95_s{/api/v1/jobs/{id}/tasks}"); math.Abs(got-0.00475) > 1e-9 {
		t.Fatalf("http.p95_s{/api/v1/jobs/{id}/tasks} = %v, want 0.00475", got)
	}
	if !hasSeries(rec, "http.p95_s{/api/v1/jobs/{id}}") || !hasSeries(rec, "http.p95_s{/api/v1/workers}") {
		t.Fatal("expected a p95 series for /api/v1/jobs/{id} and /api/v1/workers")
	}
	for _, s := range rec.Series() {
		if strings.Contains(s.Name, "0b9a3f0e") || strings.Contains(s.Name, "/readyz") {
			t.Fatalf("series %q leaks a raw id or an operational route", s.Name)
		}
	}
}

func TestScrape_QueueNames(t *testing.T) {
	rec := measure.NewRecorder()
	st := newScrapeState(map[string]string{capturedQueueID: "q1"})
	if err := st.apply(rec, time.Now(), []byte(loadCapture(t))); err != nil {
		t.Fatal(err)
	}
	if got := lastValue(t, rec, "sched.queue_depth{q1}"); got != 1 {
		t.Fatalf("sched.queue_depth{q1} = %v, want 1", got)
	}
}

func TestScrape_CounterResetSkipsInterval(t *testing.T) {
	body := loadCapture(t)
	rec := measure.NewRecorder()
	st := newScrapeState(nil)
	t0 := time.Now()
	if err := st.apply(rec, t0, []byte(advance(t, body, 100))); err != nil {
		t.Fatal(err)
	}
	// The server restarted: CPU seconds and request counts went back down.
	if err := st.apply(rec, t0.Add(5*time.Second), []byte(body)); err != nil {
		t.Fatal(err)
	}
	if hasSeries(rec, "server.cpu_cores") {
		t.Fatal("a CPU counter that went backwards produced a rate")
	}
	if hasSeries(rec, "http.p95_s{/api/v1/workers}") {
		t.Fatal("histogram counters that went backwards produced a p95")
	}
}

func TestScrape_RejectsGarbage(t *testing.T) {
	st := newScrapeState(nil)
	if err := st.apply(measure.NewRecorder(), time.Now(), []byte("this is not { prometheus text\n")); err == nil {
		t.Fatal("garbage parsed without error")
	}
}

func TestQuantileFromBuckets(t *testing.T) {
	inf := math.Inf(1)
	tests := []struct {
		name    string
		bounds  []float64
		cum     []float64
		q       float64
		want    float64
		wantOK  bool
		comment string
	}{
		{"all in first bucket", []float64{0.005, 0.01, inf}, []float64{10, 10, 10}, 0.95, 0.00475, true, ""},
		{"interpolates inside a bucket", []float64{1, 2, inf}, []float64{0, 100, 100}, 0.95, 1.95, true, ""},
		{"tail beyond the last finite bucket", []float64{1, 2, inf}, []float64{0, 50, 100}, 0.95, 2, true, "reports the last finite bound"},
		{"no observations", []float64{1, inf}, []float64{0, 0}, 0.95, 0, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := quantileFromBuckets(tt.bounds, tt.cum, tt.q)
			if ok != tt.wantOK || (ok && math.Abs(got-tt.want) > 1e-9) {
				t.Fatalf("quantile = (%v, %v), want (%v, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestRouteCollapse(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"/api/v1/jobs/01JABCDEFGHJKMNPQRSTVWXYZ0/tasks", "/api/v1/jobs/{id}/tasks"},
		{"/api/v1/jobs/0b9a3f0e-1c2d-4e5f-8a9b-0c1d2e3f4a5b", "/api/v1/jobs/{id}"},
		{"/api/v1/jobs/0B9A3F0E-1C2D-4E5F-8A9B-0C1D2E3F4A5B/tasks/0b9a3f0e-1c2d-4e5f-8a9b-0c1d2e3f4a5b/logs", "/api/v1/jobs/{id}/tasks/{id}/logs"},
		{"/api/v1/workers", "/api/v1/workers"},
		{"/api/v1/workers/12345", "/api/v1/workers/{id}"},
		{"/api/v1/jobs/", "/api/v1/jobs/"},
		{"/", "/"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := collapseRoute(tt.in); got != tt.want {
				t.Fatalf("collapseRoute(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
