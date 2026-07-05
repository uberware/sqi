// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
)

// nameParam reads the {name} path parameter and URL-decodes it. Product and
// preset names carry an optional '/' namespace (e.g. "testing/render-simulator")
// that browsers send percent-encoded as %2F; chi does not decode path params,
// so callers must unescape. An undecodable value falls back to the raw param
// (it simply won't match any stored name).
func nameParam(r *http.Request) string {
	raw := chi.URLParam(r, "name")
	if decoded, err := url.PathUnescape(raw); err == nil {
		return decoded
	}
	return raw
}
