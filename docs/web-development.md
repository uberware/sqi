# Web UI Development Guide

This document covers local development of the `sqi-server` web UI: prerequisites,
the dev-server workflow against a live server and worker, the proxy
configuration, and how to add a new route, an API query hook, and a WebSocket
subscription. For the Go server/worker side, see
[`development.md`](development.md); for the production build and embedding, see
[`web-build.md`](web-build.md).

The UI source lives in [`web/`](https://github.com/uberware/sqi/tree/main/web); see [`web/README.md`](https://github.com/uberware/sqi/blob/main/web/README.md)
for the directory layout.

---

## Prerequisites

| Tool | Purpose | Install |
|---|---|---|
| Node.js ≥ 24 with npm ≥ 11 | Build and develop the web UI | [nodejs.org](https://nodejs.org/) or `nvm use` |
| Go ≥ 1.26 (the `go` directive in `go.mod` pins 1.26.3) | Build and run `sqi-server` / `sqi-worker` to develop against | [go.dev/dl](https://go.dev/dl/) |

The Node and npm minimums are pinned in `.nvmrc` (repo root) and the `engines`
field of `web/package.json`. From the repo root:

```sh
nvm use            # selects the Node version from .nvmrc
cd web
npm install        # install dependencies (run once, and after dependency changes)
```

---

## Local development workflow

The Vite dev server provides hot-module reload and proxies API traffic to a
local `sqi-server`, so you edit UI code and see changes instantly while reading
and writing real data.

A complete loop runs three processes. Run each in its own terminal from the
repo root unless noted.

```sh
# Terminal 1 — sqi-server (SQLite + embedded NATS, defaults to :8080)
make run
# or, after `make build`: ./bin/sqi-server serve

# Terminal 2 — a worker, so submitted jobs actually run
./bin/sqi-worker --server http://localhost:8080
# (build it first with `make build` if needed)

# Terminal 3 — the Vite dev server (from web/)
cd web && npm run dev
```

Open the URL Vite prints (default `http://localhost:5173`). The dashboard should
show live worker data; submitting a job from `/submit` runs it on the worker and
the job list updates in real time over WebSocket.

You do **not** need a worker just to develop most views — the server alone
serves the job/worker lists and the dashboard. Add a worker when you want jobs
to progress through `running` → `succeeded` and to exercise the live log viewer.

Quick checks while developing:

| Command (run in `web/`) | Purpose |
|---|---|
| `npm run dev` | Dev server with HMR + proxy |
| `npm run typecheck` | `tsc -b`, no emit — catch type errors early |
| `npm run lint` | ESLint (`--max-warnings 0`) |
| `npm run test:watch` | Vitest in watch mode |

Before considering a change done, run the full gate (matches CI):

```sh
npm run format:check && npm run typecheck && npm run lint && npm run test:coverage
```

---

## Proxy configuration

The dev server never talks to the server's origin directly — it proxies, which
avoids CORS entirely and lets the same relative `/api/...` paths the production
bundle uses work in development. The relevant block in `web/vite.config.ts`:

```ts
server: {
  proxy: {
    // Forward all /api requests (REST + WebSocket) to a local sqi-server.
    // ws: true enables WebSocket upgrade proxying, covering /api/v1/ws.
    '/api': {
      target: 'http://localhost:8080',
      changeOrigin: true,
      ws: true,
    },
  },
}
```

- Every request whose path starts with `/api` is forwarded to `target`.
- `ws: true` proxies the WebSocket upgrade, so the live-update connection at
  `/api/v1/ws` works through the dev server.
- If your server listens elsewhere (e.g. `SQI_HTTP_ADDR=127.0.0.1:9090`), change
  `target` to match.

Because the client code builds API URLs from `import.meta.env.VITE_API_BASE_URL`
(defaulting to `/` — see `src/api/client.ts`), production and development use the
same relative paths; only the proxy differs.

---

## Adding a new route

Routes are declared in [`web/src/routes.tsx`](https://github.com/uberware/sqi/blob/main/web/src/routes.tsx) using React
Router.

1. Create the page component under `src/pages/`, e.g. `Presets.tsx`, default-
   exporting a React component. Co-locate styles as `Presets.module.css` and a
   test as `Presets.test.tsx`.
2. Register it in `routes.tsx`:

   ```tsx
   import Presets from '@/pages/Presets'
   // …
   <Route path="/presets" element={<Presets />} />
   ```

   Keep the catch-all `<Route path="*" element={<NotFound />} />` last.

   The product management pages live at `/products` (list), `/products/new` and
   `/products/:name/edit` (the create/edit form), and `/products/:name` (read-only
   detail). Built-in products are read-only and offer "Duplicate to custom".

   The product submission flow uses three routes:

   | Route | Component | Description |
   |---|---|---|
   | `/submit` | `ProductPicker` | Product picker — lists all catalog products for the user to choose from |
   | `/submit/product/:name` | `ProductSubmit` | Product submission form — fetches parameters and renders a dynamic form |
   | `/submit/raw` | `Submit` | Raw OpenJD editor for direct template submission without the catalog |

   The preset library (under the Admin hub) uses two routes:

   | Route | Component | Description |
   |---|---|---|
   | `/presets` | `PresetLibrary` | Browse all presets from the index with per-preset status and a Refresh button |
   | `/presets/:name` | `PresetDetail` | Preview a preset's metadata and template (read-only); Install / Update / Reinstall button |

3. Surface it in navigation:
   - **Operational views** (dashboard, submit, jobs, workers) are top-level
     entries in `src/components/layout/Sidebar.tsx` — add a `<NavLink>` so the
     active link highlights based on the URL.
   - **Admin / management views** (farms, queues, usage pools, storage, compute
     locations, products, preset library, server log) are **not** in the sidebar;
     they live on the **Admin hub** (`/admin`, `src/pages/Admin.tsx`). Add an entry
     to its `ADMIN_LINKS` registry (`label`, `description`, `to`) and the card grid
     renders it. The sidebar's only management entry is **Admin** itself; the server
     log is its own route, `/server-log`.
     The sidebar's fixed set of top-level entries is the `PHASE1_NAV` array in
     `Sidebar.tsx` (Dashboard, Submit, Jobs, Workers, Admin); there are no
     disabled or "coming soon" placeholder entries.

Use the `@/` path alias (configured in both `vite.config.ts` and
`tsconfig.app.json`) for imports instead of relative `../../` paths.

### Product parameter widgets (`selectWidget`, `ProductParamField`)

`ProductSubmit`'s form is generated from a product's parameters
(`GET /products/{name}/parameters`), not hand-coded per product.
`web/src/lib/productForm.ts`'s `selectWidget(p: ProductParameter): Widget`
picks the input for one parameter — an explicit `user_interface.control`
first, else a fallback by declared `type` — and `ProductParamField`
(`web/src/components/ProductParamField.tsx`) renders whichever `Widget` comes
back:

| Widget | Rendered as |
|---|---|
| `text` | `<input type="text">` |
| `textarea` | `<textarea>` |
| `select` | `<select>`, options from `allowed_values` |
| `checkbox` | `<input type="checkbox">` |
| `number` | `<input type="number">` |
| `hidden` | nothing — `ProductParamField` returns `null` |
| `list` | `ListParamField` (`web/src/components/ListParamField.tsx`) — one row per element, with Add item / Remove buttons |

`list` is selected by any of RFC 0007's six `*_LIST` `userInterface`
controls — `LINE_EDIT_LIST`, `SPIN_BOX_LIST`, `CHECK_BOX_LIST`, and the three
`CHOOSE_*_LIST` file-picker controls, which map to the row editor rather than
a file dialog because no file-picker widget exists yet, the same interim
already accepted for their scalar `CHOOSE_*` counterparts (plain text) — or,
absent an explicit control, by a bare `LIST[STRING]` / `LIST[PATH]` /
`LIST[INT]` / `LIST[FLOAT]` / `LIST[BOOL]` type. **`LIST[LIST[INT]]` is
deliberately excluded from that type-based fallback**: RFC 0007 gives it no
control but `HIDDEN` and describes its use case as programmatic, so it falls
through to a raw JSON text field instead of a doubly-nested row editor.
`ListParamField` falls back to that same raw-JSON field whenever the incoming
value doesn't parse as a JSON array (a malformed stored default, say), so a
bad value never blocks the form from rendering.

**The row editor's serialisation must match `internal/openjd/paramjson.go`'s
canonical JSON form on *type*, not necessarily on byte-for-byte form.**
`ListParamField` encodes with plain `JSON.stringify` — no inserted
whitespace, each element written as its own JSON type (number, boolean, or
string) — because the server decodes a submitted list value with
`encoding/json` and checks each element by its *JSON type* against the
parameter's declared element type. What actually matters is that fidelity: a
number must arrive as a JSON number, not a string, and so on for booleans.
Byte equality is not itself the requirement — `BindJobParameters`
(`internal/openjd/bind.go`) stores a submitted value verbatim and nothing
ever compares it against the canonical default — but the two encoders do
agree on every byte form the editor can actually produce: `JSON.stringify`
and `marshalCanonical` agree on separators, string escaping including
`< > &`, and float formatting, and diverge only on two inputs neither side's
editor emits — Go escapes U+2028/U+2029 and lone surrogates unconditionally
— each harmless because both sides decode the other's output identically.
Nothing mechanical keeps the two encoders in agreement — they match only
because both sides' tests assert the same literal strings, Go's
`TestEncodeListDefault` table mirrored into the TypeScript suite for every
element type the row editor itself renders (strings, ints, floats, bools,
paths, and the empty-list case; `TestEncodeListDefault`'s two
`LIST[LIST[INT]]` rows are out of scope here, since that type is
deliberately excluded from the row editor and falls to the raw-JSON field —
see above). If you touch either encoder, update both.

---

## Authentication (login route, `AuthProvider`, `useAuth`)

`GET /api/v1/auth/me` is the **single signal** that drives the whole app's
auth gating — there is no separate "is auth enabled?" flag on the client.
`AuthProvider` (`src/auth/context.tsx`) wraps the app and calls it via the
`useAuthMe` query hook, resolving to one of three `status` values:

- `'loading'` — the initial request hasn't resolved yet.
- `'authed'` — `/auth/me` returned `200`. This covers both a real logged-in
  user **and** the anonymous superuser principal `/auth/me` returns when the
  server has `auth.enabled=false` (`kind: "anonymous"`) — which is what lets
  the same client code serve both modes without a separate feature flag.
- `'anon'` — `/auth/me` returned `401` (no/invalid/expired session), or any
  other request error; a network failure or 5xx is treated as `'anon'` too,
  so the app never gets stuck rendering the shell against an unconfirmed
  identity.

`App.tsx` reads `useAuth().status` and renders `<Login />` in place of the
whole shell (sidebar, routes, WebSocket provider) whenever status is `'anon'`,
and a loading placeholder for `'loading'`. There is no `/login` entry in
`routes.tsx` — logging in is a full replacement of the app root, not a routed
page, so it renders regardless of whatever URL the user was on.

`src/pages/Login.tsx` submits `POST /auth/login` via the `useLogin` mutation
and, on success, calls `refresh()` from `useAuth()` (which invalidates the
`auth.me` query) to let `AuthProvider` re-resolve and flip the app back to the
shell. A global handler in `src/api/queryClient.ts` also invalidates `auth.me`
whenever *any* query gets a `401`, so an expired or revoked session anywhere
in the app bounces the user back to the login screen on its own.

**The web stores no token, anywhere.** There is no localStorage, sessionStorage,
or in-memory token cache — the session is an `HttpOnly` cookie the browser
attaches automatically, which JavaScript cannot read even if it wanted to.
This is deliberate: it's what makes the session resistant to exfiltration via
XSS (a malicious script running in the page still cannot read or copy the
credential). Every request goes through `apiFetch` (`src/api/client.ts`),
which sets `credentials: 'include'` so the cookie rides along on same-origin
requests without any client-side bookkeeping.

---

## Role gating (`can`, `<RequireRole>`, nav/card filtering)

As of component B1, the web UI mirrors the server's role→permission matrix
(`docs/auth.md#roles--permissions`) so nav items, Admin-hub cards, and route
access all agree with what the server would actually allow.

`can(principal, perm)` (`src/auth/policy.ts`) is the single source of truth
on the client: a `GRANTS` map from role → the set of `Permission` strings it
holds, kept in lockstep with the Go grants map
(`internal/auth/policy/policy.go`) and the matrix in `docs/auth.md`. It
returns `true` whenever `principal.kind === 'anonymous'` — the principal
`/auth/me` returns when the server has `auth.enabled=false` — so an auth-off
deployment never hides anything, and `false` for a `null` principal (identity
not yet resolved).

`<RequireRole permission="…">` (`src/components/RequireRole.tsx`) is a route
guard: it renders `children` if `can(principal, permission)`, otherwise a
friendly "Not authorized" page instead of a broken fetch. Wrap any
admin/operator-only route in `routes.tsx` with it (see the `infra.manage`-,
`users.manage`-, and `jobs.write`-gated routes there) so deep-linking to a
page a role can't use degrades cleanly.

Nav and card registries filter the same way: `Sidebar.tsx`'s `PHASE1_NAV`
entries carry an optional `permission`, and `visibleNavItems` drops any item
`can()` denies; `Admin.tsx`'s `ADMIN_LINKS` entries each carry a required
`permission`, and the card grid is `ADMIN_LINKS.filter((link) =>
can(principal, link.permission))`. A control that mutates state (a Delete
button, a create form) should be hidden or disabled with the same `can()`
check rather than relying on the route guard alone — the guard stops
navigation, `can()` stops rendering the affordance in the first place.

Prefer gating on a permission from `principal.permissions` (via `can()`) rather
than on a role name. The server computes permissions from the policy matrix, so
an externally-mapped role from LDAP/OIDC needs no client change.

---

## Adding a new API query hook

The REST layer lives in `web/src/api/`. A read endpoint is added in three steps:
the wire type, a raw fetch function, and a TanStack Query hook.

1. **Type the response** in `src/api/types.ts`, matching the server's JSON wire
   format (verify against `GET /api/v1/openapi.yaml`):

   ```ts
   /** Wire shape returned by GET /api/v1/presets. */
   export interface Preset {
     id: string
     name: string
     // …
   }
   ```

2. **Add a raw fetch function** and a query-key entry in `src/api/queries.ts`.
   The key factory enables prefix-based invalidation:

   ```ts
   export const queryKeys = {
     // …
     presets: { all: ['presets'] as const },
   }

   export function fetchListPresets(): Promise<ListResponse<Preset>> {
     return apiFetch('/presets')
   }
   ```

   `apiFetch<T>` (in `src/api/client.ts`) sets the JSON headers, prefixes
   `/api/v1`, and throws a typed `ApiError` on non-2xx responses.

3. **Wrap it in a `useQuery` hook**:

   ```ts
   /** List all presets. */
   export function useListPresets() {
     return useQuery({
       queryKey: queryKeys.presets.all,
       queryFn: fetchListPresets,
     })
   }
   ```

For a write endpoint, add a `useMutation` hook in `src/api/mutations.ts` that
calls `apiFetch` with the appropriate method and invalidates the affected query
keys in `onSuccess` (see `useCancelJob` / `useRetryJob` / `useSubmitJob` for the pattern). Add
unit tests next to the code, mirroring `client.test.ts`, `queries.test.ts`, and
`mutations.test.tsx`.

---

## Adding a WebSocket subscription

Live updates flow over a single WebSocket managed by `src/ws/`. To react to push
messages in a component:

1. Confirm (or add) a typed payload in `src/ws/events.ts` with a matching type
   guard (`isJobEvent`, `isTaskEvent`, `isWorkerEvent` are the existing ones).
2. Subscribe with the `useWebSocket(subject, handler)` hook from
   `src/ws/context.tsx`. It subscribes on mount and unsubscribes automatically
   on unmount (or when `subject` changes), and stabilises the handler so inline
   arrow functions are safe:

   ```tsx
   useWebSocket('workers', (payload) => {
     if (isWorkerEvent(payload)) {
       // update local state / patch the query cache in place
     }
   })
   ```

Subjects mirror the server's hub (e.g. `jobs`, `jobs/{jobId}/tasks`, `workers`,
`tasks/{taskId}/logs`). The provider lives at the app root
(`WebSocketProvider` in `main.tsx`), and `ConnectionStatusBadge` surfaces the
connection state in the sidebar so operators can tell whether live updates are
flowing.

`JobList` exposes per-row and bulk retry affordances: each row for a job with
`failed` or `canceled` tasks shows a **Retry** button (backed by `POST
/api/v1/jobs/{id}/retry` via the `useRetryJob` mutation); the bulk toolbar
shows a **Retry M** button when retryable jobs are selected alongside (or
instead of) cancelable ones. The select-all header includes retryable jobs in
its scope.

The `jobs` subject carries a synthetic `status: "removed"` event when a job is
hard-deleted — either by a per-row or bulk **Delete** action in `JobList`, or
automatically by the server's retention sweep. `JobList` handles this by
removing the row from the displayed list immediately on receipt. Bulk delete
shows a confirmation dialog before issuing the requests. When adding components
that display job data from the `jobs` subject, handle the `removed` status to
avoid stale rows lingering in the UI.

`JobDetail` also surfaces *why* tasks failed: a job-level failure banner
(built from the `JobDetail.failure_summary` field — omitted until at least one
task has failed) above the task list, and a per-task reason string next to
each failed row's status (`Task.failure_reason`). Neither needs a dedicated
WebSocket message type — both are ordinary REST fields on the existing
job/task responses, refreshed the same way the rest of the page is: `JobDetail`
already invalidates the job-detail and task-list queries on `jobs` and
`jobs/{jobId}/tasks` WebSocket events, so the banner and reason strings update
live along with everything else. See
[`docs/observability.md`](observability.md#why-did-my-task-fail) for what the
reason strings mean and where the underlying data comes from.

Each task row is also independently expandable to a full **attempt
timeline**: a disclosure button toggles a detail row rendering every attempt
(number, status, worker, exit code, the per-attempt message, and
started→ended duration) via `GET /api/v1/tasks/{id}/attempts`. Unlike the
failure banner and reason string above, this data is fetched lazily — the
`useTaskAttempts(id, { enabled })` hook (`src/api/queries.ts`) only queries
once a row is expanded, so collapsed rows cost nothing. The `jobs/{jobId}/tasks`
WebSocket handler invalidates `queryKeys.tasks.attempts(payload.task_id)` on
every task event; TanStack Query only refetches an invalidated query while it
has an active (i.e. expanded) observer, so an open timeline stays live and a
collapsed one just refetches on next expand. This is what lets an operator
see the reason a specific attempt failed even for a task mid-retry, whose
task-level `failure_reason` has already been cleared — see
[Attempt history](architecture.md#5-status-ingestion) in the architecture doc.

---

## TypeScript conventions

- **Strict everywhere.** `tsconfig` enables `strict`, `noUncheckedIndexedAccess`,
  and `exactOptionalPropertyTypes`. Index access yields `T | undefined`; handle
  it rather than asserting. Optional properties are not implicitly `undefined`-
  assignable — model truly-absent fields with `?`.
- **No `any`.** Parse unknown input (e.g. WebSocket payloads) as `unknown` and
  narrow with type guards, as `src/ws/events.ts` does.
- **Path alias.** Import with `@/` (→ `src/`), not relative `../../` chains.
- **Wire types are the contract.** Keep `src/api/types.ts` and `src/ws/types.ts`
  aligned with the OpenAPI spec and the server's wire structs; the spec is
  authoritative when they diverge.
- **Functional components and hooks** only; styles via CSS Modules
  (`*.module.css`) co-located with the component.
- **JSDoc on public API.** Exported functions and types in `src/api/` and
  `src/ws/` carry JSDoc so editors surface descriptions without a round-trip to
  the OpenAPI spec.

See the project [`CONTRIBUTING.md`](contributing.md) "Web UI contributions"
section for the component testing approach and styling conventions, and
[`web-accessibility.md`](web-accessibility.md) for the accessibility baseline.
