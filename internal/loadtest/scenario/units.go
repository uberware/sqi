// SPDX-License-Identifier: AGPL-3.0-or-later

package scenario

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration written as a Go duration string ("90s", "1h30m").
type Duration struct{ time.Duration }

// UnmarshalYAML parses a Go duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: a duration is written like 90s or 1h30m", n.Line)
	}
	v, err := time.ParseDuration(strings.TrimSpace(n.Value))
	if err != nil {
		return fmt.Errorf("line %d: duration %q: %w", n.Line, n.Value, err)
	}
	d.Duration = v
	return nil
}

// Rate is an events-per-second figure written as "2/s", "120/m" or "7200/h".
// The zero value means no rate is set.
type Rate struct{ PerSecond float64 }

// ParseRate parses "<positive number>/<s|m|h>".
func ParseRate(s string) (Rate, error) {
	num, unit, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok {
		return Rate{}, fmt.Errorf("rate %q: want <number>/<s|m|h>, like 2/s or 120/m", s)
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 {
		return Rate{}, fmt.Errorf("rate %q: %q is not a positive number", s, num)
	}
	var seconds float64
	switch strings.TrimSpace(unit) {
	case "s":
		seconds = 1
	case "m":
		seconds = 60
	case "h":
		seconds = 3600
	default:
		return Rate{}, fmt.Errorf("rate %q: unit %q is not one of s, m, h", s, unit)
	}
	return Rate{PerSecond: n / seconds}, nil
}

// UnmarshalYAML parses a rate string.
func (r *Rate) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: a rate is written like 2/s or 120/m", n.Line)
	}
	v, err := ParseRate(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*r = v
	return nil
}
