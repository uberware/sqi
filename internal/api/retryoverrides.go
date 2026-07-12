// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"net/url"
	"strconv"
)

// validateRetryOverrides checks the optional retry-policy override fields
// shared by farm/queue/job writes and job submission. Bounds mirror the
// server-level config validation (internal/config): max_attempts >= 1,
// retry_delay_seconds >= 0, failure_limit >= 0 (an explicit 0 disables an
// inherited limit). Nil means "inherit" and is always valid. Returns "" when
// valid, else the problem detail for a 400 response.
func validateRetryOverrides(maxAttempts, retryDelaySeconds, failureLimit *int) string {
	switch {
	case maxAttempts != nil && *maxAttempts < 1:
		return "max_attempts must be >= 1"
	case retryDelaySeconds != nil && *retryDelaySeconds < 0:
		return "retry_delay_seconds must be >= 0"
	case failureLimit != nil && *failureLimit < 0:
		return "failure_limit must be >= 0 (0 disables an inherited limit)"
	}
	return ""
}

// parseRetryOverridesQuery parses and validates the three optional
// retry-policy override query parameters used by POST /api/v1/jobs. Absent
// parameters mean "inherit" (nil); a present but non-integer or out-of-range
// value is a client error, never silently ignored — a typo must not quietly
// submit a job with different retry behavior than the caller intended.
// Returns the problem detail for a 400 response, or "" when valid.
func parseRetryOverridesQuery(q url.Values) (maxAttempts, retryDelaySeconds, failureLimit *int, problem string) {
	for _, p := range []struct {
		name string
		dst  **int
	}{
		{"max_attempts", &maxAttempts},
		{"retry_delay_seconds", &retryDelaySeconds},
		{"failure_limit", &failureLimit},
	} {
		raw := q.Get(p.name)
		if raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			return nil, nil, nil, p.name + " must be an integer"
		}
		*p.dst = &n
	}
	return maxAttempts, retryDelaySeconds, failureLimit, validateRetryOverrides(maxAttempts, retryDelaySeconds, failureLimit)
}
