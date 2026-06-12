// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// RFC 7807 problem-details error responses (task 82).
//
// All error responses from the sqi REST API use the problem+json media type
// defined in RFC 7807 (https://www.rfc-editor.org/rfc/rfc7807). Clients should
// check Content-Type: application/problem+json to detect error bodies.
//
// The canonical writer is [middleware.WriteProblem] (it also serves
// middleware-emitted errors such as the rate limiter's 429); the wire shape
// and the problem-type TODO are documented there.

import (
	"encoding/json"
	"net/http"

	"github.com/uberware/sqi/internal/middleware"
)

// writeProblem writes an RFC 7807 problem-details response.
// It sets Content-Type: application/problem+json and the given HTTP status,
// then encodes the problem body. detail should be a short, user-facing
// explanation of what went wrong.
func writeProblem(w http.ResponseWriter, r *http.Request, status int, detail string) {
	middleware.WriteProblem(w, r, status, detail)
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
