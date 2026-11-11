// SPDX-License-Identifier: AGPL-3.0-or-later

package chaos

import (
	"fmt"
	"slices"
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
//     its evidence kept.
//   - Max_rework_attempts adds a "rework" result after the invariants: OK
//     when rw.Attempts is within the bound, failed when it is over, and failed
//     with "rework could not be measured" when rw is nil. Known violations
//     apply to it like any other check.
//   - A "must" violation for goos whose check held, or was not run, adds a
//     failed "known:<check>" result, after the rest. A check is not run when
//     there is no result by its name, as "rework" when no bound is set. A
//     "may" violation that did not fire adds nothing.
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
	for i := range out {
		ran[out[i].Name] = true
		if out[i].OK {
			continue
		}
		var trackers []string
		for j, k := range entries {
			if k.Check != out[i].Name {
				continue
			}
			fired[j] = true
			if !slices.Contains(trackers, k.Tracker) {
				trackers = append(trackers, k.Tracker)
			}
		}
		if len(trackers) > 0 {
			out[i].OK = true
			out[i].Known = strings.Join(trackers, ", ")
		}
	}
	for j, k := range entries {
		if fired[j] || k.Kind == scenario.KnownMay {
			continue
		}
		why := "it held"
		if !ran[k.Check] {
			why = "the check did not run"
		}
		out = append(out, verify.Result{
			Name:     "known:" + k.Check,
			Evidence: []string{fmt.Sprintf("expected %s to fail (%s: %s), %s", k.Check, k.Tracker, k.Reason, why)},
		})
	}
	return out
}

// reworkResult judges the measured rework against the scenario's bound.
func reworkResult(rw *report.Rework, bound int) verify.Result {
	res := verify.Result{Name: scenario.ReworkCheck}
	switch {
	case rw == nil:
		res.Evidence = []string{"rework could not be measured"}
	case rw.Attempts > bound:
		res.Evidence = []string{fmt.Sprintf("%d attempts ran to completion and did not count (%.1fs of work), over the bound of %d", rw.Attempts, rw.Seconds, bound)}
		if len(rw.Examples) > 0 {
			res.Evidence = append(res.Evidence, rw.Examples...)
		}
	default:
		res.OK = true
	}
	return res
}
