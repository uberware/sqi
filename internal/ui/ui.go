// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ui serves the embedded web UI bundle over HTTP with single-page
// application (SPA) routing semantics.
//
// The compiled assets are embedded into the binary by package web; this package
// owns the request-handling policy on top of that [fs.FS]:
//
//   - A request whose path maps to a real embedded file is served verbatim
//     (e.g. GET /assets/app.js, GET /favicon.ico).
//   - GET / serves the SPA shell (index.html).
//   - A request for a path that has no embedded file but lives under /ui/* is
//     treated as a client-side route: the shell is returned so the front-end
//     router can resolve it on the client. This is the SPA fallback.
//   - Any other unknown path returns 404, so missing assets and stray requests
//     are not masked by the shell.
//
// The handler is mounted as the chi root catch-all (GET|HEAD /*) after every API
// and observability route, so it only ever sees paths those routes did not claim.
package ui

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/uberware/sqi/web"
)

// indexFile is the SPA shell served at the root and for client-side routes.
const indexFile = "index.html"

// uiPrefix is the URL path prefix reserved for client-side routes that should
// fall back to the SPA shell when they do not match an embedded asset.
const uiPrefix = "/ui"

// Handler serves the embedded web UI with SPA-fallback routing. Construct it
// with [NewHandler] and mount it as the root catch-all on the HTTP router.
type Handler struct {
	assets fs.FS
	logger *slog.Logger
}

// NewHandler builds a [Handler] backed by the embedded web/dist bundle. It
// returns an error only if the embedded asset tree cannot be opened, which
// indicates a build-time problem rather than a runtime condition.
func NewHandler(logger *slog.Logger) (*Handler, error) {
	assets, err := web.Dist()
	if err != nil {
		return nil, fmt.Errorf("load embedded web assets: %w", err)
	}
	return &Handler{assets: assets, logger: logger}, nil
}

// ServeHTTP implements [http.Handler]. See the package documentation for the
// routing policy it applies.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// path.Clean collapses any "." / ".." segments, so a cleaned absolute path
	// can never escape the embedded tree. The leading slash is then trimmed to
	// form an fs path (fs.FS uses slash-separated, unrooted names).
	cleaned := path.Clean(r.URL.Path)
	name := strings.TrimPrefix(cleaned, "/")

	// The root path serves the SPA shell.
	if name == "" {
		h.serveAsset(w, r, indexFile)
		return
	}

	// Serve a concrete embedded asset when one exists at the requested path.
	if h.isFile(name) {
		h.serveAsset(w, r, name)
		return
	}

	// SPA fallback: client-side routes live under /ui/* and have no embedded
	// file, so return the shell and let the front-end router resolve them.
	if cleaned == uiPrefix || strings.HasPrefix(cleaned, uiPrefix+"/") {
		h.serveAsset(w, r, indexFile)
		return
	}

	// Genuine miss: an unknown asset path outside the SPA route space.
	http.NotFound(w, r)
}

// isFile reports whether name resolves to a regular (non-directory) file in the
// embedded asset tree.
func (h *Handler) isFile(name string) bool {
	if !fs.ValidPath(name) {
		return false
	}
	f, err := h.assets.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// serveAsset writes the named embedded asset to the response. The name is always
// an embedded path the caller has already validated (a known file, or indexFile
// which is guaranteed to exist by the embed in package web), so a failure to
// open it is an internal error rather than a 404.
func (h *Handler) serveAsset(w http.ResponseWriter, r *http.Request, name string) {
	f, err := h.assets.Open(name)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "ui: failed to open embedded asset",
			slog.String("asset", name), slog.Any("error", err))
		http.Error(w, "web UI asset unavailable", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	// The SPA shell must always be revalidated so a freshly deployed binary's
	// index.html (which references new fingerprinted asset names) is picked up
	// immediately. Other assets are content-addressed by the build and may be
	// cached for a while.
	if name == indexFile {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}

	// http.ServeContent handles Content-Type detection (by extension and
	// sniffing), HEAD requests, range requests, and conditional headers. Embedded
	// files carry no modtime, so a zero time is passed to disable Last-Modified;
	// the Cache-Control headers set above govern freshness instead.
	if rs, ok := f.(io.ReadSeeker); ok {
		http.ServeContent(w, r, name, time.Time{}, rs)
		return
	}

	// Fallback for any fs.FS whose files are not seekable: buffer then serve.
	data, err := io.ReadAll(f)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "ui: failed to read embedded asset",
			slog.String("asset", name), slog.Any("error", err))
		http.Error(w, "web UI asset unavailable", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}
