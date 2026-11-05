// SPDX-License-Identifier: AGPL-3.0-or-later

package measure_test

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/measure"
)

func line(n int, step time.Duration, start, perHour float64) []measure.Point {
	pts := make([]measure.Point, n)
	for i := range pts {
		at := t0.Add(time.Duration(i) * step)
		pts[i] = measure.Point{T: at, V: start + perHour*at.Sub(t0).Hours()}
	}
	return pts
}

func TestSlope(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	noise := make([]measure.Point, 720)
	for i := range noise {
		noise[i] = measure.Point{T: t0.Add(time.Duration(i) * 5 * time.Second), V: 100 + rng.Float64() - 0.5}
	}
	// 100 points of junk before a clean line starting at the cutoff.
	cutoff := t0.Add(time.Hour)
	mixed := make([]measure.Point, 0, 200)
	for i := range 100 {
		mixed = append(mixed, measure.Point{T: t0.Add(time.Duration(i) * time.Second), V: float64(i * i)})
	}
	for i := range 100 {
		at := cutoff.Add(time.Duration(i) * time.Minute)
		mixed = append(mixed, measure.Point{T: at, V: 50 + 10*at.Sub(cutoff).Hours()})
	}

	tests := []struct {
		name      string
		points    []measure.Point
		from      time.Time
		wantSlope float64
		slopeTol  float64
		wantR2    float64
		r2Tol     float64
	}{
		{"perfect +10/hour", line(60, time.Minute, 5, 10), time.Time{}, 10, 1e-9, 1, 1e-9},
		{"perfect -2.5/hour", line(30, 2*time.Minute, 100, -2.5), time.Time{}, -2.5, 1e-9, 1, 1e-9},
		{"flat noise", noise, time.Time{}, 0, 0.5, 0, 0.05},
		{"constant has zero slope and r2 0", line(20, time.Minute, 7, 0), time.Time{}, 0, 1e-12, 0, 0},
		{"points before from are ignored", mixed, cutoff, 10, 1e-9, 1, 1e-9},
		{"from is inclusive", line(11, time.Minute, 0, 6), t0.Add(5 * time.Minute), 6, 1e-9, 1, 1e-9},
		{"from after every point", line(10, time.Minute, 0, 6), t0.Add(time.Hour), 0, 0, 0, 0},
		{"no points", nil, time.Time{}, 0, 0, 0, 0},
		{"one point", line(1, time.Minute, 3, 6), time.Time{}, 0, 0, 0, 0},
		{"one point after from", line(10, time.Minute, 0, 6), t0.Add(9 * time.Minute), 0, 0, 0, 0},
		{"identical timestamps", []measure.Point{{T: t0, V: 1}, {T: t0, V: 2}, {T: t0, V: 9}}, time.Time{}, 0, 0, 0, 0},
		{"large offset", line(10, time.Minute, 1e6, 3), t0.Add(3 * time.Minute), 3, 1e-6, 1, 1e-9},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slope, r2 := measure.Slope(tt.points, tt.from)
			if math.Abs(slope-tt.wantSlope) > tt.slopeTol {
				t.Errorf("slope = %v, want %v ± %v", slope, tt.wantSlope, tt.slopeTol)
			}
			if math.Abs(r2-tt.wantR2) > tt.r2Tol {
				t.Errorf("r2 = %v, want %v ± %v", r2, tt.wantR2, tt.r2Tol)
			}
		})
	}
}

func TestSlope_NoisyTrend(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	pts := line(2000, 5*time.Second, 1000, 40)
	for i := range pts {
		pts[i].V += (rng.Float64() - 0.5) * 2
	}
	slope, r2 := measure.Slope(pts, time.Time{})
	if math.Abs(slope-40) > 0.5 {
		t.Errorf("slope = %v, want ~40", slope)
	}
	if r2 < 0.99 || r2 > 1 {
		t.Errorf("r2 = %v, want in [0.99, 1]", r2)
	}
}
