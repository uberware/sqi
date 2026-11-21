// SPDX-License-Identifier: AGPL-3.0-or-later

package chaos

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/uberware/sqi/internal/loadtest/report"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/verify"
)

// Apply returns results with a scenario's expectations applied, for the OS
// goos. It does not modify results.
//
//   - A failed result that a known violation for goos names becomes OK, with
//     Known set to the tracker (several entries for one check join theirs) and
//     its evidence kept. A result marked Unmeasured is never excused: the check
//     could not judge, so no defect fired.
//   - An entry with a match pattern covers only the violations it matches
//     (the result's Violations, or its Evidence when it has none). When every
//     entry for a check has a pattern, the failure is excused only when each
//     violation matches one; otherwise it stays failed and its evidence counts
//     the unmatched violations and names the first. A scoped entry fires only
//     when it matched a violation. None can excuse a result whose violations
//     were too many to keep (ViolationsCut), which stays failed saying so.
//   - A known violation on "rework" excuses the rework only up to its
//     MaxAttempts. Rework over every such ceiling stays failed, and its
//     evidence says the known ceiling was exceeded; an entry with no ceiling
//     excuses nothing, and neither does any entry when the count is short
//     (Undercount), since the true count may be over the ceiling.
//   - A max_rework_attempts bound adds a "rework" result after results: OK
//     when rw.Attempts is within the bound, failed when it is over, and failed
//     and Unmeasured with "rework could not be measured" when rw is nil, or
//     when rw says its count is short (Undercount) and is within the bound:
//     a count over the bound is a failure whatever it left out.
//   - A "must" violation for goos whose check held, was not run, could not
//     be measured, or failed with no violation its pattern matches adds a
//     failed "known:<check>" result, after the rest. A
//     check is not run when there is no result by its name, as "rework" when
//     no bound is set. A "may" violation that did not fire adds nothing. A
//     "must" rework entry whose ceiling was exceeded counts as fired: the
//     rework result carries that failure.
//   - An entry whose OS list leaves out goos does nothing.
func Apply(results []verify.Result, rw *report.Rework, exp scenario.Expect, goos string) []verify.Result {
	out := slices.Clone(results)
	if exp.MaxReworkAttempts != nil {
		out = append(out, reworkResult(rw, *exp.MaxReworkAttempts))
	}
	entries := slices.DeleteFunc(slices.Clone(exp.KnownViolations), func(k scenario.KnownViolation) bool {
		return len(k.OS) > 0 && !slices.Contains(k.OS, goos)
	})
	patterns := compilePatterns(entries)
	fired := make([]bool, len(entries))
	ran := make(map[string]bool, len(out))
	unmeasured := make(map[string]bool)
	failed := make(map[string]bool)
	for i := range out {
		ran[out[i].Name] = true
		if out[i].Unmeasured {
			unmeasured[out[i].Name] = true
		}
		if out[i].OK || out[i].Unmeasured {
			continue
		}
		failed[out[i].Name] = true
		if out[i].Name == scenario.ReworkCheck {
			excuse(&out[i], rw, entries, fired)
		} else {
			excuseInvariant(&out[i], entries, patterns, fired)
		}
	}
	for j, k := range entries {
		if fired[j] || k.Kind == scenario.KnownMay {
			continue
		}
		why := "it held"
		switch {
		case unmeasured[k.Check]:
			why = "it could not be measured"
		case !ran[k.Check]:
			why = "the check did not run"
		case failed[k.Check] && k.Match != "":
			why = fmt.Sprintf("it failed, but no violation matches its pattern %q", k.Match)
		}
		out = append(out, verify.Result{
			Name:     "known:" + k.Check,
			Evidence: []string{fmt.Sprintf("expected %s to fail (%s: %s), %s", k.Check, k.Tracker, k.Reason, why)},
		})
	}
	return out
}

// reworkCovered reports whether a rework entry's ceiling covers the measured
// rework. An entry with no ceiling covers none.
func reworkCovered(k scenario.KnownViolation, rw *report.Rework) bool {
	return k.MaxAttempts != nil && rw.Attempts <= *k.MaxAttempts
}

func ceilingText(k scenario.KnownViolation) string {
	if k.MaxAttempts == nil {
		return "0 (the entry has none)"
	}
	return strconv.Itoa(*k.MaxAttempts)
}

// reworkResult judges the measured rework against the scenario's bound.
func reworkResult(rw *report.Rework, bound int) verify.Result {
	res := verify.Result{Name: scenario.ReworkCheck}
	switch {
	case rw == nil:
		res.Evidence = []string{"rework could not be measured"}
		res.Unmeasured = true
	case rw.Attempts > bound:
		res.Evidence = []string{fmt.Sprintf("%d attempts ran to completion and did not count (%.1fs of work), over the bound of %d", rw.Attempts, rw.Seconds, bound)}
		if len(rw.Examples) > 0 {
			res.Evidence = append(res.Evidence, rw.Examples...)
		}
	case rw.Undercount != "":
		res.Evidence = []string{fmt.Sprintf("%d attempts counted, within the bound of %d, but the count is short: %s", rw.Attempts, bound, rw.Undercount)}
		res.Unmeasured = true
	default:
		res.OK = true
	}
	return res
}

// compilePatterns compiles each entry's match pattern, nil for an entry with
// none. Validation has compiled them already; one that does not compile
// matches nothing.
func compilePatterns(entries []scenario.KnownViolation) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(entries))
	for j, k := range entries {
		if k.Match == "" {
			continue
		}
		if re, err := regexp.Compile(k.Match); err == nil {
			out[j] = re
		} else {
			out[j] = regexp.MustCompile(`[^\s\S]`) // matches nothing
		}
	}
	return out
}

