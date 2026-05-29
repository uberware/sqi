# `web/`

TypeScript + React (or Svelte) source for the embedded web UI. The build output is written to `web/dist/`, which `internal/ui` embeds into the binary via Go's `embed` package (tasks 86–88).

Phase 1 ships only a minimal placeholder `index.html` linking to the OpenAPI spec and health endpoints. The full UI lands later in the roadmap.
