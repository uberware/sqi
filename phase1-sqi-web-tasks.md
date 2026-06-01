# Phase 1 — Basic Web UI Initial Implementation Tasks

Detailed task breakdown for the third bullet of Section 17, Phase 1 of `sqi.md`:

> Basic web UI: job list, worker list, log viewer, job submission via raw OpenJD

Each item is a single, discrete task. Completing all of these yields a buildable, tested, documented web application embedded in the `sqi-server` binary that provides: a live-updating job list with filtering and bulk actions, a worker list with enable/disable controls, a task log viewer with live tail and ANSI color rendering, a raw OpenJD submission form with inline validation errors, and a minimal dashboard summarizing farm state — all updating in real time over WebSocket without page refreshes.

This UI replaces the Phase 1 placeholder `index.html` shipped by server task 88. The deliverable of this task list is a `web/dist/` directory that the server's Go `embed` directive picks up automatically on the next `make build`.

**Framework note.** `sqi.md §4` lists "TypeScript + React (or Svelte)". This task list defaults to **React + TypeScript + Vite** as the first-listed option and the more widely-known stack. If Svelte is preferred, confirm before starting task 1 — the project structure and tooling tasks differ, but the view-level tasks (sections 7–14) are framework-agnostic in intent.

**Commit markers.** Suggested commit boundaries are called out inline as blockquoted lines, e.g. `> _Commit:_ `feat(...)` _— tasks X–Y_`. Each marker groups tasks that ship as one working, build-green unit. Messages follow Conventional Commits. Solo-friendly — these are about future-you's bisect, blame, and revert experience, not PR ceremony.

---

## Common Instructions

These apply to every task in this list:

- **No git operations.** Do not run `git add`, `git commit`, `git push`, or any other git command. All work is done locally in-place. Commit markers below define logical groupings for future reference only — they are not an instruction to commit.
- **Follow the design.** All work must fit the overall design as described in `sqi.md`. When a design choice is ambiguous, prefer the approach consistent with `sqi.md` and the existing `sqi-server` and `sqi-worker` implementations. If a genuine conflict arises, pause and flag it rather than deciding unilaterally.
- **Phase 1 scope only.** Do not build views or components for Phase 2+ features (product form editor, preset browser, S3 storage configuration, LDAP/OAuth settings, etc.). Stub navigation links for deferred views as disabled items so the nav structure is established without misleading the user.
- **No auth UI.** Authentication is Phase 3. The web UI in Phase 1 makes all API calls without credentials. Do not add login pages, session management, or token handling.
- **Match the server's API shape.** All API client types and error handling must match the REST API as defined by the server implementation (particularly the `{code, message, details}` error shape from server task 78 and the OpenAPI spec from server task 79). When the OpenAPI spec and code diverge, treat the spec as authoritative and flag the discrepancy.

---

## Tracking

- **Currently working on:** —
- **Last updated:** —
- **Conventions:**
  - `- [ ]` not started · `- [x]` complete
  - Task checkboxes tick when the code lands; commit checkboxes tick once the commit is made.
  - Counts in section headers (e.g., `0 / 5`) are updated by hand when convenient — checkboxes are the source of truth.
  - Update the "Currently working on" line at the start of a session, clear it at the end.

---

## 1. Web Project Bootstrap — 0 / 5

- [ ] **1.** Initialize a Vite + React + TypeScript project in the existing `web/` directory using `npm create vite@latest` (or equivalent), choosing the `react-ts` template, and verify `npm run dev` starts without errors.
- [ ] **2.** Configure Vite's `build.outDir` to `dist` (the default) and verify that `npm run build` produces a `web/dist/index.html` that the server's Go `embed` directive (server task 86) will pick up on the next `make build`.
- [ ] **3.** Configure the Vite dev server proxy so that `/api` and `/api/v1/ws` requests from the dev server are forwarded to a local `sqi-server` instance (default `http://localhost:8080`), allowing the full UI to be developed against a live server without CORS issues.
- [ ] **4.** Add `web/node_modules/` and `web/dist/` to `.gitignore`; add `web/dist/` to a `.gitkeep`-free rule so `go build` does not fail on a clean clone before `npm run build` has been run (use a `//go:build ignore` guard or ensure the embed path is always populated by CI before the Go build step).
- [ ] **5.** Pin the Node.js version in a `.nvmrc` (or `.node-version`) file at the repository root and add a `engines` field to `web/package.json` so contributors know the minimum required Node version.

