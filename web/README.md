# `web/`

TypeScript + React + Vite source for the `sqi-server` web UI. The build output
is written to `web/dist/`, which package `web` (see `embed.go`) bakes into the
`sqi-server` binary via Go's `embed` package. `internal/ui` serves that embedded
bundle over HTTP with single-page-application fallback routing, so a single
binary ships its own front-end with no external asset directory.

The UI is a live-updating farm console: a job list with filtering and bulk
actions, a worker list with enable/disable controls, a task log viewer with live
tail and ANSI color, a raw OpenJD submission form, and a dashboard summarising
farm state — all updating in real time over WebSocket without page refreshes.

## Project structure

```
web/
├── embed.go            # Go //go:embed directive — bakes dist/ into the binary
├── index.html          # Vite HTML entry point
├── vite.config.ts      # Build, dev-server proxy, path alias, and Vitest config
├── package.json        # Scripts, dependencies, and the Node/npm engine pins
├── dist/               # Production build output (git-ignored except index.html)
└── src/
    ├── main.tsx        # React entry point: providers + router mount
    ├── App.tsx         # App shell (sidebar + routed content area)
    ├── routes.tsx      # React Router route table
    ├── api/            # Typed REST client, domain types, TanStack Query hooks
    ├── ws/             # WebSocket client, React context, typed event payloads
    ├── components/     # Reusable UI components (DataTable, StatusBadge, …)
    ├── pages/          # Top-level views (Dashboard, JobList, Submit, …)
    ├── hooks/          # Shared hooks (useDebounce, usePaginatedList, …)
    ├── styles/         # Design tokens and global CSS
    └── test/           # Vitest setup (jest-dom matchers, etc.)
```

The `api/` and `ws/` layers are the contract with the server: `api/types.ts`
mirrors the REST wire format (kept in sync with `GET /api/v1/openapi.yaml`), and
`ws/types.ts` / `ws/events.ts` mirror the WebSocket envelope and push payloads.

## Prerequisites

- **Node.js ≥ 24** and **npm ≥ 11** — pinned in `.nvmrc` (repo root) and the
  `engines` field of `package.json`. Run `nvm use` from the repo root to match.

Install dependencies once:

```sh
npm install
```

## Local development

The dev server runs the UI with hot-module reload and proxies API traffic to a
local `sqi-server`, so you develop against live data with no CORS setup.

```sh
# Terminal 1 — from the repo root, start the server (and a worker if you want
# jobs to actually run; see docs/web-development.md).
make run

# Terminal 2 — from web/, start the Vite dev server.
npm run dev
```

Vite serves on `http://localhost:5173` and forwards `/api` (REST **and** the
`/api/v1/ws` WebSocket) to `http://localhost:8080`. Override the target by
editing the `server.proxy` block in `vite.config.ts`.

See [`docs/web-development.md`](../docs/web-development.md) for the full
workflow, the proxy explained, and how to add a route or an API query hook.

## Scripts

| Script                  | Purpose                                                             |
| ----------------------- | ------------------------------------------------------------------- |
| `npm run dev`           | Start the Vite dev server with HMR and the API proxy                |
| `npm run build`         | Type-check (`tsc -b`) then produce the production bundle in `dist/` |
| `npm run preview`       | Serve the built `dist/` locally to sanity-check a production build  |
| `npm run lint`          | ESLint with `--max-warnings 0` (warnings fail)                      |
| `npm run typecheck`     | `tsc -b` with no emit                                               |
| `npm run format`        | Rewrite files with Prettier                                         |
| `npm run format:check`  | Verify formatting without writing (used in CI)                      |
| `npm run test`          | Run the Vitest suite once                                           |
| `npm run test:watch`    | Run Vitest in watch mode                                            |
| `npm run test:coverage` | Run tests with V8 coverage; fails below the configured threshold    |

The full gate, matching CI, is:

```sh
npm run format:check && npm run typecheck && npm run lint && npm run test:coverage
```

## Testing

Tests use [Vitest](https://vitest.dev/) with
[React Testing Library](https://testing-library.com/) and a `jsdom`
environment. Test files live next to the code they cover as `*.test.ts` /
`*.test.tsx`. Coverage is written to `web/coverage/` and enforced against the
threshold in `vite.config.ts`.

## Production build and embedding

`npm run build` writes content-hashed assets (`assets/index-<hash>.js`,
`.css`) plus `index.html` into `web/dist/`. The `//go:embed dist` directive in
`embed.go` bakes whatever is in `dist/` into the `sqi-server` binary at compile
time, so the served UI always matches the bundle present when the binary was
built.

You normally never run `npm run build` by hand: `make build` (and
`make build-server`) run it first via the `build-web` target, so the embedded
bundle is rebuilt from current source on every server build. The `npm ci` step
is gated on a stamp file keyed to the npm manifests, so it only reinstalls when
dependencies change.

For how cache-busting works and what to do if the UI looks stale in a built
binary, see [`docs/web-build.md`](../docs/web-build.md).

> The embed directive lives here, in package `web`, rather than in
> `internal/ui` because Go's `//go:embed` can only reference files at or below
> the embedding file's own directory.
