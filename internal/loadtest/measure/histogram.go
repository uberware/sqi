// SPDX-License-Identifier: AGPL-3.0-or-later

// Package measure holds the harness's measurement primitives: a mergeable
// latency histogram, atomic counters, named time series and growth slopes.
// It is a leaf package with no dependencies outside the standard library.
package measure

import (
	"encoding/json"
	"fmt"
	"math"
	"math/bits"
	"sync/atomic"
	"time"
)

const (
	// subBits is the number of linear sub-bucket bits per power of two. 128
	// sub-buckets give a bucket width of at most 1/128 (0.78%) of its lower
	// bound, so a bucket midpoint is within 0.39% of any value in the bucket.
	subBits  = 7
	subCount = 1 << subBits

	// MinTrackable and MaxTrackable bound the range the buckets resolve.
	// Values outside it land in the lowest or highest bucket; Min and Max
	// still report the true extremes.
	MinTrackable = time.Microsecond
	// MaxTrackable is the highest bucketed value; see MinTrackable.
	MaxTrackable = 24 * time.Hour
)

// numBuckets is one past the index of the highest trackable value.
var numBuckets = bucketIndex(int64(MaxTrackable)) + 1

// maxBuckets is the fixed array size; the init check below keeps it equal to
// numBuckets so the array never needs a runtime length.
const maxBuckets = 5150

func init() {
	if numBuckets != maxBuckets {
		panic(fmt.Sprintf("measure: maxBuckets = %d, want %d", maxBuckets, numBuckets))
	}
}

// bucketIndex maps a non-negative nanosecond value to its bucket.
func bucketIndex(v int64) int {
	if v < subCount {
		return int(v)
	}
	m := bits.Len64(uint64(v)) - 1
	shift := m - subBits
	return (shift+1)*subCount + int(v>>shift) - subCount
}

// bucketBounds returns the half-open range [lo, lo+width) of nanosecond
// values that map to bucket idx.
func bucketBounds(idx int) (lo, width int64) {
	if idx < subCount {
		return int64(idx), 1
	}
	shift := idx/subCount - 1
	sub := idx % subCount
	return int64(subCount+sub) << shift, int64(1) << shift
}

// Histogram is a log-linear latency histogram over 1 µs to 24 h with at most
// 1% relative error on quantiles. The zero value is an empty histogram ready
// for use. Record is safe for concurrent use and lock-free.
//
// Count, Quantile, Min, Max, Mean and Marshal may be called while Record runs
// and see a consistent-enough snapshot for monitoring. Merge and Unmarshal are
// meant for after recording has stopped; merging into h while h is recorded
// into is safe, merging from a histogram that is still being recorded into
// may miss in-flight records.
type Histogram struct {
	buckets [maxBuckets]atomic.Int64
	count   atomic.Int64
	sum     atomic.Int64 // nanoseconds
	minP1   atomic.Int64 // minimum + 1 in nanoseconds; 0 means no value yet
	max     atomic.Int64 // nanoseconds
}

// NewHistogram returns an empty histogram.
func NewHistogram() *Histogram { return &Histogram{} }

// Record adds one observation. A negative duration is recorded as zero.
func (h *Histogram) Record(d time.Duration) {
	v := max(int64(d), 0)
	h.buckets[bucketIndex(clampTrackable(v))].Add(1)
	h.sum.Add(v)
	h.observeExtremes(v, v)
	// Count is incremented last so a reader that loads it first never ranks
	// past the buckets.
	h.count.Add(1)
}

func clampTrackable(v int64) int64 {
	switch {
	case v < int64(MinTrackable):
		return int64(MinTrackable)
	case v > int64(MaxTrackable):
		return int64(MaxTrackable)
	}
	return v
}

func (h *Histogram) observeExtremes(lo, hi int64) {
	for {
		cur := h.minP1.Load()
		if cur != 0 && cur <= lo+1 {
			break
		}
		if h.minP1.CompareAndSwap(cur, lo+1) {
			break
		}
	}
	for {
		cur := h.max.Load()
		if cur >= hi {
			break
		}
		if h.max.CompareAndSwap(cur, hi) {
			break
		}
	}
}

// Count returns the number of recorded observations.
func (h *Histogram) Count() int64 { return h.count.Load() }

// Min returns the smallest recorded duration, or 0 for an empty histogram.
func (h *Histogram) Min() time.Duration {
	if h.count.Load() == 0 {
		return 0
	}
	return time.Duration(h.minP1.Load() - 1)
}

// Max returns the largest recorded duration, or 0 for an empty histogram.
func (h *Histogram) Max() time.Duration {
	if h.count.Load() == 0 {
		return 0
	}
	return time.Duration(h.max.Load())
}

