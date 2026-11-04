// SPDX-License-Identifier: AGPL-3.0-or-later

package observe

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/uberware/sqi/internal/loadtest/measure"
)

// maxMetricsBytes bounds a /metrics body the scrape will read.
const maxMetricsBytes = 64 << 20

// maxRoutes bounds the distinct route series. The server labels its latency
// histogram with the raw request path, so a scanner hitting random URLs would
// otherwise mint a series per URL; routes past the bound share "other".
const maxRoutes = 100

// otherRoute is the route of every path beyond maxRoutes.
const otherRoute = "other"

// quietRoutes are served by the server for operators, not for the farm: the
// scrape itself, probes, profiling, and the WebSocket upgrade, whose duration
// is the connection's lifetime.
var quietRoutes = []string{"/metrics", "/healthz", "/readyz", "/api/v1/ws", "/debug/pprof"}

var (
	uuidSegment    = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	ulidSegment    = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Za-hjkmnp-tv-z]{26}$`)
	numericSegment = regexp.MustCompile(`^\d+$`)
)

// collapseRoute turns a raw request path into a route pattern by replacing
// each segment that is a UUID, a ULID or a number with {id}.
func collapseRoute(path string) string {
	segs := strings.Split(path, "/")
	for i, s := range segs {
		if s != "" && (uuidSegment.MatchString(s) || ulidSegment.MatchString(s) || numericSegment.MatchString(s)) {
			segs[i] = "{id}"
		}
	}
	return strings.Join(segs, "/")
}

func isQuietRoute(route string) bool {
	for _, q := range quietRoutes {
		if route == q || strings.HasPrefix(route, q+"/") {
			return true
		}
	}
	return false
}

// labeled formats a series name carrying one label: name{label}.
func labeled(name, label string) string { return name + "{" + label + "}" }

// histogram is one route's latency histogram merged over methods and raw
// paths: cumulative counts by upper bound, the +Inf bucket last.
type histogram struct {
	bounds []float64
	cum    []float64
}

// scrapeState turns successive /metrics bodies into series. The rates and the
// per-interval HTTP percentile are deltas, so it keeps the previous scrape.
type scrapeState struct {
	queueNames map[string]string

	havePrev bool
	prevAt   time.Time
	prevCPU  float64
	prevHist map[string]histogram
	routes   map[string]struct{}
}

// newScrapeState returns a state that names queue gauges by queueNames (queue
// id to scenario name) where it knows the id, and by the raw label otherwise.
func newScrapeState(queueNames map[string]string) *scrapeState {
	return &scrapeState{queueNames: queueNames, routes: map[string]struct{}{}}
}

// apply parses one /metrics body taken at t and records its series.
func (s *scrapeState) apply(rec *measure.Recorder, t time.Time, body []byte) error {
	parser := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := parser.TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("parse /metrics: %w", err)
	}

	s.recordProcess(rec, t, fams)
	s.recordScheduler(rec, t, fams)
	s.recordHTTP(rec, t, fams)
	return nil
}

// gauge returns the single unlabeled value of a gauge, counter or untyped family.
func gauge(fams map[string]*dto.MetricFamily, name string) (float64, bool) {
	f, ok := fams[name]
	if !ok || len(f.GetMetric()) == 0 {
		return 0, false
	}
	return scalar(f.GetMetric()[0])
}

func scalar(m *dto.Metric) (float64, bool) {
	switch {
	case m.GetGauge() != nil:
		return m.GetGauge().GetValue(), true
	case m.GetCounter() != nil:
		return m.GetCounter().GetValue(), true
	case m.GetUntyped() != nil:
		return m.GetUntyped().GetValue(), true
	}
	return 0, false
}

func labelValue(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

// forEach calls fn for each scalar metric of family name.
func forEach(fams map[string]*dto.MetricFamily, name string, fn func(m *dto.Metric, v float64)) {
	f, ok := fams[name]
	if !ok {
		return
	}
	for _, m := range f.GetMetric() {
		if v, ok := scalar(m); ok {
			fn(m, v)
		}
	}
}

func (s *scrapeState) recordProcess(rec *measure.Recorder, t time.Time, fams map[string]*dto.MetricFamily) {
	if v, ok := gauge(fams, "process_resident_memory_bytes"); ok {
		rec.Add("server.rss_bytes", "bytes", t, v)
	}
	if v, ok := gauge(fams, "go_goroutines"); ok {
		rec.Add("server.goroutines", "count", t, v)
	}
	if v, ok := gauge(fams, "process_open_fds"); ok {
		rec.Add("server.open_fds", "count", t, v)
	}
	if v, ok := gauge(fams, "go_memstats_heap_alloc_bytes"); ok {
		rec.Add("server.heap_bytes", "bytes", t, v)
	}
	if v, ok := summaryTail(fams, "go_gc_duration_seconds"); ok {
		rec.Add("server.gc_pause_p99_s", "s", t, v)
	}

	cpu, ok := gauge(fams, "process_cpu_seconds_total")
	if !ok {
		return
	}
	// The first sample has nothing to difference against, and a counter that
	// went backwards means the server restarted: neither yields a rate.
	if s.havePrev && cpu >= s.prevCPU {
		if dt := t.Sub(s.prevAt).Seconds(); dt > 0 {
			rec.Add("server.cpu_cores", "cores", t, (cpu-s.prevCPU)/dt)
		}
	}
	s.prevCPU = cpu
}

// summaryTail returns the 0.99 quantile of a summary, or the highest quantile
// it reports. The Go collector's GC pause summary reports 0, .25, .5, .75 and
// 1, so its highest is the maximum pause the runtime has seen.
func summaryTail(fams map[string]*dto.MetricFamily, name string) (float64, bool) {
	f, ok := fams[name]
	if !ok || len(f.GetMetric()) == 0 || f.GetMetric()[0].GetSummary() == nil {
		return 0, false
	}
	best, bestQ, found := 0.0, math.Inf(-1), false
	for _, q := range f.GetMetric()[0].GetSummary().GetQuantile() {
		if q.GetQuantile() == 0.99 {
			return q.GetValue(), true
		}
		if q.GetQuantile() > bestQ {
			best, bestQ, found = q.GetValue(), q.GetQuantile(), true
		}
	}
	return best, found
}

func (s *scrapeState) recordScheduler(rec *measure.Recorder, t time.Time, fams map[string]*dto.MetricFamily) {
	forEach(fams, "sqi_scheduler_queue_depth", func(m *dto.Metric, v float64) {
		q := labelValue(m, "queue")
		if name, ok := s.queueNames[q]; ok {
			q = name
		}
		rec.Add(labeled("sched.queue_depth", q), "tasks", t, v)
	})

	idle, haveIdle := 0.0, false
	forEach(fams, "sqi_scheduler_idle_workers", func(_ *dto.Metric, v float64) { idle += v; haveIdle = true })
	if haveIdle {
		rec.Add("sched.idle_workers", "workers", t, idle)
	}

	forEach(fams, "sqi_workers_total", func(m *dto.Metric, v float64) {
		if labelValue(m, "status") == "online" {
			rec.Add("workers.online", "workers", t, v)
		}
	})
	forEach(fams, "sqi_usage_active_claims", func(m *dto.Metric, v float64) {
		rec.Add(labeled("usage.claims", labelValue(m, "pool")), "claims", t, v)
	})
}

// routeOf returns the series route for a raw path, folding paths beyond
// maxRoutes into otherRoute. It reports false for routes not worth a series.
func (s *scrapeState) routeOf(path string) (string, bool) {
	route := collapseRoute(path)
	if isQuietRoute(route) {
		return "", false
	}
	if _, known := s.routes[route]; known {
		return route, true
	}
	if len(s.routes) >= maxRoutes {
		return otherRoute, true
	}
	s.routes[route] = struct{}{}
	return route, true
}

// routeHistograms merges the request-duration histograms by route.
func (s *scrapeState) routeHistograms(fams map[string]*dto.MetricFamily) map[string]histogram {
	f, ok := fams["sqi_http_request_duration_seconds"]
	if !ok {
		return nil
	}
	merged := map[string]map[float64]float64{}
	for _, m := range f.GetMetric() {
		h := m.GetHistogram()
		if h == nil {
			continue
		}
		route, ok := s.routeOf(labelValue(m, "path"))
		if !ok {
			continue
		}
		byBound := merged[route]
		if byBound == nil {
			byBound = map[float64]float64{}
			merged[route] = byBound
		}
		for _, b := range h.GetBucket() {
			if !math.IsInf(b.GetUpperBound(), 1) {
				byBound[b.GetUpperBound()] += float64(b.GetCumulativeCount())
			}
		}
		// The +Inf bucket is implicit in the text format: it is the sample count.
		byBound[math.Inf(1)] += float64(h.GetSampleCount())
	}
	out := make(map[string]histogram, len(merged))
	for route, byBound := range merged {
		h := histogram{}
		for b := range byBound {
			h.bounds = append(h.bounds, b)
		}
		slices.Sort(h.bounds)
		for _, b := range h.bounds {
			h.cum = append(h.cum, byBound[b])
		}
		out[route] = h
	}
	return out
}

func (s *scrapeState) recordHTTP(rec *measure.Recorder, t time.Time, fams map[string]*dto.MetricFamily) {
	cur := s.routeHistograms(fams)
	prev := s.prevHist
	s.prevHist = cur
	for _, route := range sortedKeys(cur) {
		before, ok := prev[route]
		if !ok {
			// A route first seen this scrape has no baseline; its next sample does.
			// The first scrape of a long-running server would otherwise report
			// the percentile of everything since the server started.
			if !s.havePrev {
				continue
			}
			before = histogram{}
		}
		delta, ok := cur[route].minus(before)
		if !ok {
			continue
		}
		if p, ok := quantileFromBuckets(delta.bounds, delta.cum, 0.95); ok {
			rec.Add(labeled("http.p95_s", route), "s", t, p)
		}
	}
	s.havePrev = true
	s.prevAt = t
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// minus returns h less before, bucket by bucket. It reports false when any
// bucket went backwards, the sign of a server restart.
func (h histogram) minus(before histogram) (histogram, bool) {
	prevAt := func(bound float64) float64 {
		i, ok := slices.BinarySearch(before.bounds, bound)
		if !ok {
			return 0
		}
		return before.cum[i]
	}
	out := histogram{bounds: h.bounds, cum: make([]float64, len(h.cum))}
	for i, b := range h.bounds {
		d := h.cum[i] - prevAt(b)
		if d < 0 {
			return histogram{}, false
		}
		out.cum[i] = d
	}
	// Every bucket the previous scrape had must still be there.
	for _, b := range before.bounds {
		if _, ok := slices.BinarySearch(h.bounds, b); !ok {
			return histogram{}, false
		}
	}
	return out, true
}

// quantileFromBuckets estimates the q quantile from cumulative bucket counts
// by linear interpolation inside the bucket that holds it, as Prometheus's
// histogram_quantile does. bounds are ascending with +Inf last. A quantile in
// the +Inf bucket is reported as the highest finite bound. It reports false
// when there are no observations.
func quantileFromBuckets(bounds, cum []float64, q float64) (float64, bool) {
	if len(bounds) == 0 || len(bounds) != len(cum) {
		return 0, false
	}
	total := cum[len(cum)-1]
	if total <= 0 {
		return 0, false
	}
	target := q * total
	lower, lowerCount := 0.0, 0.0
	for i, b := range bounds {
		if cum[i] < target {
			if !math.IsInf(b, 1) {
				lower, lowerCount = b, cum[i]
			}
			continue
		}
		if math.IsInf(b, 1) {
			return lower, true
		}
		span := cum[i] - lowerCount
		if span <= 0 {
			return b, true
		}
		return lower + (b-lower)*(target-lowerCount)/span, true
	}
	return lower, true
}

// scraper fetches /metrics.
type scraper struct {
	client  *http.Client
	url     string
	apiKey  string
	timeout time.Duration
}

func (c scraper) fetch(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url+"/metrics", http.NoBody)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /metrics: %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxMetricsBytes))
}
