// SPDX-License-Identifier: AGPL-3.0-only

package middleware

import (
	"net/http"
	"time"
)

// APIVersion returns middleware that sets the X-API-Version response header on
// every response so clients can verify which version of the sqi API they are
// talking to without inspecting the URL.
//
// The header is added to the response map before the inner handler runs, so it
// is present even on error responses. Go's net/http flushes all headers that
// were Set/Add before the first Write or WriteHeader call, which is why setting
// headers prior to calling next.ServeHTTP is the correct pattern here.
//
// Usage (applied to a chi sub-router):
//
//	r.Route("/api/v1", func(api chi.Router) {
//	    api.Use(middleware.APIVersion("1"))
//	    // … routes …
//	})
func APIVersion(version string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-API-Version", version)
			next.ServeHTTP(w, r)
		})
	}
}

// Deprecated returns middleware that marks an endpoint as deprecated by
// injecting the standard HTTP deprecation headers defined in RFC 8594.
//
// Headers set:
//
//   - Deprecation: <RFC 7231 HTTP-date> — when the deprecation was declared.
//     Use [time.Time] zero value to emit "true" instead of a date, which is
//     valid per the RFC when the declaration date is not meaningful.
//   - Sunset: <RFC 7231 HTTP-date> — when the endpoint will be removed.
//     Omitted when sunsetDate is the zero value.
//   - Link: <link>; rel="deprecation" — URL of documentation describing the
//     deprecation and migration path. Omitted when link is empty. link must be
//     a valid absolute URL; a syntactically invalid value (e.g. one containing
//     an unescaped '>') will produce a malformed Link header.
//
// Wrap individual handlers (not an entire route group) with Deprecated so that
// only the specific endpoints that are being retired carry these headers.
//
// Both chi usage patterns work:
//
//	// Pattern 1: inline wrap
//	api.Get("/v1/old-endpoint", middleware.Deprecated(
//	    time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
//	    time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
//	    "https://docs.sqi.dev/api/deprecations#old-endpoint",
//	)(oldHandler))
//
//	// Pattern 2: chi.With (preferred when combining multiple per-route middleware)
//	api.With(middleware.Deprecated(
//	    time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
//	    time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
//	    "https://docs.sqi.dev/api/deprecations#old-endpoint",
//	)).Get("/v1/old-endpoint", oldHandler)
func Deprecated(declaredAt, sunsetDate time.Time, link string) func(http.Handler) http.Handler {
	// Pre-compute header values so the closure does no allocation per request.
	var deprecationValue string
	if declaredAt.IsZero() {
		deprecationValue = "true"
	} else {
		deprecationValue = declaredAt.UTC().Format(http.TimeFormat)
	}

	var sunsetValue string
	if !sunsetDate.IsZero() {
		sunsetValue = sunsetDate.UTC().Format(http.TimeFormat)
	}

	var linkValue string
	if link != "" {
		linkValue = "<" + link + ">; rel=\"deprecation\""
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Deprecation", deprecationValue)
			if sunsetValue != "" {
				h.Set("Sunset", sunsetValue)
			}
			if linkValue != "" {
				h.Add("Link", linkValue)
			}
			next.ServeHTTP(w, r)
		})
	}
}