// Mean returns the exact arithmetic mean, or 0 for an empty histogram.
func (h *Histogram) Mean() time.Duration {
	n := h.count.Load()
	if n == 0 {
		return 0
	}
	return time.Duration(h.sum.Load() / n)
}

// Quantile returns the q-quantile (0 <= q <= 1) as the midpoint of the bucket
// holding that rank, clamped to the observed [Min, Max]. Quantile(0) is Min,
// Quantile(1) is Max, and an empty histogram returns 0. A q outside [0, 1] or
// NaN is treated as the nearest end.
func (h *Histogram) Quantile(q float64) time.Duration {
	n := h.count.Load()
	if n == 0 {
		return 0
	}
	if math.IsNaN(q) || q <= 0 {
		return h.Min()
	}
	if q >= 1 {
		return h.Max()
	}
	rank := int64(math.Ceil(q*float64(n) - 1e-9))
	rank = min(max(rank, 1), n)
	var seen int64
	for i := range h.buckets {
		seen += h.buckets[i].Load()
		if seen >= rank {
			lo, width := bucketBounds(i)
			mid := time.Duration(lo + width/2)
			return min(max(mid, h.Min()), h.Max())
		}
	}
	return h.Max()
}

// Merge adds o's observations to h. A nil or empty o is a no-op.
func (h *Histogram) Merge(o *Histogram) {
	if o == nil {
		return
	}
	n := o.count.Load()
	if n == 0 {
		return
	}
	for i := range o.buckets {
		if c := o.buckets[i].Load(); c != 0 {
			h.buckets[i].Add(c)
		}
	}
	h.sum.Add(o.sum.Load())
	h.observeExtremes(o.minP1.Load()-1, o.max.Load())
	h.count.Add(n)
}

// histogramJSON is the wire form: only non-empty buckets, keyed by index.
type histogramJSON struct {
	Count   int64         `json:"count"`
	SumNs   int64         `json:"sum_ns"`
	MinNs   int64         `json:"min_ns"`
	MaxNs   int64         `json:"max_ns"`
	Buckets map[int]int64 `json:"buckets"`
}

// MarshalJSON writes the count, sum, extremes and non-empty buckets.
func (h *Histogram) MarshalJSON() ([]byte, error) {
	w := histogramJSON{Buckets: map[int]int64{}}
	for i := range h.buckets {
		if c := h.buckets[i].Load(); c != 0 {
			w.Buckets[i] = c
			w.Count += c
		}
	}
	if w.Count > 0 {
		w.SumNs = h.sum.Load()
		w.MinNs = h.minP1.Load() - 1
		w.MaxNs = h.max.Load()
	}
	return json.Marshal(w)
}

// UnmarshalJSON replaces h's contents with the decoded histogram. It must not
// race with Record.
func (h *Histogram) UnmarshalJSON(b []byte) error {
	var w histogramJSON
	if err := json.Unmarshal(b, &w); err != nil {
		return fmt.Errorf("histogram: %w", err)
	}
	var total int64
	for i, c := range w.Buckets {
		if i < 0 || i >= numBuckets {
			return fmt.Errorf("histogram: bucket index %d out of range [0, %d)", i, numBuckets)
		}
		if c < 0 {
			return fmt.Errorf("histogram: bucket %d has negative count %d", i, c)
		}
		total += c
	}
	if total != w.Count {
		return fmt.Errorf("histogram: count %d does not match the buckets' total %d", w.Count, total)
	}
	if w.Count > 0 && (w.MinNs < 0 || w.MinNs > w.MaxNs) {
		return fmt.Errorf("histogram: min %d and max %d are inconsistent", w.MinNs, w.MaxNs)
	}
	for i := range h.buckets {
		h.buckets[i].Store(0)
	}
	for i, c := range w.Buckets {
		h.buckets[i].Store(c)
	}
	h.sum.Store(w.SumNs)
	h.max.Store(w.MaxNs)
	if w.Count > 0 {
		h.minP1.Store(w.MinNs + 1)
	} else {
		h.minP1.Store(0)
		h.max.Store(0)
		h.sum.Store(0)
	}
	h.count.Store(w.Count)
	return nil
}

// Counter is an atomic int64 counter. The zero value is zero. It marshals as
// a plain JSON number.
type Counter struct{ v atomic.Int64 }

// Add adds n, which may be negative.
func (c *Counter) Add(n int64) { c.v.Add(n) }

// Load returns the current value.
func (c *Counter) Load() int64 { return c.v.Load() }

// MarshalJSON writes the value as a JSON number.
func (c *Counter) MarshalJSON() ([]byte, error) { return json.Marshal(c.v.Load()) }

// UnmarshalJSON sets the value from a JSON number.
func (c *Counter) UnmarshalJSON(b []byte) error {
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("counter: %w", err)
	}
	c.v.Store(n)
	return nil
}
