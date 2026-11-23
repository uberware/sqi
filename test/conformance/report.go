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
	// Passed counts live tests that passed.
	Passed int
	// Baselined and Regressed split the live failures: a failure listed in the
	// baseline has been adjudicated, while one that is not is a regression.
	// Both count toward the live total.
	Baselined, Regressed int
	// NotApplicable counts tests for extensions sqi has not registered, or for
	// a document kind sqi does not implement (env_templates). These are never
	// counted in Passed; see StateNotApplicable.
	NotApplicable int
}

// Rollup tallies results per "<extension>/<kind>" directory, sorted by name.
//
// The baseline is needed to tell a baselined failure from a regression. It
// classifies through DiffBaseline, so these counts always agree with the
// REGRESSION lines a caller prints from the same diff.
func Rollup(results []Result, baseline map[string]struct{}) []Group {
	regressions, _, _ := DiffBaseline(results, baseline)
	isRegression := make(map[string]bool, len(regressions))
	for _, id := range regressions {
		isRegression[id] = true
	}

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
		case isRegression[r.ID()]:
			g.Regressed++
		default:
			g.Baselined++
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
// ratio, so an unimplemented extension or unimplemented document kind (such as
// env_templates) cannot look like a passing one. Regressions are reported as
// "REGRESSION" ahead of the baselined count, so they cannot be mistaken for
// known, accepted failures.
func FormatRollup(groups []Group) string {
	width := 0
	for _, g := range groups {
		if len(g.Name) > width {
			width = len(g.Name)
		}
	}

	var b strings.Builder
	for _, g := range groups {
		live := g.Passed + g.Baselined + g.Regressed
		switch live {
		case 0:
			fmt.Fprintf(&b, "%-*s  %s  %d n/a — not implemented\n",
				width, g.Name, strings.Repeat(" ", 9), g.NotApplicable)
		default:
			fmt.Fprintf(&b, "%-*s  %4d/%-4d pass", width, g.Name, g.Passed, live)
			if g.Regressed > 0 {
				noun := "REGRESSION"
				if g.Regressed > 1 {
					noun = "REGRESSIONS"
				}
				fmt.Fprintf(&b, "  %d %s", g.Regressed, noun)
			}
			if g.Baselined > 0 {
				fmt.Fprintf(&b, "  %d baselined", g.Baselined)
			}
			b.WriteByte('\n')
		}
	}
	return b.String()
}
