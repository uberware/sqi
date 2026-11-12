// SPDX-License-Identifier: AGPL-3.0-or-later

package chaos

import (
	"fmt"
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
//   - A known violation on "rework" excuses the rework only up to its
//     MaxAttempts. Rework over every such ceiling stays failed, and its
//     evidence says the known ceiling was exceeded; an entry with no ceiling
//     excuses nothing.
//   - Max_rework_attempts adds a "rework" result after the invariants: OK
//     when rw.Attempts is within the bound, failed when it is over, and failed
//     and Unmeasured with "rework could not be measured" when rw is nil, or
//     when rw says its count is short (Undercount) and is within the bound:
//     a count over the bound is a failure whatever it left out.
//   - A "must" violation for goos whose check held, was not run or could not
//     be measured adds a failed "known:<check>" result, after the rest. A
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
	fired := make([]bool, len(entries))
	ran := make(map[string]bool, len(out))
	unmeasured := make(map[string]bool)
	for i := range out {
		ran[out[i].Name] = true
		if out[i].Unmeasured {
			unmeasured[out[i].Name] = true
		}
		if out[i].OK || out[i].Unmeasured {
			continue
		}
		excuse(&out[i], rw, entries, fired)
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

// excuse marks the failed result res known when an entry for its check
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
