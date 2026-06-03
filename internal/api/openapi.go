// SPDX-License-Identifier: AGPL-3.0-only

package api

// OpenAPI spec serving — task 83.
//
// The spec is embedded from api/openapi.yaml at compile time so the binary is
// self-contained.  The handler is wired into the router at:
//
//	GET /api/v1/openapi.yaml

import (
	_ "embed"
	"net/http"
)

// openapiSpec is the raw OpenAPI 3.1 YAML document embedded at build time.
// The path is relative to this file's directory (internal/api/).
//
//go:embed openapi.yaml
var openapiSpec []byte

// serveOpenAPISpec writes the embedded OpenAPI 3.1 specification as YAML.
// It is mounted at GET /api/v1/openapi.yaml by NewRouter.
func serveOpenAPISpec(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	// Disable caching so clients always receive the current binary's spec.
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(openapiSpec) //nolint:errcheck // client disconnect is non-fatal
}
