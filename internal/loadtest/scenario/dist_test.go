// SPDX-License-Identifier: AGPL-3.0-or-later

package scenario

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

func newRand(seed uint64) *rand.Rand { return rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)) }

func TestLognormal_HitsMedianAndP95(t *testing.T) {
	d := Dist{Kind: DistLognormal, Median: 20, P95: 90, Unit: "s"}
	r := newRand(1)
	const n = 100_000
	xs := make([]float64, n)
	for i := range xs {
		xs[i] = d.Sample(r)
	}
	slices.Sort(xs)
	median, p95 := xs[n/2], xs[n*95/100]
	if math.Abs(median-20)/20 > 0.03 {
		t.Errorf("empirical median %.2f is not within 3%% of 20", median)
	}
	if math.Abs(p95-90)/90 > 0.05 {
		t.Errorf("empirical p95 %.2f is not within 5%% of 90", p95)
	}
}

func TestDist_YAMLForms(t *testing.T) {
	tests := []struct {
		name    string
		doc     string
		want    Dist
		wantErr bool
	}{
		{"duration", "20s", Dist{Kind: DistConstant, Value: 20, Unit: "s"}, false},
		{"count", "200", Dist{Kind: DistConstant, Value: 200}, false},
		{"uniform", "{dist: uniform, min: 5s, max: 1m}", Dist{Kind: DistUniform, Min: 5, Max: 60, Unit: "s"}, false},
		{"lognormal", "{dist: lognormal, median: 200, p95: 2000}", Dist{Kind: DistLognormal, Median: 200, P95: 2000}, false},
		{"unknown kind", "{dist: nope}", Dist{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got Dist
			err := yaml.Unmarshal([]byte(tc.doc), &got)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("got %+v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestDist_Deterministic(t *testing.T) {
	for _, d := range []Dist{
		{Kind: DistConstant, Value: 7},
		{Kind: DistUniform, Min: 1, Max: 9},
		{Kind: DistLognormal, Median: 20, P95: 90},
	} {
		t.Run(d.Kind, func(t *testing.T) {
			a, b := newRand(42), newRand(42)
			for i := range 100 {
				if x, y := d.Sample(a), d.Sample(b); x != y {
					t.Fatalf("sample %d differs under one seed: %v vs %v", i, x, y)
				}
			}
		})
	}
}

func TestDist_Sample(t *testing.T) {
	tests := []struct {
		name   string
		d      Dist
		lo, hi float64 // every sample lies in [lo, hi]
	}{
		{"zero Dist is zero", Dist{}, 0, 0},
		{"constant", Dist{Kind: DistConstant, Value: 5, Unit: "s"}, 5, 5},
		{"uniform", Dist{Kind: DistUniform, Min: 3, Max: 8}, 3, 8},
		{"uniform degenerate", Dist{Kind: DistUniform, Min: 4, Max: 4}, 4, 4},
		{"lognormal p95 equals median", Dist{Kind: DistLognormal, Median: 12, P95: 12}, 12, 12},
		{"lognormal p95 below median", Dist{Kind: DistLognormal, Median: 12, P95: 3}, 12, 12},
		{"lognormal never negative", Dist{Kind: DistLognormal, Median: 1, P95: 1e6}, 0, math.Inf(1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRand(3)
			for range 2000 {
				if x := tc.d.Sample(r); x < tc.lo || x > tc.hi || math.IsNaN(x) {
					t.Fatalf("sample %v outside [%v, %v]", x, tc.lo, tc.hi)
				}
			}
		})
	}
}

func TestDist_UniformCoversRange(t *testing.T) {
	d := Dist{Kind: DistUniform, Min: 10, Max: 20}
	r := newRand(5)
	lo, hi := math.Inf(1), math.Inf(-1)
	for range 5000 {
		x := d.Sample(r)
		lo, hi = math.Min(lo, x), math.Max(hi, x)
	}
	if lo > 10.2 || hi < 19.8 {
		t.Errorf("5000 samples cover only [%.2f, %.2f] of [10, 20]", lo, hi)
	}
}
