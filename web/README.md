# `web/`

TypeScript + React (or Svelte) source for the embedded web UI. The build output is written to `web/dist/`, which package `web` (see `embed.go`) bakes into the binary via Go's `embed` package (task 93). `internal/ui` serves that embedded bundle over HTTP with single-page-application fallback routing (tasks 94–95).

The embed directive lives here, in package `web`, rather than in `internal/ui` because Go's `//go:embed` can only reference files at or below the embedding file's own directory.

Phase 1 ships only a minimal placeholder `index.html` linking to the OpenAPI spec and health endpoints. It is the one file under `web/dist/` tracked in git (everything else there is build output and is ignored); the full UI build overwrites it later in the roadmap.