> - [ ] _Commit:_ `feat(web): vite react-ts bootstrap with dev proxy and embed integration` _— tasks 1–5_

---

## 2. Tooling and Code Quality — 0 / 7

- [ ] **6.** Configure ESLint with `eslint-config-typescript-strict` (or equivalent) and React-specific rules; add a `lint` script to `web/package.json` that fails on any warning so the linter acts as a gate, not a suggestion.
- [ ] **7.** Configure Prettier with a `.prettierrc` checked into the repository; add a `format` script and a `format:check` script used in CI.
- [ ] **8.** Configure TypeScript in `tsconfig.json` with `strict: true`, `noUncheckedIndexedAccess: true`, and `exactOptionalPropertyTypes: true` so type errors surface at development time rather than at runtime against the API.
- [ ] **9.** Configure Vitest for unit and component tests; add a `test` script (`vitest run`) and a `test:watch` script; configure coverage output to `web/coverage/` and set a minimum coverage threshold consistent with the server's.
- [ ] **10.** Configure path aliases (`@/` → `src/`) in both `vite.config.ts` and `tsconfig.json` so imports do not rely on relative `../../` paths.
- [ ] **11.** Add `web/` lint, type-check (`tsc --noEmit`), and test steps to the existing GitHub Actions CI workflow so the web build is gated on the same pipeline as the Go build.
- [ ] **12.** Run the full web test suite in CI with coverage reporting and enforce the minimum coverage threshold; ensure no TypeScript errors (`tsc --noEmit`) and no ESLint warnings remain on `main`.

> - [ ] _Commit:_ `build(web): eslint, prettier, strict typescript, vitest, ci integration` _— tasks 6–12_

---

## 3. Design Tokens, Global Styles, and App Shell — 0 / 6

- [ ] **13.** Define a CSS custom-properties token file (`src/styles/tokens.css`) covering the color palette, typography scale, spacing scale, border radii, and shadow levels; use neutral, professional values appropriate for a farm management tool rather than a consumer product.
- [ ] **14.** Implement a global CSS reset and base styles (`src/styles/global.css`) that normalize browser defaults, set the base font family and size from the token file, and establish a `box-sizing: border-box` baseline.
- [ ] **15.** Implement the top-level `App` component with a two-panel layout: a fixed left sidebar containing the primary navigation and a main content area that renders the active route; ensure the layout fills the viewport height without overflow issues.
- [ ] **16.** Implement the sidebar navigation component with links to all Phase 1 views (Dashboard, Jobs, Workers, Submit) and disabled stub entries for deferred Phase 2+ views (Presets, Products, Storage, License Pools, Settings) clearly labeled as "coming soon".
- [ ] **17.** Implement a reusable `<PageHeader>` component accepting a title, optional subtitle, and optional action slot (used by list pages for their primary action button) so all views have a consistent page-level heading treatment.
- [ ] **18.** Implement a reusable `<ErrorBoundary>` React error boundary wrapping each route so an unhandled render error in one view does not crash the entire application; display an inline error card with a "reload this section" recovery action.

> - [ ] _Commit:_ `feat(web): design tokens, global styles, app shell, nav, error boundary` _— tasks 13–18_

---

## 4. API Client Layer — 0 / 7

