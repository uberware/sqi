// SPDX-License-Identifier: AGPL-3.0-or-later

package scenario

import (
	"maps"
	"math"
	"slices"
	"strconv"
	"time"
)

// Clone returns a deep copy of s: changing the copy, its slices, maps and
// server configuration never changes s. Clone of nil is nil.
func (s *Scenario) Clone() *Scenario {
	if s == nil {
		return nil
	}
	c := *s
	c.Server.Config = cloneMap(s.Server.Config)
	c.Farm.Queues = slices.Clone(s.Farm.Queues)
	c.Farm.UsagePools = slices.Clone(s.Farm.UsagePools)
	c.Workers = slices.Clone(s.Workers)
	for i := range c.Workers {
		c.Workers[i].Tags = maps.Clone(s.Workers[i].Tags)
		c.Workers[i].Queues = slices.Clone(s.Workers[i].Queues)
	}
	c.Jobs = slices.Clone(s.Jobs)
	c.Clients.WebSocket.Subscribe = slices.Clone(s.Clients.WebSocket.Subscribe)
	c.Clients.RESTPollers.Endpoints = slices.Clone(s.Clients.RESTPollers.Endpoints)
	c.Actions = slices.Clone(s.Actions)
	for i := range c.Actions {
		c.Actions[i].CancelJobs = cloneFraction(s.Actions[i].CancelJobs)
		c.Actions[i].DisableWorkers = cloneFraction(s.Actions[i].DisableWorkers)
	}
	c.Assert = maps.Clone(s.Assert)
	return &c
}

func cloneFraction(f *Fraction) *Fraction {
	if f == nil {
		return nil
	}
	c := *f
	return &c
}

// cloneMap deep-copies a decoded YAML mapping: nested mappings and sequences
// are copied, scalars are shared (they are immutable).
func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneValue(v)
	}
	return out
}

func cloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneMap(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = cloneValue(e)
		}
		return out
	default:
		return v
	}
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
