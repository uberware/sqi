// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"bytes"
	"encoding/json"
	"math"
	"net/url"
	"strconv"
)

// maxEstimatedSeconds is the largest accepted job estimate; it is the largest
// value the store's integer column holds.
const maxEstimatedSeconds = math.MaxInt32

const estimatedSecondsProblem = "estimated_seconds must be an integer from 1 to 2147483647"

// validateEstimatedSeconds checks a submitter's optional run-time estimate.
// Nil means "no estimate" and is valid. Returns "" when valid, else the
// problem detail for a 400 response.
func validateEstimatedSeconds(v *int) string {
	if v != nil && (*v < 1 || *v > maxEstimatedSeconds) {
		return estimatedSecondsProblem
	}
	return ""
}

// parseEstimatedSecondsQuery parses the optional estimated_seconds query
// parameter. A present but malformed or out-of-range value is a client error.
func parseEstimatedSecondsQuery(q url.Values) (estimate *int, problem string) {
	raw := q.Get("estimated_seconds")
	if raw == "" {
		return nil, ""
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return nil, estimatedSecondsProblem
	}
	if problem := validateEstimatedSeconds(&n); problem != "" {
		return nil, problem
	}
	return &n, ""
}

// optionalInt distinguishes an omitted JSON key (Set false) from an explicit
// null (Set true, Value nil) and from a value.
type optionalInt struct {
	Set   bool
	Value *int
}

// UnmarshalJSON records that the key was present.
func (o *optionalInt) UnmarshalJSON(b []byte) error {
	o.Set = true
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		o.Value = nil
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	o.Value = &n
	return nil
}

// MarshalJSON writes the value, or null when there is none.
func (o optionalInt) MarshalJSON() ([]byte, error) {
	return json.Marshal(o.Value)
}

// The omitzero behavior below is load-bearing: encoding a patchJobRequest in
// tests must not send an explicit null, which would clear the estimate.
// IsZero lets `omitzero` drop an unset field when a request is encoded.
func (o optionalInt) IsZero() bool { return !o.Set }
