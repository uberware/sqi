// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

// SubmitValidationError is returned by [Submitter.Submit] when the job
// template fails to parse, fails OpenJD validation, or references storage
// locations that are not registered or lack the required root coverage.
//
// The REST layer maps this error type to HTTP 422 (Unprocessable Entity) via
// [errors.As] so that callers get a precise status code regardless of how the
// underlying error message is phrased.
type SubmitValidationError struct {
	// Cause is the underlying parse, validation, or storage-location error.
	Cause error
}

func (e *SubmitValidationError) Error() string { return e.Cause.Error() }
func (e *SubmitValidationError) Unwrap() error { return e.Cause }
