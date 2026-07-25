// SPDX-License-Identifier: AGPL-3.0-or-later

package conformance

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Group is the per-directory tally reported at the end of a run.
type Group struct {
	// Name is "<extension>/<kind>", e.g. "base/job_templates".
	Name string
	// Passed and Failed count live tests only.
	Passed, Failed int
	// NotApplicable counts tests for extensions sqi has not registered. These
	// are never folded into Passed — see StateNotApplicable.
	NotApplicable int
}

// Rollup tallies results per "<extension>/<kind>" directory, sorted by name.
func Rollup(results []Result) []Group {
	byName := map[string]*Group{}
	for _, r := range results {
		name := filepath.ToSlash(filepath.Dir(r.ID()))
		g, ok := byName[name]
		if !ok {
			g = &Group{Name: name}
			byName[name] = g
		}
		switch {
		case r.State == StateNotApplicable:
			g.NotApplicable++
		case r.Passed:
			g.Passed++
		default:
			g.Failed++
		}
	}

	out := make([]Group, 0, len(byName))
	for _, g := range byName {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// FormatRollup renders groups as an aligned human-readable table.
//
// Not-applicable counts are reported in their own column and never as a pass
// ratio, so an unimplemented extension can never look like a green one.
func FormatRollup(groups []Group) string {
	width := 0
	for _, g := range groups {
		if len(g.Name) > width {
			width = len(g.Name)
		}
	}

	var b strings.Builder
	for _, g := range groups {
		live := g.Passed + g.Failed
		switch live {
		case 0:
			fmt.Fprintf(&b, "%-*s  %s  %d n/a — extension not registered\n",
				width, g.Name, strings.Repeat(" ", 9), g.NotApplicable)
		default:
			fmt.Fprintf(&b, "%-*s  %4d/%-4d pass", width, g.Name, g.Passed, live)
			if g.Failed > 0 {
				fmt.Fprintf(&b, "  %d baselined", g.Failed)
			}
			b.WriteByte('\n')
		}
	}
	return b.String()
}
