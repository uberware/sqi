// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

// RFC 7807 problem-details error responses.
//
// All error responses from the sqi HTTP surface use the problem+json media
// type defined in RFC 7807 (https://www.rfc-editor.org/rfc/rfc7807). Clients
// should check Content-Type: application/problem+json to detect error bodies.
//
// The canonical writer lives here (rather than in internal/api) so that
// middleware-emitted errors — e.g. the rate limiter's 429 — share one
// implementation with the REST handlers without inverting the dependency
// direction (api imports middleware, never the reverse).
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
// TODO: replace "about:blank" with sqi-specific problem type
// URIs (e.g. "https://sqi.dev/problems/not-found") once the domain and a
// problem-type registry are established.

import (
	"encoding/json"
	"net/http"
)

// problemDetail is the RFC 7807 problem-details object written by WriteProblem.
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

// WriteProblem writes an RFC 7807 problem-details response.
// It sets Content-Type: application/problem+json and the given HTTP status,
// then encodes a problemDetail body. detail should be a short, user-facing
// explanation of what went wrong.
func WriteProblem(w http.ResponseWriter, r *http.Request, status int, detail string) {
	p := problemDetail{
		Type:     "about:blank",
		Title:    http.StatusText(status),
		Status:   status,
		Detail:   detail,
		Instance: RequestIDFromContext(r.Context()),
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(p) //nolint:errcheck // client disconnect is non-fatal
}
