// SPDX-License-Identifier: AGPL-3.0-only

// Package web embeds the compiled web UI bundle into the sqi-server binary so
// the single binary serves its own front-end with no external asset directory.
//
// The bundle lives under web/dist and is produced by the Vite build of the
// TypeScript/React app under web/src (run via "make build-web", which
// "make build"/"make build-server" invoke first). Whatever files are present
// under web/dist at compile time are baked into the binary by the //go:embed
// directive below. A single git-tracked placeholder web/dist/index.html keeps
// the embed valid on a clean checkout before the first web build runs; a real
// build overwrites it with the hashed production assets. See web/README.md and
// docs/web-build.md for the build-and-embed flow.
//
// The embed declaration lives in this package — rather than in internal/ui —
// because Go's embed directive can only reference files at or below the
// embedding source file's own directory. internal/ui consumes the [fs.FS]
// returned by [Dist] to implement the HTTP serving and SPA-fallback logic.
package web

import (
	"embed"
	"fmt"
	"io/fs"
)

// distFS holds the embedded web/dist bundle.
//
// The plain (non-"all:") form deliberately excludes files whose names begin with
// "." or "_". That keeps OS sidecar junk (e.g. macOS "._index.html" resource
// forks) and editor dotfiles out of the binary; the Vite build that populates
// web/dist emits only normally-named files (index.html, assets/*), so nothing we
// need is dropped. If a future bundler emits "_"-prefixed paths, switch this to
// "all:dist" and add a build step that strips OS sidecars first.
//
//go:embed dist
var distFS embed.FS

// Dist returns an [fs.FS] rooted at the embedded web/dist directory, so callers
// open assets by their public path (e.g. "index.html", "assets/app.js") without
// the "dist/" prefix. It returns an error only if the embedded tree is malformed,
// which would indicate a build-time problem.
func Dist() (fs.FS, error) {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil, fmt.Errorf("sub embedded web/dist: %w", err)
	}
	return sub, nil
}
