// SPDX-License-Identifier: AGPL-3.0-only

package api

// RFC 7807 problem-details error responses (task 82).
//
// All error responses from the sqi REST API use the problem+json media type
// defined in RFC 7807 (https://www.rfc-editor.org/rfc/rfc7807). Clients should
// check Content-Type: application/problem+json to detect error bodies.
//
// Current wire shape:
//
//	{
//	  "type":     "about:blank",
//	  "title":    "Not Found",
//	  "status":   404,
//	  "detail":   "job not found",
//	  "instance": "a1b2c3d4e5f60708"
//	}
//
// TODO(task 82 follow-up): replace "about:blank" with sqi-specific problem type
// URIs (e.g. "https://sqi.dev/problems/not-found") once the domain and a
// problem-type registry are established.

import (
	"encoding/json"
	"net/http"

	"github.com/uberware/sqi/internal/middleware"
)

// problemDetail is the RFC 7807 problem-details object written by writeProblem.
type problemDetail struct {
	// Type is a URI that identifies the problem type. We use "about:blank"
	// until sqi-specific problem URIs are defined (see TODO above).
	Type string `json:"type"`

	// Title is a short, human-readable summary of the problem type.
	// Populated from http.StatusText so it is consistent with the status code.
	Title string `json:"title"`

	// Status mirrors the HTTP response status code as a convenience for
	// consumers that parse the body without access to the HTTP status line.
	Status int `json:"status"`

	// Detail is a human-readable explanation specific to this occurrence.
	Detail string `json:"detail"`

	// Instance is the request ID that uniquely identifies this occurrence,
	// drawn from the X-Request-ID header / context value set by RequestLogger.
	Instance string `json:"instance,omitempty"`
}

// writeProblem writes an RFC 7807 problem-details response.
// It sets Content-Type: application/problem+json and the given HTTP status,
// then encodes a problemDetail body. detail should be a short, user-facing
// explanation of what went wrong.
func writeProblem(w http.ResponseWriter, r *http.Request, status int, detail string) {
	p := problemDetail{
		Type:     "about:blank",
		Title:    http.StatusText(status),
		Status:   status,
		Detail:   detail,
		Instance: middleware.RequestIDFromContext(r.Context()),
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(p) //nolint:errcheck // client disconnect is non-fatal
}

// writeJSON serializes v to w with the given HTTP status code and
// Content-Type: application/json. Used for all non-error responses.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v) //nolint:errcheck // client disconnect is non-fatal
}