- [ ] **19.** Implement a typed base `apiFetch` function in `src/api/client.ts` that wraps `fetch`, sets the correct `Content-Type` and `Accept` headers, reads the base URL from Vite's environment variables (defaulting to `/`), and throws a typed `ApiError` (containing `code`, `message`, and `details`) on non-2xx responses matching the server's error shape (server task 78).
- [ ] **20.** Hand-write or generate TypeScript interfaces in `src/api/types.ts` for the core domain objects — `Job`, `Step`, `Task`, `TaskAttempt`, `Worker`, `Farm`, `Queue`, `StorageLocation`, `LicensePool` — matching the shapes returned by the server's REST API; keep these in sync with the OpenAPI spec served at `/api/v1/openapi.yaml` (server task 79).
- [ ] **21.** Install and configure TanStack Query (`@tanstack/react-query`) with a `QueryClient` at the app root using sensible defaults for Phase 1: `staleTime: 10_000`, `retry: 1`, and a global `onError` handler that logs unexpected API errors to the browser console.
- [ ] **22.** Implement typed query functions for the list endpoints used in Phase 1: `listJobs(params)`, `getJob(id)`, `listTasks(jobId)`, `getTask(id)`, `listWorkers(params)`, `getWorker(id)`, `listFarms()`, `listQueues(farmId)` — each returning a typed TanStack Query `useQuery` hook wrapper.
- [ ] **23.** Implement typed mutation functions for the write operations used in Phase 1: `submitJob(payload)`, `cancelJob(id)`, `retryTask(id)`, `disableWorker(id)`, `enableWorker(id)` — each returning a `useMutation` hook wrapper that invalidates the relevant queries on success.
- [ ] **24.** Implement a `usePaginatedList` helper hook that wraps a list query with `page` and `pageSize` state, manages URL-driven pagination via `useSearchParams`, and returns the current page data, total count, and navigation callbacks.
- [ ] **25.** Add unit tests for the `apiFetch` wrapper covering: successful JSON response parsing, `ApiError` construction from non-2xx responses, network error handling, and correct header attachment.

> - [ ] _Commit:_ `feat(web/api): typed fetch client, domain types, tanstack query, list and mutation hooks` _— tasks 19–25_

---

## 5. WebSocket Client — 0 / 6

- [ ] **26.** Implement a WebSocket client class in `src/ws/client.ts` that connects to `/api/v1/ws`, manages the connection lifecycle (open, close, error), and automatically reconnects with exponential backoff after disconnects.
- [ ] **27.** Implement typed message parsing for the server's envelope format (`type`, `subject`, `payload`, `seq`) as defined by server task 82, discarding and logging any message that fails schema validation rather than crashing the client.
- [ ] **28.** Implement a subscription registry that maps subject strings to sets of handler callbacks, dispatching incoming messages to all matching handlers; expose `subscribe(subject, handler)` and `unsubscribe(subject, handler)` methods.
- [ ] **29.** Expose the WebSocket client as a React context (`WebSocketContext`) and a `useWebSocket()` hook so any component can subscribe to a subject without needing access to the client instance directly; automatically unsubscribe when the consuming component unmounts.
- [ ] **30.** Implement a `ConnectionStatusBadge` component that reads the WebSocket connection state from context and displays a small colored indicator in the app header: green (connected), yellow (reconnecting), red (failed) — so operators can see at a glance whether live updates are flowing.
- [ ] **31.** Add unit tests for the WebSocket client covering: subscription dispatch to registered handlers, handler cleanup on unsubscribe, reconnect attempt sequencing, and graceful handling of malformed message envelopes.

> - [ ] _Commit:_ `feat(web/ws): websocket client with reconnect, typed dispatch, react context, status badge` _— tasks 26–31_

---

## 6. Client-Side Routing — 0 / 4

- [ ] **32.** Install React Router and define the top-level route table in `src/routes.tsx` covering all Phase 1 views: `/` (Dashboard), `/jobs` (Job List), `/jobs/:id` (Job Detail), `/workers` (Worker List), `/workers/:id` (Worker Detail), and `/submit` (Job Submission); wrap the app in `<BrowserRouter>` at the root.
- [ ] **33.** Implement a `<NotFound>` page component for the catch-all route that shows a friendly message and a link back to the dashboard, so navigating to an unknown URL does not render a blank page.
- [ ] **34.** Update the sidebar navigation component to use React Router `<NavLink>` so the active link is highlighted based on the current URL path, with `end` semantics on `/` to avoid it matching all routes.
- [ ] **35.** Implement URL-driven filter state for the job list: status filter, search text, sort field, sort direction, and page number are stored in `useSearchParams` so the URL can be bookmarked or shared and the correct filtered view is restored on load.

> - [ ] _Commit:_ `feat(web/routing): react router, notfound page, active nav, url-driven job filter state` _— tasks 32–35_

---

## 7. Job List View — 0 / 8

