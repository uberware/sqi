// SPDX-License-Identifier: AGPL-3.0-or-later

package conformance

import (
	"fmt"

	"github.com/uberware/sqi/internal/openjd"
)

// Result is the outcome of running one conformance fixture.
type Result struct {
	// Case is the fixture that was run.
	Case TestCase
	// State is whether this result counts toward the score.
	State State
	// Accepted reports whether sqi accepted the template (parsed and validated
	// with no errors).
	Accepted bool
	// Passed reports whether the outcome matched the fixture's expectation.
	// Always false when State is StateNotApplicable.
	Passed bool
	// Reason describes the outcome, including the validation error when the
	// template was rejected. Empty when a live fixture passed.
	Reason string
}

// ID returns the fixture's stable identifier — its path relative to the suite
// root. This is what the baseline file lists.
func (r Result) ID() string { return r.Case.Path }

// RunCase parses and validates one template fixture and scores it against the
// expectation encoded in its filename.
//
// A fixture whose name lacks ".invalid" must be ACCEPTED; one carrying it must
// be REJECTED. Malformed YAML counts as a rejection — the spec's contract is
// accept-versus-reject, and it does not distinguish a parse failure from a
// validation failure.
//
// Limits are enforced: the suite tests spec-defined limits, so the enforcing
// path is the one under test.
//
// When state is StateNotApplicable the fixture is not scored at all — Passed is
// false regardless of outcome. See the StateNotApplicable doc comment.
func RunCase(tc TestCase, state State, data []byte) Result {
	res := Result{Case: tc, State: state}

	tmpl, err := openjd.Parse(data, openjd.FormatYAML)
	switch {
	case err != nil:
		res.Reason = fmt.Sprintf("parse rejected: %v", err)
	default:
		if errs := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{EnforceLimits: true}); len(errs) > 0 {
			res.Reason = fmt.Sprintf("validation rejected: %v", errs)
		} else {
			res.Accepted = true
		}
	}

	if state == StateNotApplicable {
		return res
	}

	res.Passed = res.Accepted != tc.Invalid
	if res.Passed {
		res.Reason = ""
	} else if res.Accepted {
		res.Reason = "accepted, but fixture is marked .invalid"
	}
	return res
}
