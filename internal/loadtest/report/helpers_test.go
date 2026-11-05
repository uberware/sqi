// SPDX-License-Identifier: AGPL-3.0-or-later

package report

import (
	"time"

	"github.com/uberware/sqi/internal/loadtest/measure"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/verify"
)

var t0 = time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)

// hist returns a histogram holding one observation per argument.
func hist(ds ...time.Duration) *measure.Histogram {
	h := measure.NewHistogram()
	for _, d := range ds {
		h.Record(d)
	}
	return h
}

// ramp returns a series of n points one step apart starting at t0, with value
// start + i*inc.
func ramp(name, unit string, n int, step time.Duration, start, inc float64) measure.Series {
	s := measure.Series{Name: name, Unit: unit}
	for i := range n {
		s.Points = append(s.Points, measure.Point{T: t0.Add(time.Duration(i) * step), V: start + float64(i)*inc})
	}
	return s
}

// values returns a series with the given values one step apart from t0.
func values(name, unit string, step time.Duration, vs ...float64) measure.Series {
	s := measure.Series{Name: name, Unit: unit}
	for i, v := range vs {
		s.Points = append(s.Points, measure.Point{T: t0.Add(time.Duration(i) * step), V: v})
	}
	return s
}

// baseRecord is a small valid record: a ten minute run with two minutes of
// warmup and no scenario churn.
func baseRecord() *Record {
	return &Record{
		RunID:    "run-1",
		Mode:     "load",
		Complete: true,
		Valid:    true,
		Scenario: &scenario.Scenario{
			Name:    "unit",
			Warmup:  scenario.Duration{Duration: 2 * time.Minute},
			Workers: []scenario.WorkerGroup{{Name: "w", Count: 100}},
		},
		Started:    t0,
		Ended:      t0.Add(10 * time.Minute),
		Histograms: map[string]*measure.Histogram{},
		Counters:   map[string]int64{},
	}
}

func okResult(name string) verify.Result { return verify.Result{Name: name, OK: true} }