// excuseInvariant marks the failed invariant result res known when the
// entries for its check cover it, and sets fired for each entry that applies:
// an entry with no pattern covers every violation, and the scoped entries
// cover the result together when each violation matches one of them.
func excuseInvariant(res *verify.Result, entries []scenario.KnownViolation, patterns []*regexp.Regexp, fired []bool) {
	unscoped, scoped := entriesFor(res.Name, entries, patterns)
	if len(unscoped) == 0 && len(scoped) == 0 {
		return
	}
	for _, j := range unscoped {
		fired[j] = true
	}
	if why := judgeScoped(res, entries, scoped, patterns, fired); why != "" && len(unscoped) == 0 {
		res.Evidence = append(slices.Clip(res.Evidence), why)
		return
	}
	res.OK = true
	res.Known = firedTrackers(res.Name, entries, fired)
}

// entriesFor returns the indexes of the entries for check, split into those
// with no pattern and those scoped by one.
func entriesFor(check string, entries []scenario.KnownViolation, patterns []*regexp.Regexp) (unscoped, scoped []int) {
	for j, k := range entries {
		switch {
		case k.Check != check:
		case patterns[j] == nil:
			unscoped = append(unscoped, j)
		default:
			scoped = append(scoped, j)
		}
	}
	return unscoped, scoped
}

// judgeScoped matches res's violations against the scoped entries, setting
// fired for each that matched one, and returns why they do not cover res, or
// "" when they do. With no scoped entry there is nothing to judge.
func judgeScoped(res *verify.Result, entries []scenario.KnownViolation, scoped []int, patterns []*regexp.Regexp, fired []bool) string {
	if len(scoped) == 0 {
		return ""
	}
	if res.ViolationsCut {
		// No pattern can judge the violations, and the result says so; a
		// must entry is not also reported as having matched nothing.
		for _, j := range scoped {
			fired[j] = true
		}
		return fmt.Sprintf("too many violations (more than %d) to check against the known violations' patterns", verify.MaxKeptViolations)
	}
	lines := res.Violations
	if lines == nil {
		lines = res.Evidence
	}
	uncovered := matchViolations(lines, scoped, patterns, fired)
	if len(uncovered) == 0 && len(lines) > 0 {
		return ""
	}
	return fmt.Sprintf("%d of %d violations match no known violation's pattern (%s); the first: %s",
		len(uncovered), len(lines), patternsText(entries, scoped), firstOr(uncovered, "(none)"))
}

// firedTrackers joins the trackers of the fired entries for check, in entry
// order.
func firedTrackers(check string, entries []scenario.KnownViolation, fired []bool) string {
	var trackers []string
	for j, k := range entries {
		if fired[j] && k.Check == check && !slices.Contains(trackers, k.Tracker) {
			trackers = append(trackers, k.Tracker)
		}
	}
	return strings.Join(trackers, ", ")
}

// matchViolations returns the lines no scoped entry's pattern matches, and
// sets fired for each scoped entry that matched one.
func matchViolations(lines []string, scoped []int, patterns []*regexp.Regexp, fired []bool) []string {
	var uncovered []string
	for _, line := range lines {
		covered := false
		for _, j := range scoped {
			if patterns[j].MatchString(line) {
				fired[j], covered = true, true
			}
		}
		if !covered {
			uncovered = append(uncovered, line)
		}
	}
	return uncovered
}

func patternsText(entries []scenario.KnownViolation, scoped []int) string {
	parts := make([]string, 0, len(scoped))
	for _, j := range scoped {
		parts = append(parts, fmt.Sprintf("%s: %q", entries[j].Tracker, entries[j].Match))
	}
	return strings.Join(parts, ", ")
}

func firstOr(ss []string, fallback string) string {
	if len(ss) == 0 {
		return fallback
	}
	return ss[0]
}

// excuse marks the failed rework result res known when an entry for it
// covers it, and sets fired for each entry that applies. A rework entry
// covers the result only when rw is within its ceiling; when none does, the
// result stays failed and says each ceiling it went over.
func excuse(res *verify.Result, rw *report.Rework, entries []scenario.KnownViolation, fired []bool) {
	var trackers, within, over []string
	for j, k := range entries {
		if k.Check != res.Name {
			continue
		}
		fired[j] = true
		if k.Check == scenario.ReworkCheck && rw != nil && !reworkCovered(k, rw) {
			over = append(over, fmt.Sprintf("%d attempts is over the known ceiling of %s for %s", rw.Attempts, ceilingText(k), k.Tracker))
			continue
		}
		if k.Check == scenario.ReworkCheck && rw != nil && rw.Undercount != "" {
			over = append(over, fmt.Sprintf("%d attempts counted, but the count is short (%s), so it cannot be shown within the known ceiling of %s for %s",
				rw.Attempts, rw.Undercount, ceilingText(k), k.Tracker))
			continue
		}
		if !slices.Contains(trackers, k.Tracker) {
			trackers = append(trackers, k.Tracker)
		}
		if k.Check == scenario.ReworkCheck {
			within = append(within, fmt.Sprintf("within the known ceiling of %s for %s", ceilingText(k), k.Tracker))
		}
	}
	if len(trackers) > 0 {
		res.OK = true
		res.Known = strings.Join(trackers, ", ")
		res.Evidence = append(slices.Clip(res.Evidence), within...)
		return
	}
	res.Evidence = append(slices.Clip(res.Evidence), over...)
}
