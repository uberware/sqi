// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

// CSRF is an Origin-based CSRF guard for state-changing requests.
//
// # Why this exists
//
// A1 introduces an HttpOnly session cookie. Cookies are ambient credentials:
// the browser attaches them automatically to any request to this origin,
// including ones initiated by a malicious third-party page (a classic CSRF
// attack). Requests authenticated some other way (no cookie present — e.g. a
// future bearer-token client, or the pre-cookie login call itself) carry no
// ambient credential and are not in scope for this guard.
//
// # Policy
//
//   - Safe methods (GET/HEAD/OPTIONS) are never checked — they must not have
//     side effects per HTTP semantics.
//   - A request without the session cookie passes through unchecked: it is not
//     cookie-authenticated, so there is no ambient-credential vector to guard.
//   - An unsafe-method request WITH the session cookie must carry an Origin
//     (falling back to Referer's origin) that is either same-origin (matches
//     r.Host) or explicitly allow-listed. Otherwise the request is rejected
//     with 403.
//   - A cookie-bearing unsafe request with no Origin or Referer at all is
//     rejected: browsers always send Origin on cross-origin requests and on
//     same-origin unsafe requests, so its absence is not something to trust.
//
// Mount only when auth is enabled (see internal/api/router.go) — with no
// session cookie in play there is nothing for this guard to protect.

import (
	"log/slog"
	"net/http"
	"net/url"
	"slices"
)

// CSRFConfig configures the Origin-based CSRF guard.
type CSRFConfig struct {
	// CookieName is the session cookie whose presence marks a request as
	// cookie-authenticated (and therefore CSRF-relevant). Mirrors
	// api.Deps.CookieName / config.AuthConfig.Session.CookieName.
	CookieName string

	// AllowedOrigins are additional acceptable Origin values for
	// cookie-authenticated cross-origin mutations, beyond same-origin (which
	// is always allowed). Typically cfg.CORSOrigins.
	AllowedOrigins []string
}

// CSRF returns middleware enforcing the policy described in the package-level
// doc comment above.
func CSRF(cfg CSRFConfig, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isSafeMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			if _, err := r.Cookie(cfg.CookieName); err != nil {
				next.ServeHTTP(w, r) // not cookie-authenticated — no CSRF vector
				return
			}
			if originAllowed(r, cfg.AllowedOrigins) {
				next.ServeHTTP(w, r)
				return
			}
			logger.WarnContext(r.Context(), "middleware: CSRF origin rejected",
				slog.String("origin", r.Header.Get("Origin")),
				slog.String("referer", r.Header.Get("Referer")),
				slog.String("path", r.URL.Path))
			WriteProblem(w, r, http.StatusForbidden, "cross-site request blocked")
		})
	}
}

// isSafeMethod reports whether m is an HTTP method that must not have side
// effects and is therefore exempt from CSRF checks.
func isSafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

// originAllowed reports whether r's Origin (falling back to Referer's origin)
// is same-origin with r (scheme + host + port) or present in allowed.
func originAllowed(r *http.Request, allowed []string) bool {
	origin := requestOrigin(r)
	if origin == "" {
		// Cookie-bearing unsafe request with no origin info at all — reject.
		return false
	}
	if slices.Contains(allowed, origin) {
		return true
	}
	return sameOrigin(origin, r)
}

// sameOrigin reports whether origin (a "scheme://host[:port]" string) is
// truly same-origin with r: scheme, host, and port must all match. Comparing
// host alone would accept http://farm.example against an https://farm.example
// deployment — same host, different scheme, not the same origin.
//
// r.Host is required to be non-empty: an empty Host would let a parsed
// "null" Origin (u.Host == "") compare equal to it. Unreachable from a real
// browser (net/http rejects HTTP/1.1 requests without a Host), but the
// precondition removes the reasoning burden.
func sameOrigin(origin string, r *http.Request) bool {
	if r.Host == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == requestScheme(r) && u.Host == r.Host
}

// requestScheme reports the request's effective scheme, honoring the
// X-Forwarded-Proto header set by a reverse proxy (sqi commonly runs behind
// one) in addition to r.TLS. Mirrors authHandler.secure in internal/api/auth.go.
func requestScheme(r *http.Request) string {
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		return "https"
	}
	return "http"
}

// requestOrigin returns the effective Origin header value, falling back to
// the scheme+host parsed from Referer when Origin is absent.
func requestOrigin(r *http.Request) string {
	if origin := r.Header.Get("Origin"); origin != "" {
		return origin
	}
	ref := r.Header.Get("Referer")
	if ref == "" {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
