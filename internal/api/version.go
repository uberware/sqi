// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Version REST handler.
//
// Route summary:
//
//	GET /api/v1/version — return the server's build metadata

import (
	"net/http"

	"github.com/uberware/sqi/internal/version"
)

// versionHandler serves the server build metadata. The info is captured at
// construction (injected via [Deps.Version]) rather than read from the version
// package directly, so the handler is deterministically testable.
type versionHandler struct {
	info version.Info
}

// newVersionHandler returns a versionHandler that reports the given build info.
func newVersionHandler(info version.Info) *versionHandler {
	return &versionHandler{info: info}
}

// ── GET /api/v1/version ───────────────────────────────────────────────────────

// getVersion returns the server's version, commit, build date, and Go runtime
// version as JSON.
func (h *versionHandler) getVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.info)
}
