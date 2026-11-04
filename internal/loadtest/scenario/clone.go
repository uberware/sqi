// SPDX-License-Identifier: AGPL-3.0-or-later

package scenario

import (
	"math"
	"reflect"
	"strconv"
	"time"
)

// Clone returns a deep copy of s: changing the copy, its slices, maps and
// server configuration never changes s. Clone of nil is nil.
func (s *Scenario) Clone() *Scenario {
	if s == nil {
		return nil
	}
	c := deepCopy(reflect.ValueOf(s).Elem()).Interface().(Scenario) //nolint:errcheck,forcetypeassert // deepCopy keeps the type
	return &c
}

// MarshalYAML writes a duration the way it is read, like 1m30s.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// MarshalYAML writes a rate per second, like 2/s; the zero rate (none set) is
// null.
func (r Rate) MarshalYAML() (any, error) {
	if r.PerSecond == 0 {
		return nil, nil //nolint:nilnil // null is the YAML of an unset rate
	}
	return strconv.FormatFloat(r.PerSecond, 'g', -1, 64) + "/s", nil
}

// distYAML is the map form of a non-constant Dist, in a fixed key order.
type distYAML struct {
	Dist   string `yaml:"dist"`
	Min    any    `yaml:"min,omitempty"`
	Max    any    `yaml:"max,omitempty"`
	Median any    `yaml:"median,omitempty"`
	P95    any    `yaml:"p95,omitempty"`
}

// MarshalYAML writes a Dist in the form it is read in: a bare duration or
// number for a constant, a {dist: ...} map otherwise. The zero Dist is null.
func (d Dist) MarshalYAML() (any, error) {
	num := func(v float64) any {
		if d.Unit == "s" {
			return time.Duration(math.Round(v * float64(time.Second))).String()
		}
		return v
	}
	switch d.Kind {
	case DistConstant:
		return num(d.Value), nil
	case DistUniform:
		return distYAML{Dist: DistUniform, Min: num(d.Min), Max: num(d.Max)}, nil
	case DistLognormal:
		return distYAML{Dist: DistLognormal, Median: num(d.Median), P95: num(d.P95)}, nil
	}
	return nil, nil //nolint:nilnil // null is the YAML of an unset dist
}
