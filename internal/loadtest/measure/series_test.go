// SPDX-License-Identifier: AGPL-3.0-or-later

package measure_test

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/measure"
)

var t0 = time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)

func TestRecorder_SortedCopies(t *testing.T) {
	r := measure.NewRecorder()
	r.Add("zeta", "bytes", t0, 1)
	r.Add("alpha", "ms", t0, 2)
	r.Add("zeta", "ignored", t0.Add(time.Second), 3)
	r.Add("mid", "", t0, 4)

	got := r.Series()
	names := make([]string, len(got))
	for i, s := range got {
		names[i] = s.Name
	}
	if want := []string{"alpha", "mid", "zeta"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	z := got[2]
	if z.Unit != "bytes" {
		t.Errorf("zeta unit = %q, want the first Add's %q", z.Unit, "bytes")
	}
	if want := []measure.Point{{T: t0, V: 1}, {T: t0.Add(time.Second), V: 3}}; !reflect.DeepEqual(z.Points, want) {
		t.Errorf("zeta points = %v, want %v", z.Points, want)
	}

	// Mutating a returned copy must not reach the recorder.
	got[2].Points[0].V = 999
	got[0].Name = "mutated"
	again := r.Series()
	if again[2].Points[0].V != 1 || again[0].Name != "alpha" {
		t.Errorf("Series() is not a copy: %+v", again)
	}
	// And a later Add must not appear in an earlier result.
	r.Add("alpha", "ms", t0.Add(time.Minute), 5)
	if len(again[0].Points) != 1 {
		t.Errorf("earlier result grew to %d points", len(again[0].Points))
	}
}

func TestRecorder_EmptyAndNonFinite(t *testing.T) {
	r := measure.NewRecorder()
	if s := r.Series(); len(s) != 0 {
		t.Errorf("empty recorder returned %v", s)
	}
	r.Add("x", "u", t0, math.NaN())
	r.Add("x", "u", t0, math.Inf(1))
	r.Add("x", "u", t0, math.Inf(-1))
	if s := r.Series(); len(s) != 0 {
		t.Errorf("non-finite values were stored: %v", s)
	}
	r.Add("x", "u", t0, 1)
	if _, err := json.Marshal(r.Series()); err != nil {
		t.Errorf("series do not encode: %v", err)
	}
}

func TestRecorder_Concurrent(t *testing.T) {
	r := measure.NewRecorder()
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range 500 {
				r.Add(fmt.Sprintf("s%d", g%3), "u", t0.Add(time.Duration(i)*time.Second), float64(i))
				if i%100 == 0 {
					_ = r.Series()
				}
			}
		}(g)
	}
	wg.Wait()
	total := 0
	for _, s := range r.Series() {
		total += len(s.Points)
	}
	if total != 8*500 {
		t.Errorf("total points = %d, want 4000", total)
	}
}

func TestSeries_JSONRoundTrip(t *testing.T) {
	r := measure.NewRecorder()
	r.Add("rss", "bytes", t0, 1.5e9)
	r.Add("rss", "bytes", t0.Add(5*time.Second), 1.6e9)
	b, err := json.Marshal(r.Series())
	if err != nil {
		t.Fatal(err)
	}
	var back []measure.Series
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, r.Series()) {
		t.Errorf("round trip: got %+v want %+v", back, r.Series())
	}
}
