// SPDX-License-Identifier: AGPL-3.0-or-later

package measure

import (
	"math"
	"slices"
	"strings"
	"sync"
	"time"
)

// Point is one sample of a series.
type Point struct {
	T time.Time `json:"t"`
	V float64   `json:"v"`
}

// Series is a named, ordered sequence of samples with a display unit.
type Series struct {
	Name   string  `json:"name"`
	Unit   string  `json:"unit"`
	Points []Point `json:"points"`
}

// Recorder collects samples into named series. The zero value is ready for
// use and Add is safe for concurrent use.
type Recorder struct {
	mu     sync.Mutex
	series map[string]*Series
}

// NewRecorder returns an empty recorder.
func NewRecorder() *Recorder { return &Recorder{} }

// Add appends a sample to the named series, creating it on first use. The
// series' unit is fixed by its first Add; later units for the same name are
// ignored. Samples are kept in call order. A NaN or infinite value is dropped,
// because JSON cannot represent it and it would make run.json unwritable.
func (r *Recorder) Add(name, unit string, t time.Time, v float64) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.series == nil {
		r.series = map[string]*Series{}
	}
	s, ok := r.series[name]
	if !ok {
		s = &Series{Name: name, Unit: unit}
		r.series[name] = s
	}
	s.Points = append(s.Points, Point{T: t, V: v})
}

// Series returns a deep copy of every series, sorted by name.
func (r *Recorder) Series() []Series {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Series, 0, len(r.series))
	for _, s := range r.series {
		out = append(out, Series{Name: s.Name, Unit: s.Unit, Points: slices.Clone(s.Points)})
	}
	slices.SortFunc(out, func(a, b Series) int { return strings.Compare(a.Name, b.Name) })
	return out
}
