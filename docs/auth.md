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

Every request carries a `Principal` in its context. When auth is off, the
middleware injects an anonymous principal with the superuser flag set, so
authorization checks are bypassed. Authentication is pluggable: an
`Authenticator` resolves a request's credentials to a `Principal`. Today the
only non-anonymous `Authenticator`s are the session-cookie and API-key ones
described below.

Directory-backed credentials do **not** implement that interface.
[LDAP/AD](#ldap--active-directory) (C1), and OIDC (C2) after it, attach at
`POST /auth/login` instead: they verify the credential once and then mint an
ordinary session, so no request path binds against an external identity
provider. See [It attaches at login, not at every
request](#it-attaches-at-login-not-at-every-request).

`Principal` carries `Subject` (opaque user id), `Username` (login name — the
value bound to `Job.Owner`/`Job.Submitter`), `DisplayName`, `Roles`, `Kind`,
and `Superuser`.

`GET /auth/me` returns both `roles` and `permissions`. Clients should gate on
`permissions`: it is computed server-side from the policy matrix, so an
externally-mapped role from an LDAP or OIDC provider (C1/C2) needs no
client-side change. A superuser principal — the anonymous identity used when
auth is disabled — reports the full permission set, which is what keeps every
control enabled in an auth-off deployment.

A `Principal`'s `roles` field is populated (a user's single stored role, e.g.
`["admin"]`) and, as of component B1, **is enforced**: every mutating route
and several read routes are gated by a role→permission policy. See
[Roles & permissions](#roles--permissions) for the matrix.

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

Disabling a user (`disabled: true`) takes effect immediately: the session
authenticator re-checks the user record on every request and rejects a
disabled account's session outright (`internal/auth/session/session.go`), and
`POST /auth/login` refuses a disabled account with the same generic 401 as a
bad password. Deleting a user cascades to its sessions — see
[Login & sessions](#login--sessions).

## Roles & permissions

As of component B1, roles are enforced on every route. There are four
built-in roles (no custom-role builder — YAGNI):

- **admin** — full access, including user management, API-key management for
  any account, and configuration-adjacent surfaces.
- **operator** — runs the farm: all jobs, workers, farm infrastructure
  (farms/queues/storage/compute/usage-pools), products/presets, and
  diagnostics (server log).
- **user** — submit and control jobs; manage their own API keys; read-only on
  infrastructure.
- **read-only** — reads the operational surface; no mutations anywhere;
  cannot see diagnostics or the user list — but *can* manage its own API
  keys.

| Permission | read-only | user | operator | admin |
|---|:-:|:-:|:-:|:-:|
| jobs.read | ✅ | ✅ | ✅ | ✅ |
| `jobs.read.all` — see jobs owned by anyone | ✅ | ❌ | ✅ | ✅ |
| jobs.write | ❌ | ✅ | ✅ | ✅ |
| `jobs.submit_as` — set `Owner` to another user | ❌ | ❌ | ✅ | ✅ |
| workers.read | ✅ | ✅ | ✅ | ✅ |
| workers.manage | ❌ | ❌ | ✅ | ✅ |
| infra.read (farms/queues/storage/compute/usage-pools) | ✅ | ✅ | ✅ | ✅ |
| infra.manage | ❌ | ❌ | ✅ | ✅ |
| products.read (products/presets) | ✅ | ✅ | ✅ | ✅ |
| products.manage | ❌ | ❌ | ✅ | ✅ |
| diagnostics.read (server log) | ❌ | ❌ | ✅ | ✅ |
| users.read | ❌ | ❌ | ❌ | ✅ |
| users.manage | ❌ | ❌ | ❌ | ✅ |
| apikeys.self (own keys) | ✅ | ✅ | ✅ | ✅ |
| apikeys.admin (anyone's keys) | ❌ | ❌ | ❌ | ✅ |

`apikeys.admin` is enforced by `GET /users/{id}/api-keys` and
`DELETE /users/{id}/api-keys/{keyId}` — see [API keys](#api-keys).

A denied request returns **403** with an RFC-7807 problem-details body, and
is recorded to the audit log (`AuditEntry.Actor`) as well as the server's own
diagnostic log. With `auth.enabled=false` (the default), the anonymous
superuser principal bypasses every check — unchanged behavior from before
B1.

**Last-admin guard.** The last *enabled* admin account can't be deleted,
disabled, or demoted to a non-admin role — any of those requests fail with
**409 Conflict** — so an operator can never lock themselves (or everyone)
out of user management.

## Self-service account changes

Any authenticated principal may change its own display name and password.
Both routes resolve their target from the session — there is no id in the
path — so reaching another account is structurally impossible rather than
guarded against. The web surfaces them at `/account`, linked from the
sidebar identity control.

| Route | Effect |
|---|---|
| `PATCH /api/v1/auth/me` | Sets `display_name`. Returns the same principal shape as `GET /auth/me`. |
| `PUT /api/v1/auth/password` | Verifies the current password, then sets the new one. |

Three choices worth knowing:

- **Only `display_name` is accepted.** `role`, `disabled`, and `username`
  are absent from the request type, so a body carrying them is inert — a
  self-service route that could reach `role` would be a privilege-escalation
  hole, and this makes that unrepresentable rather than merely checked for.
- **A wrong current password is 403, not 401.** The caller *is*
  authenticated and only failed a re-auth check; a 401 would trip the web's
  login interceptor and eject them mid-form.
- **Changing a password evicts every session for the account, then
  re-issues one for the caller** — other devices are signed out while the
  device that made the change stays signed in. **API keys are deliberately
  not revoked**: they are an independent credential, and silently killing a
  user's automation because they rotated a password would be a nasty
  surprise. Revoke them explicitly if that is what you want.

With `auth.enabled=false` both routes return **409 Conflict** — the
anonymous superuser has no account record to change — and the web hides the
Account link entirely.

## Job identity

`POST /api/v1/jobs` and `POST /api/v1/products/{name}/jobs` resolve the
`Owner` and `Submitter` persisted on a job from the authenticated principal
and whatever the client supplied, following this precedence:

1. Submitter is always the principal's username. A client value is discarded
   silently, never an error — a client asserting its own identity is
   meaningless rather than hostile, and erroring would break every existing
   submitter the moment auth is switched on.
2. Owner defaults to Submitter when the client supplies none.
3. Owner equal to self (case-insensitive) is accepted; the principal's own
   canonical casing is stored, not the client's.
4. Owner other than self requires policy.JobsSubmitAs, else 403.

An owner naming no known user is rejected with 400 when
`auth.validate_job_owner` is on (the default).

WebSocket delivery is scoped the same way as REST. Per-job subjects
(`jobs/{id}/tasks`, `tasks/{id}/logs`) are authorized once at subscribe time;
the global `jobs` subject is filtered per event. A client that cannot resolve a
job's owner receives nothing for it rather than everything.

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
cookie; it always returns `200` with a JSON body (even if the cookie was
already invalid). The body is `{}` for a local logout, and carries
`redirect_url` — the identity provider's RP-initiated logout URL — when SSO is
configured with `auth.oidc.logout_mode=provider`.
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

> **Note — a WebSocket is authenticated once, at upgrade.** Authentication and
> owner-scope resolution happen a single time, when `/api/v1/ws` is upgraded;
> they are **not** re-evaluated for the life of the connection. Disabling the
> account, revoking its session, or changing its role does **not** drop a live
> WebSocket — the connection keeps delivering until the client closes it or it
> hits the idle timeout (`wsIdleTimeout`, 5 minutes, in `internal/api/ws.go`).
> The exposure is bounded to that one connection's own already-authorized scope:
> it can only keep receiving what it was authorized to receive at upgrade, with
> no cross-user leakage or privilege escalation. REST is different — every
> request re-authenticates and re-authorizes, so a revoked credential stops
> working on the next call.

### Configuring allowed origins

Name the origins a separately-hosted UI will call from, through any of the
three config layers (later beats earlier):

```yaml
http:
  cors_origins:
    - "https://ui.example.com"
    - "http://localhost:5173"
```

```sh
SQI_HTTP_CORS_ORIGINS="https://ui.example.com,http://localhost:5173"
sqi-server serve --http-cors-origins=https://ui.example.com
```

Each entry must be `scheme://host[:port]` or `"*"`. A trailing slash, a
path, a query, a fragment, or embedded whitespace is rejected at startup
with a `http.cors_origins` validation error — go-chi/cors could never match
such a value, so a typo fails loudly at boot instead of silently at request
time. **Wildcard patterns other than the bare `"*"` are rejected too**: an
entry containing an embedded `*` (e.g. `https://*.example.com` or
`https://app.example.com*`) is refused at startup, because go-chi/cors would
otherwise honor it as a prefix/suffix match — with credentials, once auth is
enabled — letting an attacker-registrable origin (like
`https://app.example.com.evil.io`) ride a victim's session cookie. Name every
allowed origin explicitly.

Leaving the list empty keeps the previous default of `["*"]`. **The
wildcard-drop above still applies**: with auth enabled, `"*"` — whether
explicit or defaulted — is dropped and credentialed cross-origin requests
are refused. A separately-hosted UI must therefore name its origin
explicitly here. Same-origin deployments need none of this.

## Headless / SDK auth

As of component A2, sqi has an issuable headless credential: **API keys**,
covered in full below. `internal/server/server.go`'s `selectAuth` now wires
`auth.Chain(keyAuthn, sessAuthn)` — a Bearer API key is tried first, and the
session cookie is the fallback for browser requests.

The Python SDK (`clients/python`) was already wired ahead of time for this:
`SqiClient(base_url, token=...)` sends `Authorization: Bearer <token>`,
falling back to the `$SQI_TOKEN` then `$SQI_API_KEY` environment variables
when `token` isn't passed explicitly, and a 401/403 response raises the typed
`SqiAuthError` (`clients/python/src/sqi_client/errors.py`). The submitter
framework (`clients/submitter`) resolves a key the same way, one tier
simpler: an `api_key` argument, then `$SQI_API_KEY`, then the `api_key` key
in `~/.sqi/submitter.json` — see `clients/submitter/README.md`. With
`auth.enabled=true`, issue yourself a key (`POST /api/v1/api-keys` or the web
Admin → API Keys page) and pass it via `token=`/`$SQI_TOKEN`/`$SQI_API_KEY`
(SDK) or `api_key=`/`$SQI_API_KEY`/`submitter.json` (submitter) to unblock
headless usage.

## API keys

An API key is a per-user, `sqi_`-prefixed Bearer credential for scripts, the
SDK, and DCC submitters — the machine/headless counterpart to the browser
session cookie above.

**Issuance.** `POST /api/v1/api-keys` with `{"name", "expires_at"?}` creates
a key owned by the calling principal and returns it once
(`internal/api/apikeys.go`):

```json
{
  "id": "…", "name": "render farm", "prefix": "sqi_AbCdEfGh",
  "expires_at": null, "last_used_at": null, "created_at": "…",
  "secret": "sqi_AbCdEfGh1234…"
}
```

The `secret` field — the full raw key — is present **only** in this create
response. Every other response (`GET /api/v1/api-keys`, the list on the web
Admin → API Keys page) omits it and shows the `prefix` instead, so copy the
secret down before navigating away; it cannot be recovered later, only
revoked and reissued.

**Presentation.** Clients send `Authorization: Bearer <key>`. Keys are the
credential for headless/machine access; browser sessions stay cookie-based as
described above — see [Headless / SDK auth](#headless--sdk-auth) for how the
SDK and submitter pick a key up from an argument, environment variable, or
settings file.

**Storage & security.** The raw key is generated as 256 bits of random data,
base64url-encoded, and prefixed `sqi_`. Only its hex SHA-256 digest
(`internal/auth/password.HashToken`) is stored in the `api_keys` table —
never the raw key — alongside a 12-character display `prefix` taken from the
start of the raw key, used to tell keys apart in the list view without
revealing the secret. `last_used_at` is updated on successful authentication,
throttled to at most once per minute per key so a busy key doesn't write on
every request (`internal/auth/apikey.Authenticator`'s `touchThreshold`).
Optional `expires_at` is enforced at authentication time, not just at
creation: `GetAPIKeyByTokenHash` only matches rows that are unexpired (and
unrevoked), so an expired key stops authenticating the instant it lapses,
with no separate sweep required.

**Revocation.** `DELETE /api/v1/api-keys/{id}` is a soft revoke — it sets
`revoked_at` rather than deleting the row — and takes effect immediately:
the same "unrevoked" filter that enforces expiry means the very next request
bearing that key is rejected.

**Scope.** `POST`/`GET`/`DELETE /api/v1/api-keys` are self-scoped: they
resolve the caller's own user id and only ever see or touch that user's
keys (`apikeys.self`, held by every role).

Admins additionally hold `apikeys.admin`, which unlocks two cross-user
routes:

| Route | Effect |
|---|---|
| `GET /api/v1/users/{id}/api-keys` | List that user's keys (metadata only — never a secret). |
| `DELETE /api/v1/users/{id}/api-keys/{keyId}` | Revoke one of that user's keys. |

**There is deliberately no admin create.** An admin may see and revoke
another person's keys, but minting a credential someone else is accountable
for is a materially different act, so no route offers it.

Revocation stays owner-scoped underneath: a `keyId` that does not belong to
the named user returns **404**, so that existing scoping *is* the
authorization check rather than a separate ownership branch that could be
forgotten. The web surfaces this at `/users/{id}/api-keys`, reachable from
the per-row "API keys" action on Admin → Users.

**Auth-off behaviour.** With `auth.enabled=false`, every request is the
anonymous superuser principal, which has no real user id to own a key
against, so all three `/api-keys` endpoints reject with `409 Conflict`
("API keys require authentication to be enabled") rather than silently
operating on a fake account — consistent with the rest of the auth-off
posture elsewhere in this doc.

**CSRF.** A Bearer request carries no cookie, so the CSRF guard in
[CSRF & CORS](#csrf--cors) never engages for it: that guard only inspects
requests that carry the session cookie in the first place, and a
Bearer-authenticated request has nothing for it to check.

## LDAP / Active Directory

As of component C1, `sqi-server` can verify passwords against an LDAP or
Active Directory server instead of its own store. Enable it with
`auth.ldap.enabled` on top of `auth.enabled` — LDAP is an addition to the
auth system, not an alternative to it, and an auth-off server never contacts
a directory whatever `auth.ldap.*` says. Every field is catalogued in
[`docs/configuration.md`](configuration.md#authldap).

### It attaches at login, not at every request

LDAP is a **login-time credential verifier**, not an `Authenticator`. This is
a deliberate departure from the obvious design (an `Authenticator`
implementation alongside the session and API-key ones): a per-request
authenticator would mean a directory bind on *every API call*, which turns
the DC into a hard dependency of every page load and every SDK poll.

Instead, `POST /api/v1/auth/login` checks the password against the directory
and then mints **the same server-side session a local account gets**. After
that moment nothing is LDAP-specific: `auth.Chain(apikey, session)` is
untouched, and the session cookie, its TTL, roles and permissions, job owner
binding, and WebSocket scoping all behave exactly as documented above. A
directory outage blocks new *logins*; it does not disturb sessions already
issued.

The practical consequence is [revocation lag](#revocation-lag), below.

### Per-account routing

Each account carries `users.auth_source`, either `local` or `ldap`. It is set
when the account is created and is **immutable** — no route can change it.
Login reads it and consults exactly one backend: the stored argon2id hash, or
the directory. Never both, never in sequence.

Not chaining the two is a security decision, not an optimization. If a failed
local login fell through to a directory bind, every wrong password in sqi
would become a failed bind against a real DN — and in Active Directory
repeated bad binds *lock the directory account*. A brute force against sqi
would become an org-wide denial of service.

The same routing means **a local account shadows a same-named directory
account outright**. If `alice` exists locally, a directory `alice` can never
log in, and the directory is not even contacted. The reverse — a directory
login adopting an existing local record — is refused rather than allowed:
adopting would mean anyone who can create a directory account named `admin`
inherits the local admin. Both cases return the same generic 401, so the
users page (Admin → Users) shows each account's source; that column is
usually the fastest way to see *why* a login is being refused.

### Just-in-time provisioning

An unknown username with LDAP enabled is a provisioning event: sqi binds
against the directory, maps a role from the returned groups, and creates a
local record with `auth_source: ldap`. There is no import step and no
pre-registration. The stored `password_hash` is an unusable placeholder, not
a copy of the directory password.

The display name is read from `display_name_attr` **once, at creation, and
never re-synced**. That is what makes `PATCH /api/v1/auth/me` meaningful for
a directory user — a self-service display-name edit persists instead of being
overwritten at the next login. The trade is that a name changed in the
directory (a marriage, a correction) does not propagate; an admin edits it,
or the user does.

### Accounts are matched on a stable identifier, not a username

`unique_id_attr` is **required** whenever LDAP is enabled and has **no
default**. Set it to `objectGUID` on Active Directory, or `entryUUID` on
OpenLDAP and other RFC 4530 servers. No single value is correct on both, and
guessing on a server that exposes both would silently pick the wrong one.

sqi stores that value in `users.external_id` and matches every later login on
it, never on the username. A username is not an identity: directories recycle
login names and email addresses, so a new hire given a departed admin's name
would otherwise log straight into that admin's account — same role, same owned
jobs, no error anywhere. A rename at the directory is the mirror failure, and
would orphan the account and provision a duplicate.

Two consequences worth stating plainly:

- **A directory rename is transparent.** The entry keeps its identifier, so
  the same sqi account is reached under the new name.
- **A recycled username is refused, not adopted.** A new directory entry
  wearing an old name has a new identifier, so provisioning runs and collides
  on the taken username. The login fails until an operator renames or removes
  one of the two accounts. Refusal is the intended outcome.

Active Directory returns `objectGUID` as raw binary, which sqi hex-encodes
before storing. That encoding is permanent — changing it would orphan every
account already stamped.

#### Upgrading from an earlier sqi

LDAP accounts provisioned before identifier matching shipped carry an empty
`external_id`. They **cannot log in** once matching is in effect: the identity
lookup misses, provisioning collides on the username, and the result is a
permanent 401.

This is deliberate. Adopting a row whose stored identifier is empty is
username matching under another name and would preserve the recycling hazard
indefinitely, for exactly the long-lived, often privileged accounts most
likely to predate the upgrade. **Delete and recreate such accounts**; the next
login re-provisions them with the directory's identifier. The server logs an
`ERROR` naming the account and this remedy each time one is refused — the 401
itself is identical to every other login failure by design, so the log is the
only signal.

### Both bind modes

The two modes are mutually exclusive; setting `user_dn_template` selects
template bind, and config validation rejects any attempt to combine it with
`bind_dn`/`base_dn`.

**Search-then-bind** (the usual Active Directory shape): a service account
searches for the user's entry, then sqi binds as the DN it found.

```yaml
auth:
  enabled: true
  ldap:
    enabled: true
    url: "ldaps://dc01.example.com:636"
    bind_dn: "CN=sqi-svc,OU=Service Accounts,DC=example,DC=com"
    bind_password: "…"           # SQI_AUTH_LDAP_BIND_PASSWORD in practice
    base_dn: "DC=example,DC=com"
    user_filter: "(sAMAccountName=%s)"
    username_attr: "sAMAccountName"
    display_name_attr: "displayName"
    unique_id_attr: "objectGUID"
    nested_groups: true
    role_map:
      - group: "CN=Farm Admins,OU=Groups,DC=example,DC=com"
        role: admin
      - group: "CN=Farm Operators,OU=Groups,DC=example,DC=com"
        role: operator
    default_role: "read-only"
```

**Template bind** (typical OpenLDAP, no service account): sqi builds the
user's DN directly and binds as them, then reads their own `memberOf`.

```yaml
auth:
  ldap:
    enabled: true
    url: "ldap://ldap.example.com:389"
    start_tls: true
    user_dn_template: "uid=%s,ou=people,dc=example,dc=com"
    username_attr: "uid"
    display_name_attr: "cn"
    unique_id_attr: "entryUUID"
    role_map:
      - group: "cn=farm-admins,ou=groups,dc=example,dc=com"
        role: admin
    default_role: "read-only"
```

In both modes `%s` is the username, escaped for its context (filter escaping
for `user_filter`, DN escaping for `user_dn_template`) before substitution.

**Anonymous search is supported.** Setting `base_dn` with no `bind_dn` runs
the search on an anonymous connection, which is what a world-readable
directory wants. Setting `bind_password` *without* `bind_dn` is rejected at
boot instead: the password would be silently discarded and the search would
go out anonymously with nothing in the logs to say so.

**Alias / UPN login works.** With `user_filter: "(userPrincipalName=%s)"` and
`username_attr: "sAMAccountName"`, a user typing `alice@example.com` is
provisioned as `alice` and is recognized as that same account on every
subsequent login. The two spellings deliberately do not create two records.

**`username_attr` must name a directory-controlled, unique attribute.** It is
the identity sqi keys a local row on, and the alias-recovery path re-reads
whichever row already owns that username. Point it at something *users can
edit themselves* — `mail` is the obvious trap — and a user who sets their own
attribute to another person's value can steer that recovery onto the other
person's row and inherit their sqi role. `sAMAccountName` and `uid` are the
right kind of attribute; a self-service directory field is not. This is
bounded — a **local** row is always refused, so the bootstrap admin and every
locally-created account are out of reach, and only *directory* users can be
impersonated — and it requires a misconfiguration to reach at all. Configure
it correctly and the path does not exist.

**Nested groups are search-mode only.** `nested_groups: true` expands
transitive membership via the AD matching-rule OID; template bind reads the
flat `memberOf` attribute and cannot do it, so config validation rejects the
combination rather than silently ignoring it. Two things are worth knowing
about the expansion:

- It runs on the **service-account connection**, not the user's, so the
  service account needs read access to the group tree.
- **`base_dn` must cover the group tree, not just the user tree.** The
  expansion reuses `base_dn` as its search base. Scoping it narrowly — say
  `base_dn: "OU=Users,DC=example,DC=com"` while groups live under
  `OU=Groups` — produces a search that succeeds and matches nothing, with no
  error to notice. Set `base_dn` to a subtree containing both (commonly the
  domain root, `DC=example,DC=com`).
- If it fails **or returns nothing while flat `memberOf` had values**, sqi
  **falls back to flat `memberOf` and logs a `WARN`** rather than failing the
  login. That is the safer failure for availability, but it means a user who
  holds `admin` *only* through a nested group can be silently granted a lower
  role for that session. The warning in the server log is the only signal. If
  your admin group is nested, treat that `WARN` as an alert.
- **The matching rule is Active-Directory-only.** Verified against OpenLDAP
  2.6: it does not reject the unknown rule, it rewrites the filter to
  `(?=undefined)` and answers **success with zero entries**. So on a non-AD
  directory `nested_groups: true` never expands anything — it just takes the
  fallback path above on every login, logging a `WARN` each time. Leave it
  `false` unless you are actually on AD.

### Your directory must populate `memberOf`

**Both** bind modes read group membership from the `memberOf` attribute on the
user's own entry. Active Directory populates it natively. **OpenLDAP does
not** — it requires the `memberof` overlay to be configured on the database:

```
overlay memberof
memberof-group-oc groupOfNames
memberof-member-ad member
memberof-memberof-ad memberOf
```

Without it, every search and bind succeeds, the user authenticates, and sqi
sees **no groups at all** — so every account silently lands on `default_role`.
There is no warning for this, because "this user is in no groups" is a
legitimate state indistinguishable from "the directory never populates
`memberOf`". If every LDAP user is arriving with your `default_role`, check
the overlay before anything else.

Verify from the command line before configuring sqi — the attribute must come
back for a user you expect to be in a group:

```sh
ldapsearch -x -H ldap://ldap.example.com -D "<bind_dn>" -W \
  -b "dc=example,dc=com" "(uid=alice)" memberOf
```

The account that reads it must also be allowed to: in search mode that is the
service account (or the anonymous connection, if `bind_dn` is unset), and in
template mode it is the *user themselves*, reading their own entry.

### Group → role mapping

`role_map` is an **ordered** list of group-DN → role rules and **the first
match wins** — order is how you express precedence, so put `admin` above
`operator`. A role naming anything other than `admin`, `operator`, `user`, or
`read-only` fails config validation at boot rather than falling through to
`default_role`, which would hand out the wrong privileges with no error to
explain it.

`default_role` applies when no rule matches. Setting it to **empty rejects
the login entirely**, which is how a deployment requires group membership to
sign in at all. The default is `read-only`.

### `role_source` — who owns a user's role

| Value | Role on login | `PATCH /users/{id}` role edit |
|---|---|---|
| `directory` (default) | Recomputed from groups on **every** login | **409 Conflict** |
| `local` | Seeded from groups at JIT-create only | Allowed |

One value drives both halves, and it must stay that way. Splitting them
produces the worst outcome available here: an admin edits a role, the API
returns 200, and the next login silently reverts it with nothing to indicate
which value is real.

Switching `local` → `directory` is not a neutral change. At each user's next
login their role is recomputed from their groups, so **every manual role
assignment is overwritten**, quietly and one user at a time as people log
back in. The server logs the active `role_source` at boot so there is at
least a record of when the mode changed.

### Keep a local admin account

**In `directory` mode, a local admin account is a requirement, not a
suggestion.** Consider a renamed or deleted admin group: at their next login
every LDAP admin maps to `default_role` and is demoted. There is now no admin
in the system, and no way to fix it through the UI — role edits on directory
accounts are 409 by design, and there is nobody with `users.manage` left to
make them anyway.

A local account (the [bootstrap admin](#first-admin-bootstrap) is exactly
this) is unaffected by any directory-side change and can always log in and
repair the configuration.

**Known gap:** the [last-admin guard](#roles--permissions) does not cover
this. It refuses the last admin's deletion, disablement, or demotion *through
the API*, which is the only place it can see. A group renamed in Active
Directory reaches the same end state through a path the guard has no
visibility into. Nothing in sqi can detect that; the local admin account is
the mitigation.

### Revocation lag

Login is the only moment sqi talks to the directory, so a user disabled or
deleted in the directory **keeps their sqi session until it expires** —
`auth.session.ttl`, 7 days by default. They cannot get a *new* session, but
the one they hold keeps working.

Shorten `auth.session.ttl` if that window matters. To cut a user off
immediately, disable the account in sqi (`PATCH /api/v1/users/{id}` with
`disabled: true`): the session authenticator re-checks the user record on
every request, so it takes effect on the next call, and the local `disabled`
flag overrides the directory for both session checks and login.

sqi deliberately does **not** auto-disable accounts when a directory lookup
fails. A DC outage would otherwise mass-disable the entire farm — an
availability failure far worse than the lag it would close.

### Timing

The 401 bodies are byte-identical across every failure path — unknown user,
wrong local password, wrong directory password, directory unreachable, no
role matched, collision with a local account, disabled account,
unrecognized `auth_source`. That is tested. **The latencies are not
identical, and cannot be made so.**

The local-only equalization described under
[Login & sessions](#login--sessions) works because both sides of that
comparison are argon2id derivations of matching cost. A directory bind is a
network round trip; no local computation can be made to match it. So an
observer timing `/auth/login` can distinguish, at minimum, an immediate
rejection (a locally disabled account, a shadowed local account) from a local
password check from a directory round trip. Whether that yields useful
account enumeration depends on your directory's own timing behavior — a bind
against a nonexistent DN typically differs from one against a real DN, and
that is the directory's behavior, not something sqi can conceal.

The mitigation for the timing channel is rate limiting — but read the next
section before assuming the shipped limiter provides it.

### Brute force: what sqi actually does, and does not, protect against

**There is no login-specific throttle.** The only control is the generic
per-IP token bucket applied to all of `/api/v1` (`internal/api/router.go`):
**20 requests/second sustained, burst 40, keyed on client IP**. Nothing
counts failures, nothing backs off after a wrong password, nothing locks an
account, and nothing distinguishes `/auth/login` from a job listing.

Twenty per second is roughly **1.7 million login attempts per day, per source
IP**. Treat that as no brute-force control at all. It is a capacity guard
that keeps one client from saturating the API; it was never a credential
defense and does not become one because the login route sits behind it.

**Failed logins against directory accounts hit your directory.** For an
account whose `auth_source` is `ldap`, every wrong password produces a real
bind against a real DN. In Active Directory that increments
**`badPwdCount`**, so an unauthenticated attacker who can *name* your users
can drive them into **domain-wide lockout** — locking them out of Windows,
email, and everything else, not just sqi. That is the same end state the
[per-account routing](#per-account-routing) rule was designed to avoid for
local accounts, reached instead through the front door.

**Unknown usernames also cost the directory.** With LDAP enabled, an
unrecognized username takes the just-in-time provisioning path, which
consults the directory before failing. So `/auth/login` will happily convert
unauthenticated HTTP requests into domain-controller round trips — an
amplifier aimed at your DC.

**If any of this matters to you, put a real control in front of sqi.** A
login-specific throttle, fail2ban on the access log, or a WAF rule on
`POST /api/v1/auth/login` — something that counts failures per account and
per source and backs off. sqi does not ship one today, and the generic
limiter should not be mistaken for one. Do not expose an LDAP-enabled
deployment to the internet without it.

### A hung directory

go-ldap offers no context-aware dial, so a login in progress **cannot be
aborted by request cancellation** — a client that gives up does not free the
server-side attempt. What bounds it is `auth.ldap.timeout` (default `10s`),
applied to the TCP connect and to each subsequent request leg. Set it to
something you are willing to have a request block for; the default is not a
generous one by accident.

### Directory accounts have no local password

There is no password in sqi for a directory account to change, so the
password routes refuse rather than pretend:

| Route | On an `ldap` account |
|---|---|
| `PUT /api/v1/users/{id}/password` (admin) | **409 Conflict** |
| `PUT /api/v1/auth/password` (self-service) | **409 Conflict** |
| `PATCH /api/v1/auth/me` (display name) | **Works** |

The admin-side 409 also closes a real hole: without it, an admin could write
a genuine argon2id hash onto a directory account. Login routes on
`auth_source` and would not consult it today, but leaving a usable credential
lying in the row is the kind of thing a future refactor turns into a bypass.

`PATCH /auth/me` is deliberately *not* guarded, for the reason given under
[just-in-time provisioning](#just-in-time-provisioning): the display name is
seeded from the directory once and never re-synced, so a self-service edit
has nothing to conflict with and persists.

### Turning LDAP off strands the accounts it created

Disabling `auth.ldap.enabled` does not clean up after itself. Every row
already provisioned with `auth_source: ldap` stays exactly as it is, and
every route that touches it now refuses:

| Action on a stranded `ldap` account | Result |
|---|---|
| Login | **401** — no verifier is configured, so no credential can satisfy it |
| `PUT /users/{id}/password` | **409** — a directory account has no local password |
| `PATCH /users/{id}` role edit (under `role_source: directory`) | **409** — the role is directory-owned |

The account is unusable and unrepairable in place. **Delete and recreate it
as a local account** — that is the only remedy, and it is deliberate: there
is no conversion endpoint, because flipping an account's `auth_source` is
exactly the operation that would let a directory entry inherit a local
account's privileges (see [per-account routing](#per-account-routing)). A
missing convenience is the correct trade against a privilege-escalation
primitive.

Note that display names and role assignments do not survive that round trip,
and the user's sessions die with the old row. If you are migrating away from
LDAP, plan it as a re-provisioning exercise, not a config flag.

### How this is tested

Most tests of this feature drive a fake LDAP connection. Those cover sqi's own
logic thoroughly — routing, provisioning, role mapping, collisions, the
equalized 401 — but a fake cannot catch a mistake in the go-ldap *wire* usage:
a wrong search scope, a misnamed attribute, a filter a real server rejects, or
a server that answers an unsupported request in a way the fake never would.

So `test/integration/ldap_test.go` runs the whole login path against a **real
OpenLDAP server** in a throwaway container, in every supported configuration:
search-then-bind with a service account, template bind, and anonymous
search-then-bind — plus group→role mapping and precedence, `default_role`,
JIT provisioning, role re-sync under `role_source: directory`, filter and DN
injection, and empty-password binds. It runs in CI on every change, on both
amd64 and arm64.

```sh
make test-ldap        # needs Docker (or colima/podman); skips cleanly without it
```

Point it at a directory you already have — including a real Active Directory —
with `SQI_TEST_LDAP_URL`, and it uses that instead of starting a container. The
fixture tree it expects is the `seedLDIF` constant in that file.

That suite exists because of a bug it now guards: OpenLDAP does not reject the
AD-only nested-group matching rule, it answers *success with zero entries*, and
an earlier revision let that empty result replace a user's real groups and
silently demote them. No fake reproduced it.

**Still test a new deployment against your own directory before relying on
it.** Directories differ in exactly the places this integration is sensitive
to: whether `memberOf` is populated, what the service account is allowed to
read, and how an unsupported matching rule is answered.

## Coming next

- C2 — OAuth2/OIDC (SSO).
- D1 — per-user concurrent task caps.