- [ ] **36.** Implement the job list page at `/jobs` using the `listJobs` query hook, rendering a table with columns: name, job ID (truncated with copy-on-click), owner, queue, status badge, priority, task progress (completed/total as a fraction and a compact progress bar), submission time, and elapsed/duration time.
- [ ] **37.** Implement a `<StatusBadge>` component that renders a color-coded pill for each task/job status (`pending`, `ready`, `assigned`, `running`, `succeeded`, `failed`, `canceled`) — used throughout the job list, job detail, and worker views.
- [ ] **38.** Implement the status filter bar above the job table showing counts for each status group (All, Running, Pending, Succeeded, Failed, Canceled) as clickable pills that set the `status` URL parameter and re-query accordingly.
- [ ] **39.** Implement a search input that filters by job name, job ID prefix, or owner username; debounce the input by 300 ms before updating the URL parameter to avoid a query per keystroke.
- [ ] **40.** Implement sortable column headers for job name, priority, submission time, and status; store sort field and direction in URL parameters so the sort is preserved on page reload.
- [ ] **41.** Implement a per-row cancel action (a "Cancel" button visible on hover for non-terminal jobs) that calls the `cancelJob` mutation, shows a loading state on the row, and refreshes the job's status badge on success.
- [ ] **42.** Implement multi-row selection with checkboxes and a bulk "Cancel selected" action that calls `cancelJob` for each selected non-terminal job sequentially, updating each row's status as each call resolves, and disabling the bulk action when no cancellable rows are selected.
- [ ] **43.** Add component tests for the job list view covering: rendering a list of mock jobs, status filter selection updating displayed rows, search input debounce, per-row cancel button calling the mutation, and the bulk cancel action.

> - [ ] _Commit:_ `feat(web/jobs): job list with status filter, search, sort, per-row cancel, bulk cancel` _— tasks 36–43_

---

## 8. Job Detail View — 0 / 5

- [ ] **44.** Implement the job detail page at `/jobs/:id` using the `getJob` query hook, displaying a metadata header card with: job name, ID, owner, submitter (shown separately per `sqi.md §11.2`), priority, queue, project tag, submission time, start time, end time, and current status badge.
- [ ] **45.** Implement a step breakdown section that lists each step in dependency order with its step name, dependency arrows (or a written "depends on step X" annotation), aggregate task counts (total, running, succeeded, failed), and an overall step status badge.
- [ ] **46.** Implement a per-task table within each step showing: task ID (truncated), parameters (key-value pairs from the OpenJD parameter space, collapsed to a summary with expand-on-click), status badge, assigned worker name (linked to the worker detail page), start time, duration, exit code (on terminal states), and attempt count.
- [ ] **47.** Implement a "Retry" button on each failed or canceled task row that calls the `retryTask` mutation and updates the row optimistically to `pending` while the server processes the request; revert to the original state and show an inline error if the mutation fails.
- [ ] **48.** Implement a "View Logs" action on each task row (or attempt row) that opens the log viewer for that task attempt; on Phase 1 this can be an in-page panel that slides in from the right or a dedicated route at `/jobs/:id/tasks/:taskId/logs`, whichever fits the layout better.

> - [ ] _Commit:_ `feat(web/jobs): job detail with metadata, step breakdown, task table, retry, log link` _— tasks 44–48_

---

## 9. Task Log Viewer — 0 / 8

