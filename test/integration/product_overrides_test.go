// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

import (
	"net/http"
	"testing"
)

// TestProductSubmit_RetryOverrides submits a job through the built-in "python"
// product with per-job retry overrides and verifies the created job carries
// them back — proving the overrides survive the full REST -> submitter ->
// store round trip against the real server binary.
func TestProductSubmit_RetryOverrides(t *testing.T) {
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)

	reqBody := []byte(`{
		"farm_id": "` + farmID + `",
		"queue_id": "` + queueID + `",
		"parameters": {"Script": "print('hi')"},
		"max_attempts": 5,
		"retry_delay_seconds": 30,
		"failure_limit": 7
	}`)

	var created struct {
		ID                string `json:"id"`
		MaxAttempts       *int   `json:"max_attempts"`
		RetryDelaySeconds *int   `json:"retry_delay_seconds"`
		FailureLimit      *int   `json:"failure_limit"`
	}
	mustDoJSON(t, http.MethodPost,
		apiURL(ts, "/api/v1/products/python/jobs"),
		reqBody, "application/json", http.StatusCreated, &created)

	if created.MaxAttempts == nil || *created.MaxAttempts != 5 {
		t.Errorf("max_attempts = %v, want 5", created.MaxAttempts)
	}
	if created.RetryDelaySeconds == nil || *created.RetryDelaySeconds != 30 {
		t.Errorf("retry_delay_seconds = %v, want 30", created.RetryDelaySeconds)
	}
	if created.FailureLimit == nil || *created.FailureLimit != 7 {
		t.Errorf("failure_limit = %v, want 7", created.FailureLimit)
	}

	// Re-fetch to confirm the values persisted, not just echoed.
	var fetched struct {
		MaxAttempts *int `json:"max_attempts"`
	}
	mustDoJSON(t, http.MethodGet,
		apiURL(ts, "/api/v1/jobs/"+created.ID),
		nil, "", http.StatusOK, &fetched)
	if fetched.MaxAttempts == nil || *fetched.MaxAttempts != 5 {
		t.Errorf("persisted max_attempts = %v, want 5", fetched.MaxAttempts)
	}
}
