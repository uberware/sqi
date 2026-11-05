// SPDX-License-Identifier: AGPL-3.0-or-later

package measure_test

import (
	"encoding/json"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/measure"
)

func within(t *testing.T, name string, got, want time.Duration, rel float64) {
	t.Helper()
	if diff := math.Abs(float64(got-want)) / float64(want); diff > rel {
		t.Errorf("%s = %v, want %v within %.2f%% (off by %.3f%%)", name, got, want, rel*100, diff*100)
	}
}

func TestHistogram_Quantiles(t *testing.T) {
	h := measure.NewHistogram()
	for i := 1; i <= 10000; i++ {
		h.Record(time.Duration(i) * time.Millisecond)
	}
	if got := h.Count(); got != 10000 {
		t.Fatalf("Count = %d, want 10000", got)
	}
	tests := []struct {
		name string
		q    float64
		want time.Duration
	}{
		{"p50", 0.50, 5000 * time.Millisecond},
		{"p90", 0.90, 9000 * time.Millisecond},
		{"p95", 0.95, 9500 * time.Millisecond},
		{"p99", 0.99, 9900 * time.Millisecond},
		{"p999", 0.999, 9990 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			within(t, tt.name, h.Quantile(tt.q), tt.want, 0.01)
		})
	}
	if got := h.Max(); got != 10000*time.Millisecond {
		t.Errorf("Max = %v, want 10s", got)
	}
	if got := h.Min(); got != time.Millisecond {
		t.Errorf("Min = %v, want 1ms", got)
	}
	if got := h.Quantile(1); got != h.Max() {
		t.Errorf("Quantile(1) = %v, want Max %v", got, h.Max())
	}
	if got := h.Quantile(0); got != h.Min() {
		t.Errorf("Quantile(0) = %v, want Min %v", got, h.Min())
	}
	within(t, "Mean", h.Mean(), 5000500*time.Microsecond, 1e-9)
}

func TestHistogram_RelativeError(t *testing.T) {
	// A single value is exact: the quantile is clamped to the true min and max.
	for _, d := range []time.Duration{
		time.Microsecond, 1500 * time.Nanosecond, 37 * time.Microsecond, 999 * time.Microsecond,
		time.Millisecond, 123456 * time.Microsecond, time.Second, 1234567 * time.Millisecond,
		time.Minute, time.Hour, 23*time.Hour + 59*time.Minute,
	} {
		h := measure.NewHistogram()
		h.Record(d)
		if h.Min() != d || h.Max() != d || h.Quantile(0.5) != d {
			t.Errorf("single %v: min=%v max=%v p50=%v", d, h.Min(), h.Max(), h.Quantile(0.5))
		}
	}
	// Two values force the quantile to come from a bucket midpoint.
	for _, d := range []time.Duration{
		time.Microsecond, 1500 * time.Nanosecond, 37 * time.Microsecond, 999 * time.Microsecond,
		123456 * time.Microsecond, 1234567 * time.Millisecond, time.Hour + 17*time.Second,
	} {
		h := measure.NewHistogram()
		h.Record(d)
		h.Record(24 * time.Hour)
		within(t, "p25 of "+d.String(), h.Quantile(0.25), d, 0.01)
	}
}

func TestHistogram_Extremes(t *testing.T) {
	h := measure.NewHistogram()
	h.Record(-5 * time.Second)
	h.Record(0)
	h.Record(100 * time.Hour)
	if h.Count() != 3 {
		t.Fatalf("Count = %d, want 3", h.Count())
	}
	if h.Min() != 0 {
		t.Errorf("Min = %v, want 0 (negative values are recorded as zero)", h.Min())
	}
	if h.Max() != 100*time.Hour {
		t.Errorf("Max = %v, want the true 100h", h.Max())
	}
	if got := h.Quantile(1); got != 100*time.Hour {
		t.Errorf("Quantile(1) = %v, want 100h", got)
	}
}

func TestHistogram_Empty(t *testing.T) {
	h := measure.NewHistogram()
	if h.Count() != 0 || h.Quantile(0.5) != 0 || h.Min() != 0 || h.Max() != 0 || h.Mean() != 0 {
		t.Errorf("empty histogram not all zero: n=%d q=%v min=%v max=%v mean=%v",
			h.Count(), h.Quantile(0.5), h.Min(), h.Max(), h.Mean())
	}
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var back measure.Histogram
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Count() != 0 || back.Min() != 0 || back.Max() != 0 {
		t.Errorf("round-tripped empty histogram: n=%d min=%v max=%v", back.Count(), back.Min(), back.Max())
	}
	back.Record(time.Second)
	if back.Min() != time.Second {
		t.Errorf("Min after first record = %v, want 1s", back.Min())
	}
	var zero measure.Histogram
	zero.Record(time.Second)
	if zero.Min() != time.Second || zero.Max() != time.Second {
		t.Errorf("zero-value histogram: min=%v max=%v", zero.Min(), zero.Max())
	}
}

func TestHistogram_QuantileClamps(t *testing.T) {
	h := measure.NewHistogram()
	h.Record(time.Second)
	h.Record(2 * time.Second)
	if h.Quantile(-1) != h.Min() || h.Quantile(2) != h.Max() || h.Quantile(math.NaN()) != h.Min() {
		t.Errorf("out-of-range q: %v %v %v", h.Quantile(-1), h.Quantile(2), h.Quantile(math.NaN()))
	}
}

