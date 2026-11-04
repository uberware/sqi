// SPDX-License-Identifier: AGPL-3.0-or-later

package scenario

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Distribution kinds.
const (
	DistConstant  = "constant"
	DistUniform   = "uniform"
	DistLognormal = "lognormal"
)

// Dist is a value a task draws from a distribution: how long it runs, how many
// log lines it writes. It is written as a bare duration (20s) or number (200),
// meaning a constant, or as a map:
//
//	{dist: uniform, min: 5s, max: 1m}
//	{dist: lognormal, median: 20s, p95: 90s}
//
// A map without dist is lognormal when it has median and p95, and uniform when
// it has min and max. Durations are stored as seconds with Unit "s"; plain
// numbers have an empty Unit. The zero Dist means unset.
type Dist struct {
	Kind                         string
	Value, Min, Max, Median, P95 float64
	Unit                         string
}

// distFields lists, per kind, the keys it takes besides dist.
var distFields = map[string][]string{
	DistConstant:  {"value"},
	DistUniform:   {"min", "max"},
	DistLognormal: {"median", "p95"},
}

// UnmarshalYAML parses every form Dist is written in.
func (d *Dist) UnmarshalYAML(n *yaml.Node) error {
	var (
		v   Dist
		err error
	)
	switch n.Kind {
	case yaml.ScalarNode:
		v, err = distFromScalar(n)
	case yaml.MappingNode:
		v, err = distFromMap(n)
	default:
		err = fmt.Errorf("line %d: a dist is a duration, a number or a {dist: ...} map", n.Line)
	}
	if err != nil {
		return err
	}
	*d = v
	return nil
}

func distFromScalar(n *yaml.Node) (Dist, error) {
	val, unit, err := parseDistNumber(n)
	if err != nil {
		return Dist{}, err
	}
	if val < 0 {
		return Dist{}, fmt.Errorf("line %d: %q is negative", n.Line, n.Value)
	}
	return Dist{Kind: DistConstant, Value: val, Unit: unit}, nil
}

// parseDistNumber reads a plain number (empty unit) or a duration (unit "s",
// value in seconds).
func parseDistNumber(n *yaml.Node) (val float64, unit string, err error) {
	if n.Kind != yaml.ScalarNode {
		return 0, "", fmt.Errorf("line %d: expected a number or a duration", n.Line)
	}
	s := strings.TrimSpace(n.Value)
	if f, perr := strconv.ParseFloat(s, 64); perr == nil {
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, "", fmt.Errorf("line %d: %q is not a finite number", n.Line, s)
		}
		return f, "", nil
	}
	dur, perr := time.ParseDuration(s)
	if perr != nil {
		return 0, "", fmt.Errorf("line %d: %q is neither a number nor a duration like 20s", n.Line, s)
	}
	return dur.Seconds(), "s", nil
}

func distFromMap(n *yaml.Node) (Dist, error) {
	fields := make(map[string]*yaml.Node, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		key := n.Content[i].Value
		if _, dup := fields[key]; dup {
			return Dist{}, fmt.Errorf("line %d: duplicate key %q", n.Content[i].Line, key)
		}
		fields[key] = n.Content[i+1]
	}

	kind, err := distKind(n, fields)
	if err != nil {
		return Dist{}, err
	}
	allowed := distFields[kind]
	for key, node := range fields {
		if key != "dist" && !slices.Contains(allowed, key) {
			return Dist{}, fmt.Errorf("line %d: %q does not belong to a %s dist (it takes %s)",
				node.Line, key, kind, strings.Join(allowed, ", "))
		}
	}

	d := Dist{Kind: kind}
	targets := map[string]*float64{"value": &d.Value, "min": &d.Min, "max": &d.Max, "median": &d.Median, "p95": &d.P95}
	for i, key := range allowed {
		node, ok := fields[key]
		if !ok {
			return Dist{}, fmt.Errorf("line %d: a %s dist needs %q", n.Line, kind, key)
		}
		val, unit, err := parseDistNumber(node)
		if err != nil {
			return Dist{}, err
		}
		if i == 0 {
			d.Unit = unit
		} else if unit != d.Unit {
			return Dist{}, fmt.Errorf("line %d: %q mixes a duration with a plain number; use one unit throughout", node.Line, key)
		}
		*targets[key] = val
	}
	if err := d.check(n.Line); err != nil {
		return Dist{}, err
	}
	return d, nil
}

func distKind(n *yaml.Node, fields map[string]*yaml.Node) (string, error) {
	if node, ok := fields["dist"]; ok {
		kind := strings.TrimSpace(node.Value)
		if _, known := distFields[kind]; !known {
			return "", fmt.Errorf("line %d: unknown dist %q (want constant, uniform or lognormal)", node.Line, kind)
		}
		return kind, nil
	}
	_, hasMedian := fields["median"]
	_, hasP95 := fields["p95"]
	_, hasMin := fields["min"]
	_, hasMax := fields["max"]
	switch {
	case hasMedian && hasP95:
		return DistLognormal, nil
	case hasMin && hasMax:
		return DistUniform, nil
	}
	return "", fmt.Errorf("line %d: cannot tell the dist: give median and p95, or min and max, or set the dist key", n.Line)
}

// check rejects values no distribution of this kind can have.
func (d Dist) check(line int) error {
	switch d.Kind {
	case DistConstant:
		if d.Value < 0 {
			return fmt.Errorf("line %d: value is negative", line)
		}
	case DistUniform:
		if d.Min < 0 || d.Max < d.Min {
			return fmt.Errorf("line %d: uniform needs 0 <= min <= max, got min %v, max %v", line, d.Min, d.Max)
		}
	case DistLognormal:
		if d.Median <= 0 {
			return fmt.Errorf("line %d: lognormal needs median > 0, got %v", line, d.Median)
		}
		if d.P95 < d.Median {
			return fmt.Errorf("line %d: lognormal needs p95 >= median, got p95 %v, median %v", line, d.P95, d.Median)
		}
	}
	return nil
}
