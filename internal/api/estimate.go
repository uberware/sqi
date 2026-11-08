// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
)

// maxEstimatedSeconds is the largest accepted job estimate; it is the largest
// value the store's integer column holds.
const maxEstimatedSeconds = math.MaxInt32

const minEstimatedSeconds = 1

var estimatedSecondsProblem = fmt.Sprintf("estimated_seconds must be an integer from %d to %d", minEstimatedSeconds, maxEstimatedSeconds)

// errEstimateNotInteger is returned while decoding a request body whose
// estimated_seconds is present but not a JSON integer (1.5, "1h", 1e10, true).
var errEstimateNotInteger = errors.New(estimatedSecondsProblem)

// bodyDecodeProblem is the 400 detail for a failed request-body decode. A
// malformed estimated_seconds names the field; any other failure is reported
// as an unreadable body.
func bodyDecodeProblem(err error) string {
	var typeErr *json.UnmarshalTypeError
	if errors.Is(err, errEstimateNotInteger) ||
		(errors.As(err, &typeErr) && typeErr.Field == "estimated_seconds") {
		return estimatedSecondsProblem
	}
	return "invalid JSON body"
}

// validateEstimatedSeconds checks a submitter's optional run-time estimate.
// Nil means "no estimate" and is valid. Returns "" when valid, else the
// problem detail for a 400 response.
func validateEstimatedSeconds(v *int) string {
	if v != nil && (*v < minEstimatedSeconds || *v > maxEstimatedSeconds) {
		return estimatedSecondsProblem
	}
	return ""
}

// parseEstimatedSecondsQuery parses the optional estimated_seconds query
// parameter. A present but empty, malformed or out-of-range value is a client
// error.
func parseEstimatedSecondsQuery(q url.Values) (estimate *int, problem string) {
	if !q.Has("estimated_seconds") {
		return nil, ""
	}
	raw := q.Get("estimated_seconds")
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
// null (Set true, Value nil) and from a value. It carries estimated_seconds,
// so a value that is not an integer decodes to [errEstimateNotInteger].
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
		return errEstimateNotInteger
	}
	o.Value = &n
	return nil
}

// MarshalJSON writes the value, or null when there is none.
func (o optionalInt) MarshalJSON() ([]byte, error) {
	return json.Marshal(o.Value)
}

// IsZero makes `omitzero` drop an unset field when a patchJobRequest is
// re-encoded (for example by tests), so an unset value is never sent as an
// explicit null, which would clear the estimate.
func (o optionalInt) IsZero() bool { return !o.Set }
