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
	// Baselined and Regressed split the live FAILURES, and the split is the
	// whole point of this type: a failure listed in the baseline has been
	// adjudicated and written down, while one that is not is a break. Both are
	// failures and both are counted in the live total; only one of them is news.
	//
	// They were a single Failed field until 2026-08-19, and FormatRollup
	// labeled all of it "baselined" — so the run that first met an unlisted
	// failure summarized it as "449/450 pass  1 baselined" directly above the
	// line calling the same fixture a REGRESSION.
	Baselined, Regressed int
	// NotApplicable counts tests for extensions sqi has not registered, or for
	// a document kind sqi does not implement at all (env_templates). These
	// are never folded into Passed — see StateNotApplicable.
	NotApplicable int
}

// Rollup tallies results per "<extension>/<kind>" directory, sorted by name.
//
// The baseline is a parameter rather than something the caller applies
// afterwards because the tally cannot be honest without it: "failed" and
// "failed in a way we already accepted" are different facts about a run.
//
// It classifies through DiffBaseline rather than re-deriving membership from
// the map, so the counts here and the REGRESSION lines a caller prints from the
// same diff are one judgement rendered twice. Re-deriving would be two lines of
// code and a standing opportunity for them to disagree.
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
// env_templates) can never look like a green one. Regressions are reported in
// their own words and in CAPITALS, ahead of the baselined count, for the same
// reason: this table is the first thing a reader sees, and the one number on it
// that means "something broke" must not be spelled like the one that means
// "something is known and accepted".
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