func TestHistogram_Merge(t *testing.T) {
	all, lo, hi := measure.NewHistogram(), measure.NewHistogram(), measure.NewHistogram()
	for i := 1; i <= 10000; i++ {
		d := time.Duration(i) * time.Millisecond
		all.Record(d)
		if i <= 5000 {
			lo.Record(d)
		} else {
			hi.Record(d)
		}
	}
	merged := measure.NewHistogram()
	merged.Merge(lo)
	merged.Merge(hi)
	merged.Merge(nil)
	merged.Merge(measure.NewHistogram())
	if merged.Count() != all.Count() {
		t.Fatalf("Count = %d, want %d", merged.Count(), all.Count())
	}
	for _, q := range []float64{0, 0.01, 0.5, 0.9, 0.99, 0.999, 1} {
		if g, w := merged.Quantile(q), all.Quantile(q); g != w {
			t.Errorf("Quantile(%v) = %v, want %v", q, g, w)
		}
	}
	if merged.Min() != all.Min() || merged.Max() != all.Max() || merged.Mean() != all.Mean() {
		t.Errorf("min/max/mean: got %v/%v/%v want %v/%v/%v",
			merged.Min(), merged.Max(), merged.Mean(), all.Min(), all.Max(), all.Mean())
	}
}

func TestHistogram_JSONRoundTrip(t *testing.T) {
	h := measure.NewHistogram()
	for i := 1; i <= 5000; i++ {
		h.Record(time.Duration(i*i) * time.Microsecond)
	}
	h.Record(48 * time.Hour)
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var back measure.Histogram
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Count() != h.Count() || back.Min() != h.Min() || back.Max() != h.Max() || back.Mean() != h.Mean() {
		t.Errorf("summary differs: got n=%d min=%v max=%v mean=%v, want n=%d min=%v max=%v mean=%v",
			back.Count(), back.Min(), back.Max(), back.Mean(), h.Count(), h.Min(), h.Max(), h.Mean())
	}
	for _, q := range []float64{0, 0.001, 0.25, 0.5, 0.75, 0.95, 0.99, 0.9999, 1} {
		if g, w := back.Quantile(q), h.Quantile(q); g != w {
			t.Errorf("Quantile(%v) = %v, want %v", q, g, w)
		}
	}
	// Only non-empty buckets are written.
	if len(b) > 40000 {
		t.Errorf("encoding is %d bytes; expected only non-empty buckets", len(b))
	}
	// Re-encoding is byte-identical.
	b2, err := json.Marshal(&back)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(b2) {
		t.Errorf("re-encoding differs:\n%s\n%s", b, b2)
	}
}

func TestHistogram_JSONInStruct(t *testing.T) {
	type wrap struct {
		H *measure.Histogram `json:"h"`
		V measure.Histogram  `json:"v"`
	}
	w := wrap{H: measure.NewHistogram()}
	w.H.Record(3 * time.Millisecond)
	w.V.Record(7 * time.Millisecond)
	b, err := json.Marshal(&w)
	if err != nil {
		t.Fatal(err)
	}
	var back wrap
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.H.Max() != 3*time.Millisecond || back.V.Max() != 7*time.Millisecond {
		t.Errorf("got %v %v", back.H.Max(), back.V.Max())
	}
}

func TestHistogram_UnmarshalRejects(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"not an object", `[]`},
		{"bucket index out of range", `{"count":1,"sum_ns":5,"min_ns":5,"max_ns":5,"buckets":{"99999999":1}}`},
		{"negative bucket index", `{"count":1,"sum_ns":5,"min_ns":5,"max_ns":5,"buckets":{"-1":1}}`},
		{"non-numeric bucket index", `{"count":1,"sum_ns":5,"min_ns":5,"max_ns":5,"buckets":{"x":1}}`},
		{"negative bucket count", `{"count":1,"sum_ns":5,"min_ns":5,"max_ns":5,"buckets":{"5":-1}}`},
		{"count mismatch", `{"count":2,"sum_ns":5,"min_ns":5,"max_ns":5,"buckets":{"5":1}}`},
		{"min above max", `{"count":1,"sum_ns":5,"min_ns":9,"max_ns":5,"buckets":{"5":1}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var h measure.Histogram
			if err := json.Unmarshal([]byte(tt.in), &h); err == nil {
				t.Errorf("accepted %s", tt.in)
			}
		})
	}
}

func TestHistogram_ConcurrentRecord(t *testing.T) {
	const goroutines, per = 8, 10000
	h := measure.NewHistogram()
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 1; i <= per; i++ {
				h.Record(time.Duration(g*per+i) * time.Microsecond)
				if i%1000 == 0 {
					_ = h.Quantile(0.99)
					_ = h.Count()
				}
			}
		}(g)
	}
	wg.Wait()
	if got := h.Count(); got != goroutines*per {
		t.Fatalf("Count = %d, want %d", got, goroutines*per)
	}
	if h.Min() != time.Microsecond || h.Max() != goroutines*per*time.Microsecond {
		t.Errorf("min/max = %v/%v", h.Min(), h.Max())
	}
	within(t, "p50", h.Quantile(0.5), 40000*time.Microsecond, 0.01)
}
