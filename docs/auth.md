# Authentication (Phase 3)

sqi ships with **authentication off by default** — on a trusted local network,
every request is served as an anonymous superuser and nothing is gated. This is
the pre-Phase-3 behaviour and remains the default.

## The opt-in gate

The single switch is `auth.enabled` (config file `auth.enabled`, env
`SQI_AUTH_ENABLED`, flag `--auth-enabled`; default `false`).

As of component A1, the gate is live: flipping `auth.enabled` to `true` and
restarting actually locks the server down. Every REST request and the
WebSocket upgrade now require a valid session (see below); there is no more
"scaffolding only" caveat. See [Local accounts](#local-accounts) and
[Login & sessions](#login--sessions) for what that means in practice, and
[First-admin bootstrap](#first-admin-bootstrap) for how to get your first
credential.

## Model

Every request carries a `Principal` (subject, display name, roles, kind,
superuser flag) in its context. When auth is off, the middleware injects an
anonymous principal with the superuser flag set, so authorization checks
(added in a later component) are bypassed. Authentication is pluggable: an
`Authenticator` resolves a request's credentials to a `Principal`, and future
credential types (API keys, LDAP, OIDC) each implement that one interface.
Today, the only non-anonymous `Authenticator` is the session-cookie one
described below.

A `Principal`'s `roles` field is populated (a user's single stored role, e.g.
`["admin"]`) but **nothing reads it to make an authorization decision yet** —
see the interim gap called out in [Local accounts](#local-accounts). Role
enforcement is component B1.

The REST resource routes are gated by the auth middleware; the WebSocket
upgrade is gated by its own hook; the health/readiness/metrics probes and the
OpenAPI spec are always public.

## Local accounts

A local account (`internal/store` `User`) has a username (case-insensitive
unique), a display name, a `role` (`admin`, `operator`, `user`, or
`read-only`; defaults to `user` if omitted), and a `disabled` flag. Passwords
are hashed with argon2id (OWASP-baseline parameters: 19 MiB memory, 2
iterations, 1 thread — `internal/auth/password/password.go`) and never
returned by any endpoint.

Accounts are created two ways:

- **Bootstrap** — exactly one admin account, seeded at startup. See
  [First-admin bootstrap](#first-admin-bootstrap).
- **The `/users` REST API** (and the web Admin → Users page) —
  `POST /api/v1/users`, `GET /api/v1/users`, `GET /api/v1/users/{id}`,
  `PATCH /api/v1/users/{id}` (display name / role / disabled),
  `PUT /api/v1/users/{id}/password`, `DELETE /api/v1/users/{id}`.

> **Interim authorization gap — read this before relying on roles.**
> `role` is stored on every account and returned by the API, but **no
> endpoint enforces it yet**. The `/users` routes (and every other
> authenticated route) are gated only on "is this an authenticated
> principal", not on "does this principal have the right role". Concretely:
> with `auth.enabled=true`, **any successfully logged-in user — including one
> created with `role: "user"` or `role: "read-only"` — can create new admin
> accounts, disable or delete any account (including its own), and change
> anyone's password.** This is a known, deliberate interim state on the way
> to full RBAC, not a bug: role-based enforcement is component B1. Until B1
> ships, treat every local account as equivalent to admin, and grant accounts
> only to people you'd trust with full user-management access.

Disabling a user (`disabled: true`) takes effect immediately: the session
authenticator re-checks the user record on every request and rejects a
disabled account's session outright (`internal/auth/session/session.go`), and
`POST /auth/login` refuses a disabled account with the same generic 401 as a
bad password. Deleting a user cascades to its sessions — see
[Login & sessions](#login--sessions).

## Login & sessions

`POST /api/v1/auth/login` takes `{"username", "password"}` and, on success,
mints a server-side session and sets it as a cookie via `Set-Cookie`. On
failure it returns a **401 with an identical body** whether the username is
unknown, the password is wrong, or the account is disabled — deliberately, so
the endpoint can't be used to enumerate valid usernames. (The unknown-user
path still runs a dummy argon2id verify so it costs about the same time as a
real check — otherwise the *response latency* alone would leak which
usernames exist, even with identical bodies.)

The cookie:

| Attribute | Value |
|---|---|
| Name | `auth.session.cookie_name`, default `sqi_session` |
| `HttpOnly` | always set — client-side JS can never read it |
| `SameSite` | always `Lax` |
| `Secure` | per `auth.session.cookie_secure` — `"auto"` (default), `"true"`, or `"false"` |
| `Max-Age` | `auth.session.ttl` in seconds, default `168h` (7 days) |

`cookie_secure` is a **3-valued string, not a bool**, because sqi's default
deployment posture is a trusted, plain-HTTP LAN (`http.addr` defaults to
`0.0.0.0:8080`, no TLS). `"auto"` sets `Secure` when the request arrived over
TLS or carries `X-Forwarded-Proto: https`, which is right behind a
TLS-terminating proxy that sets that header — but an operator on plain HTTP,
with no such proxy, needs to be able to force `Secure` off explicitly rather
than depend on `"auto"` guessing right; hence `"false"` as an explicit option,
and `"true"` for the reverse (force it on even if the auto-detection would
miss it).

Session expiry is **absolute**, not sliding: `auth.session.ttl` sets the
lifetime from creation, and using a session does not extend it. A session
becomes invalid at exactly `created_at + ttl` regardless of activity; the
user must log in again.

`POST /api/v1/auth/logout` deletes the session server-side and clears the
cookie; it always returns `204` (even if the cookie was already invalid).
`GET /api/v1/auth/me` returns the current `Principal` — `401` if
unauthenticated, otherwise the resolved subject/display name/roles/kind. This
is the single endpoint the web UI polls to decide shell-vs-login; see
[`docs/web-development.md`](web-development.md).

Deleting a user (`DELETE /api/v1/users/{id}`) cascades to its sessions at the
database level (`sessions.user_id REFERENCES users(id) ON DELETE CASCADE`) —
every session belonging to a deleted user is revoked immediately, in the same
transaction as the delete.

## First-admin bootstrap

When `auth.enabled=true` and the `users` table is empty, `sqi-server` seeds a
single admin account at startup from `auth.bootstrap.username` /
`auth.bootstrap.password` (env `SQI_AUTH_BOOTSTRAP_USERNAME` /
`SQI_AUTH_BOOTSTRAP_PASSWORD`):

```sh
SQI_AUTH_ENABLED=true \
SQI_AUTH_BOOTSTRAP_USERNAME=admin \
SQI_AUTH_BOOTSTRAP_PASSWORD=change-me-after-first-login \
./bin/sqi-server serve
```

This creates one `role: "admin"` account named `admin`. Log in with it, then
immediately set a real password via `PUT /api/v1/users/{id}/password` (or the
web Admin → Users page) — the bootstrap password is meant to be transient.

Bootstrap behavior:

- **Idempotent and non-destructive.** It only runs when the `users` table is
  empty. Once any user exists — bootstrapped or otherwise — it is a
  permanent no-op, even if the bootstrap env vars are still set; it never
  overwrites an existing account's password.
- **Empty and unconfigured does not fail closed.** If auth is enabled, the
  table is empty, and *neither* bootstrap variable is set, the server logs a
  `WARN` ("auth is enabled but no users exist and no bootstrap credentials
  are configured…") and **still boots successfully**. There is simply no one
  who can log in yet — the server is up but practically unusable for any
  authenticated route until an operator sets the bootstrap env vars and
  restarts, or otherwise seeds a user directly.
- **A half-set pair is a startup validation error**, not a warning: setting
  only `auth.bootstrap.username` or only `auth.bootstrap.password` (e.g. a
  typo'd env var name) fails config validation and the server does not
  start — this guards against silently creating (or trying to create) an
  admin with an empty password.

## CSRF & CORS

A session cookie is an **ambient credential**: once set, the browser attaches
it automatically to every request to this origin — including ones a
malicious third-party page initiates (classic CSRF), and including the
WebSocket upgrade (cross-site WebSocket hijacking is the same vector applied
to `/api/v1/ws`). A stateless `Authorization` header client has no such
problem, because nothing attaches it automatically; cookies need explicit
defenses that header-based auth doesn't.

sqi's model, mounted only when auth is enabled:

- The cookie itself carries `SameSite=Lax`, which already blocks the cookie
  from being sent on most cross-site subrequests (though not top-level
  navigations, which `Lax` still allows).
- On top of that, `internal/middleware/csrf.go` enforces an **Origin check on
  unsafe methods** for cookie-authenticated requests: `GET`/`HEAD`/`OPTIONS`
  are never checked (they must not have side effects); a request that does
  **not** carry the session cookie passes through unchecked (it isn't
  cookie-authenticated, so there's no ambient-credential vector to guard);
  an unsafe-method request that **does** carry the cookie must present an
  `Origin` (falling back to `Referer`'s origin) that is either same-origin or
  in an explicit allow-list — otherwise it's rejected with `403`. A
  cookie-bearing unsafe request with neither header at all is rejected too:
  browsers always send one or the other on such requests, so their total
  absence isn't trusted.
- CORS enables the credentialed cross-origin case at the browser level:
  `Access-Control-Allow-Credentials` is only ever sent when auth is enabled.
  A wildcard `*` origin can never be combined with credentials (browsers
  reject the combination outright), so if the configured origin list still
  contains `"*"` once auth is enabled, the router drops the wildcard and logs
  an error rather than silently disabling credentials or leaving the
  combination broken.

The **normal deployment is same-origin** — `sqi-server` serves the built web
UI itself (`web/dist/` embedded via `internal/ui`) — so none of this affects
the shipped UI in its default configuration. It only matters for a
separately-hosted UI (a different scheme/host/port) that wants to call a
`sqi-server` instance's cookie-authenticated API cross-origin.

> **Known gap: no operator-facing knob to allow-list origins yet.** The
> underlying mechanism (`CORSOrigins` on `internal/api.Config`) supports an
> explicit allow-list, and is exercised by tests — but `cmd/sqi-server`
> does not currently expose it through the config file, `SQI_*` environment
> variables, or a CLI flag; `serve.go` never sets it, so it is always the
> Go zero value (empty), which the router treats as `["*"]`. Combined with
> the wildcard-drop above, this means that **with auth enabled, cross-origin
> browser access is always rejected** in the shipped binary today — not just
> "unless configured", but unconditionally, since there is presently no
> supported way to configure it. This only affects a separately-hosted UI
> (same-origin deployments are unaffected); adding a config surface for it is
> tracked as follow-up work, not part of A1.

## Headless / SDK auth

A1 ships **no issuable headless credential**. The only working authentication
path is the browser session cookie described above — there is no bearer
token, API key, or service-account mechanism a script or DCC submitter can
obtain today, and the only `Authenticator` wired into the server is the
session-cookie one (`internal/server/server.go`'s `selectAuth`).

The Python SDK (`clients/python`) is nonetheless already wired for it ahead
of time: `SqiClient(base_url, token=...)` sends
`Authorization: Bearer <token>`, falling back to the `$SQI_TOKEN` then
`$SQI_API_KEY` environment variables when `token` isn't passed explicitly,
and a 401/403 response raises the typed `SqiAuthError`
(`clients/python/src/sqi_client/errors.py`). Be clear-eyed about what this
means today, though: **with `auth.enabled=true`, passing `token=` accomplishes
nothing** — the server has no code path that reads or validates an
`Authorization` header, so a bearer-token request is simply treated as
unauthenticated and gets a `401` from `GET /auth/me` and everything else.
Issuable API keys — the piece that makes this plumbing actually work
end-to-end — are component **A2**. Until then, enabling auth effectively
locks out any headless/SDK/submitter usage of a given `sqi-server` instance;
keep auth off for farms driven primarily by scripts or DCC submitters until
A2 ships.

## Coming next

- A2 — API keys for headless SDK/submitter use.
- B1 — role-based access control (admin / operator / user / read-only),
  enforcing the `role` field that A1 already stores but does not check.
