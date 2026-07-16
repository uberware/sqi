# `docs/`

Long-form documentation for `sqi-server` operators and contributors.

**Getting started**

- `quickstart.md` — end-to-end first run (binary or Docker Compose): farm, queue, first job
- `index.md` — overview / landing page (mirrors the repo-root `README.md` for the docs site)

**Architecture & API**

- `architecture.md` — component layout and job-lifecycle data flow
- `api.md` — REST API guide with worked examples, links to the served OpenAPI spec
- `python-client.md` — `sqi-sdk` Python library reference (construction, every method, examples)
- `openjd-submission.md` — minimal, parameter-space, and multi-step OpenJD examples
- `openjd-extensions.md` — the OpenJD extension registry and how to add one (subdocs under `openjd-extensions/`)

**Products, presets & submitters** (Phase 2)

- `products.md` — products as a catalog layer over OpenJD templates
- `preset-library.md` — the community preset library and its index format
- `dcc-submitters.md` — in-DCC submitters for Maya, Houdini, Nuke, and Blender
- `compute-locations.md` — compute-location entities and worker affinity

**Storage**

- `storage-locations.md` — named storage locations, `loc://` URIs, and path resolution
- `storage-s3.md` — S3-compatible storage and `stage_locally` path delivery

**Configuration & operations**

- `configuration.md` — every server config option with type, default, env var, example
- `auth.md` — the opt-in `auth.enabled` gate, the `Principal`/`Authenticator` model, and what's scaffolding vs. live
- `worker-configuration.md` — the full `sqi-worker` config reference
- `worker-capabilities.md` — capability tags and software auto-detection
- `worker-deployment.md` — running workers (systemd, bare binary)
- `worker-docker.md` — the worker container image
- `operations.md` — install, upgrade, backup/restore, log rotation, metrics scraping
- `observability.md` — diagnostics ring buffer, health, metrics, and why-isn't-my-job-running
- `release-runbook.md` — cutting a tagged release
- `cross-platform-validation.md` — the pre-release cross-platform check

**Web UI**

- `web-development.md` — web UI dev-server workflow, proxy, adding a route/query hook
- `web-build.md` — how `web/dist/` is built, embedded, cache-busted, and debugged
- `web-accessibility.md` — the web UI accessibility baseline

**Contributor reference**

- `development.md` — local setup, test commands, code layout, how to add an endpoint
- `contributing.md` — contribution guide (mirrors the repo-root `CONTRIBUTING.md`)
- `roadmap.md` — technical-architecture and roadmap reference (mirrors the repo-root `ROADMAP.md`)
- `spdx-header.md` — the SPDX license-header convention for source files

The canonical product/vision docs (`README.md`, `ROADMAP.md`, `CONTRIBUTING.md`) live at the repo root; `docs/index.md`, `docs/roadmap.md`, and `docs/contributing.md` are docs-site copies with site-relative links. `ROADMAP.md` is the technical-architecture and roadmap reference these docs point to for design rationale.