- [ ] **49.** Implement the log viewer component as a dark-background fixed-width scrollable panel that renders pre-formatted text, styled to evoke a terminal window and visually distinct from the surrounding UI chrome.
- [ ] **50.** Fetch existing log chunks for a task attempt from `GET /api/v1/tasks/:id/logs` using offset-based pagination; load the most-recent N lines on first open, and load older chunks on scroll-up (virtual scroll or infinite-scroll-upward pattern) so large logs do not require loading everything at once.
- [ ] **51.** For tasks currently in `running` state, subscribe to live log chunks via the WebSocket using the `task.logs.<attempt_id>` subject (server task 83); append arriving chunks to the bottom of the viewer in real time as they are published.
- [ ] **52.** Implement auto-scroll to the bottom for live tailing with a "Pause scroll" toggle button; when scroll is paused, show an unread-lines count badge and a "Jump to bottom" button that re-enables auto-scroll.
- [ ] **53.** Implement ANSI escape code rendering using the `ansi-to-html` library (or equivalent) to honor color output from render processes (e.g., Arnold's colored progress output, Python `rich` logs); fall back to plain text if parsing fails rather than showing raw escape sequences.
- [ ] **54.** Display line numbers in a left gutter; implement a "Copy visible range" button that copies the currently visible log text to the clipboard as plain text without ANSI codes.
- [ ] **55.** Display the stream source (stdout vs stderr) as a subtle left-border color distinction on each line group, so mixed stdout/stderr output is visually navigable without requiring separate tabs.
- [ ] **56.** Add component tests for the log viewer covering: rendering a list of mock log chunks, ANSI escape code stripping for copy-to-clipboard, auto-scroll behavior toggling, and the stdout-vs-stderr gutter color distinction.

> - [ ] _Commit:_ `feat(web/logs): log viewer with pagination, live tail, ansi color, line numbers, copy` _— tasks 49–56_

---

## 10. Real-Time Job and Task Updates — 0 / 4

- [ ] **57.** Subscribe to WebSocket job summary updates on the job list page using the `useWebSocket` hook; when an update arrives for a job currently in the visible table, update its status badge, progress fraction, and elapsed time in place without triggering a full re-render of the list.
- [ ] **58.** Subscribe to WebSocket task-level updates on the job detail page; update each task row's status badge, duration, and exit code in real time as the server pushes state transitions so the detail view acts as a live dashboard for the job.
- [ ] **59.** Implement a "last updated" timestamp in the page header of the job list and job detail views showing when the data was last refreshed (either by a WebSocket push or a manual refetch), so operators can tell whether live updates are flowing.
- [ ] **60.** Implement a manual "Refresh" button on both the job list and job detail pages that calls `queryClient.invalidateQueries` for the relevant queries and fetches fresh data from the REST API as a fallback for environments where WebSocket is blocked.

> - [ ] _Commit:_ `feat(web/realtime): websocket-driven job list and detail updates, refresh fallback` _— tasks 57–60_

---

## 11. Job Submission Form — 0 / 7

- [ ] **61.** Implement the submission page at `/submit` with a two-column layout: a left panel for queue selection and metadata, and a right panel for the raw OpenJD input.
- [ ] **62.** Populate a "Target queue" selector from the `listFarms` and `listQueues` queries, presenting farms as option groups and queues as options within each group; default to the first available queue and persist the last-used queue in `localStorage`.
- [ ] **63.** Implement the raw OpenJD input using CodeMirror 6 configured for YAML and JSON modes (auto-detected from the first non-whitespace character) with syntax highlighting, line numbers, and basic bracket matching — so power users can paste directly from their template files with readable formatting.
- [ ] **64.** On form submit, call the `submitJob` mutation with the textarea content and selected queue; display structured validation errors returned by the server (the `details` array from the `{code, message, details}` error shape, server task 78) as inline annotations below the textarea, referencing the JSON Pointer path in each error so the user knows which part of their template is invalid.
- [ ] **65.** On successful submission, redirect to the newly created job's detail page at `/jobs/:id` and display a toast notification ("Job submitted — ID: abc123") so the user has a clear confirmation and an immediate path to monitor progress.
- [ ] **66.** Implement a "Load example" dropdown in the submission form that populates the textarea with one of two minimal valid OpenJD examples (a single-step shell command and a multi-task parameter-space job) so new users can submit a test job without needing to author OpenJD from scratch.
- [ ] **67.** Add component tests for the job submission form covering: validation error display mapping JSON Pointer paths to textarea annotations, successful submit redirecting to job detail, and the "Load example" dropdown populating the textarea.

> - [ ] _Commit:_ `feat(web/submit): openjd submission form with queue selector, codemirror, validation errors, redirect` _— tasks 61–67_

---

## 12. Worker List View — 0 / 6

- [ ] **68.** Implement the worker list page at `/workers` using the `listWorkers` query hook, rendering a table with columns: worker name, worker ID (truncated), compute location, status badge (online/offline/disabled), active tasks / max-concurrent, OS, key capability tags (GPU, RAM as a summary), and last heartbeat timestamp.
- [ ] **69.** Implement a status filter (All / Online / Offline / Disabled) above the table, analogous to the job list status filter, showing per-status counts.
- [ ] **70.** Implement per-row enable/disable toggle buttons: "Disable" for online workers and "Enable" for disabled workers, calling the `disableWorker` / `enableWorker` mutations; show a loading state on the row and update the status badge on success.
- [ ] **71.** Subscribe to WebSocket worker status updates using the `useWebSocket` hook; update each worker's status badge, active task count, and last heartbeat time in place as updates arrive so the worker list acts as a live node monitor.
- [ ] **72.** Implement a compact capability tag display in the worker table: show up to three tags inline and a "+N more" badge that expands to the full list in a tooltip or popover on hover, so the table does not overflow on workers with many tags.
- [ ] **73.** Add component tests for the worker list covering: status filter counts, per-row enable/disable button calling the correct mutation, and the "+N more" capability tag overflow display.

> - [ ] _Commit:_ `feat(web/workers): worker list with status filter, enable/disable, live updates, tag display` _— tasks 68–73_

---

## 13. Worker Detail View — 0 / 4

- [ ] **74.** Implement the worker detail page at `/workers/:id` using the `getWorker` query hook, displaying a metadata header with: worker name, ID, compute location, status badge, registration timestamp, last heartbeat timestamp, and uptime.
- [ ] **75.** Display the full capability tag list in a structured format — auto-detected fields (OS, OS version, CPU count, RAM, GPU presence, GPU VRAM) in a dedicated section and custom tags in a separate section — so operators can quickly verify what was reported vs. manually configured.
- [ ] **76.** Display the list of currently assigned tasks on this worker (from the worker detail response), each as a card showing job name (linked to job detail), task ID, step name, start time, and elapsed duration.
- [ ] **77.** Implement enable/disable toggle on the detail page (same mutation as the list page) so operators can act on a worker from either the list or the detail view.

> - [ ] _Commit:_ `feat(web/workers): worker detail with capabilities, assigned tasks, enable/disable` _— tasks 74–77_

---

## 14. Minimal Dashboard — 0 / 5

- [ ] **78.** Implement the dashboard home page at `/` as the landing view, structured as a grid of summary cards rather than a table, giving a quick at-a-glance overview of the farm's current state.
- [ ] **79.** Implement a "Workers" summary card showing: online / total worker count, active tasks / total capacity (as a utilization fraction), and a compact status breakdown (N online, N offline, N disabled) — linking to the worker list for each count.
- [ ] **80.** Implement a "Jobs" summary card showing counts of jobs in each active status for the current day (running, pending, succeeded, failed) and a link to the full job list pre-filtered by each status.
- [ ] **81.** Implement a "Recent Failures" list widget showing the last 5–10 failed jobs with job name, owner, queue, and failure time, each linking to the job detail page so an operator landing on the dashboard can immediately drill into any failure.
- [ ] **82.** Subscribe to WebSocket worker and job summary updates on the dashboard so all four widgets refresh without polling; display the `ConnectionStatusBadge` prominently if the WebSocket is disconnected so operators know the dashboard may be stale.

> - [ ] _Commit:_ `feat(web/dashboard): farm summary cards, recent failures, live websocket updates` _— tasks 78–82_

---

## 15. Shared UI Components — 0 / 6

- [ ] **83.** Implement a `<DataTable>` component with typed columns, row data, optional row click handler, loading skeleton state (shimmer rows), and empty state slot — used by the job list, worker list, and task table so table behavior is consistent and tested once.
- [ ] **84.** Implement a `<Pagination>` component that renders page navigation controls (previous, next, page number pills, items-per-page selector) driven by the `usePaginatedList` hook and consistent with the URL parameter scheme from task 35.
- [ ] **85.** Implement a `<Toast>` notification system (a context + hook + portal-rendered container) that supports success, error, and info severities, auto-dismisses after a configurable timeout, and stacks multiple simultaneous toasts — used for submit confirmation, mutation errors, and WebSocket reconnect notices.
- [ ] **86.** Implement a `<CopyButton>` component that copies a given string to the clipboard and shows a brief "Copied!" confirmation tooltip, used for job IDs, task IDs, and log text throughout the application.
- [ ] **87.** Implement a `<RelativeTime>` component that renders a timestamp as a relative string ("3 minutes ago", "just now") using the browser's `Intl.RelativeTimeFormat` API and re-renders every 30 seconds so displayed times stay current without a dedicated timer per instance.
- [ ] **88.** Add component tests (Vitest + React Testing Library) for `<DataTable>` covering: rendering with typed data, loading skeleton state, empty state slot, and row click handler invocation.

> - [ ] _Commit:_ `feat(web/ui): datatable, pagination, toast, copy button, relative time components` _— tasks 83–88_

---

## 16. Build, Packaging, and Release — 0 / 4

- [ ] **89.** Update the repository `Makefile` (or `Taskfile.yml`) so the `build` target runs `npm ci && npm run build` in `web/` before `go build ./...`, ensuring the `web/dist/` bundle embedded by the server is always built from the current source.
- [ ] **90.** Add the web build step (`npm ci && npm run build`) to the goreleaser `before.hooks` configuration so release binaries and Docker images include the production-optimized UI bundle, not the Phase 1 placeholder.
- [ ] **91.** Verify that a `go build ./cmd/sqi-server` from a clean clone (after running `npm run build` in `web/`) produces a single binary where `GET /` returns a real HTML page with the React app, not the placeholder, and that no `embed` path errors occur.
- [ ] **92.** Add an end-to-end test using Playwright (or Cypress) that: starts a real `sqi-server` + `sqi-worker`, opens the web UI, submits a minimal OpenJD job using the form, watches the job list update the status badge to `running` then `succeeded` in real time, clicks through to the job detail, and verifies log output is visible in the log viewer.

> - [ ] _Commit:_ `build(web): wire npm build into makefile and goreleaser` _— tasks 89–92_

---

## 17. Documentation — 0 / 6

- [ ] **93.** Write a `web/README.md` describing the project structure, how to run the dev server against a local `sqi-server`, how to run tests, and how the production build is embedded in the Go binary.
- [ ] **94.** Write `docs/web-development.md` covering: prerequisites (Node version, npm), local development workflow (start server, start worker, start Vite dev server), the proxy configuration, adding a new route, adding a new API query hook, and TypeScript conventions used in the project.
- [ ] **95.** Write `docs/web-build.md` explaining how `web/dist/` is generated, how Go's `embed` directive picks it up, how cache-busting works in the Vite production build (content-hashed filenames), and what to do if the UI is stale in a built binary.
- [ ] **96.** Write inline JSDoc comments on all exported functions and types in `src/api/` and `src/ws/` so IDEs surface parameter descriptions and return types without needing to cross-reference the OpenAPI spec.
- [ ] **97.** Write `docs/web-accessibility.md` stating the Phase 1 accessibility baseline: all interactive elements reachable by keyboard, all color usage meeting WCAG AA contrast ratio, all status badges and icons having text alternatives — so future contributors know the target and do not regress it.
- [ ] **98.** Add a `CONTRIBUTING.md` section (or extend the existing one) for web UI contributions describing: how to run Storybook (if added later), the component testing approach, the API client pattern, and how to match the existing styling conventions.

> - [ ] _Commit:_ `docs(web): readme, dev guide, build docs, jsdoc, accessibility baseline` _— tasks 93–98_

---

## 18. Final Verification — 0 / 5

- [ ] **99.** Execute an end-to-end smoke test: build a fresh `sqi-server` binary with the embedded UI, start it with default config alongside a connected `sqi-worker`, open `http://localhost:8080` in a browser, confirm the dashboard loads with live worker data, submit a minimal OpenJD job, watch it run to completion on the job list, click through to the log viewer, and verify all log output is visible.
- [ ] **100.** Verify the UI works without JavaScript console errors in Chrome (latest), Firefox (latest), and Safari (latest); fix any browser-compatibility issues in the ANSI renderer, WebSocket reconnect logic, or Clipboard API usage.
- [ ] **101.** Verify the full web build (`npm run build`) completes without TypeScript errors, Vite warnings, or ESLint violations; verify the production bundle size is reasonable (flag if the JS bundle exceeds 500 KB gzipped, as CodeMirror and ansi-to-html are the likely heavyweights).
- [ ] **102.** Verify the test suite passes with no failures and coverage meets the configured threshold; verify the Playwright E2E test passes against a freshly built server+worker.
- [ ] **103.** Verify every command and URL in the web documentation works as written against the freshly built binary; update any examples that have drifted from the implementation.

> - [ ] _Commit:_ `test(web): end-to-end smoke and cross-browser verification` _— tasks 99–103_
