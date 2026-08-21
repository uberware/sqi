# Authentication (Phase 3)

sqi ships with **authentication off by default** — on a trusted local network,
every request is served as an anonymous superuser and nothing is gated. This is
the pre-Phase-3 behavior and remains the default.

## The opt-in gate

The single switch is `auth.enabled` (config file `auth.enabled`, env
`SQI_AUTH_ENABLED`, flag `--auth-enabled`; default `false`).

As of component A1, the gate is live: flipping `auth.enabled` to `true` and
restarting actually locks the server down. Every REST request and the
WebSocket upgrade now require a valid credential — a session cookie or a
Bearer API key (`auth.Chain(apikey, session)`); there is no more
"scaffolding only" caveat. See [Local accounts](#local-accounts),
[Login & sessions](#login--sessions) and [API keys](#api-keys) for what that
means in practice, and [First-admin bootstrap](#first-admin-bootstrap) for
how to get your first credential.

## Broker authentication (transport)

`auth.enabled` gates the HTTP REST API and the WebSocket upgrade. It does
**nothing** to the worker transport. A second, independent switch —
`nats.auth.enabled` (config file `nats.auth.enabled`, env
`SQI_NATS_AUTH_ENABLED`; default `false`) — gates the embedded NATS broker
that workers connect to. The two flags protect different surfaces and must
each be turned on deliberately; flipping one does not flip the other.

### What it protects, and what it does not

Broker authentication addresses two things: an unauthorized host attaching to
the broker and registering as a worker, and one enrolled worker forging
traffic that claims to come from another. **It does not encrypt the
transport.** Task payloads, assignment contents and log chunks all travel
over the connection in cleartext whether broker authentication is on or off.
Turning it on stops an attacker from attaching or impersonating another
worker; it does not stop someone who can already read the network from
reading what crosses it. Do not conclude "I turned on broker authentication"
means "the channel is private" — it does not.

### The credential: an nkey per worker

Each worker holds an Ed25519 **nkey** keypair. The worker generates it
locally — only the public key ever leaves the machine — and the private seed
is written to `<data_dir>/worker.nk`, mode `0600`. Connecting to the broker
is challenge-response (the broker sends a nonce, the worker signs it with the
seed), so nothing replayable crosses the wire.

### Getting a credential

Two ways, both ultimately calling the same store write:

- **Join token (self-service).** An operator mints a TTL-bounded token —
  `sqi-server worker token issue` always works, or `POST
  /api/v1/workers/join-tokens` when `auth.enabled` is on, gated on the
  `workers.enroll` permission (admin-only by default) — and hands it to the
  worker via `nats.join_token_file` (preferred) or `nats.join_token`. On
  first boot with no credential file present, the worker generates its
  keypair and calls `POST /api/v1/workers/enroll` with the token, its worker
  ID and its public key. That endpoint is unauthenticated by design — the
  token itself is the credential, so gating it on a session or API key would
  be circular — and exists only while `nats.auth.enabled` and
  `nats.auth.enrollment_endpoint_enabled` are both true. Tokens default to a
  1-hour TTL (bounded 1 minute to 24 hours) and are single-use by default
  (`nats.auth.join_token_single_use`).
- **Manual pre-provisioning.** Run `sqi-worker keygen` on the worker host —
  it writes the seed and prints the public key and the exact `sqi-server
  worker enroll --worker-id … --public-key …` command to run on the server.
  No token, no REST call, no `POST /api/v1/workers/enroll` route needs to
  exist at all. This is the path for an air-gapped worker, or a site that
  sets `nats.auth.enrollment_endpoint_enabled: false` and wants no
  self-service enrollment surface whatsoever. `sqi-server worker enroll` is
  **offline**, exactly like `sqi-server worker revoke`: it writes the
  database from a process with no broker handle, so a **running** server
  keeps refusing the new key until it restarts.

**Enrollment always runs over REST, never over NATS.** The broker's entire
job is to refuse unauthenticated connections, so it cannot also be the
channel a worker obtains its first credential over. This means
`nats.server_url` (the server's HTTP base URL) **must be set explicitly on
the worker** whenever a join token is configured — it is **not** derived
from mDNS discovery. A worker relying on mDNS with no `server_url` set fails
enrollment fast, naming that field, rather than attempting a request with no
host.

See [`docs/worker-deployment.md`](worker-deployment.md) for both paths
walked end to end, and [`docs/worker-configuration.md`](worker-configuration.md)
for every `nats.*` field.

### Revocation: two paths, deliberately distinct

- `DELETE /api/v1/workers/{id}/credential` runs inside the server process,
  which holds the live broker handle: it writes the store and reloads the
  broker's authorized-key set in the same call, so the worker is
  disconnected before the request returns.
- `sqi-server worker revoke <id>` is an **offline** CLI command — it opens
  the SQLite file directly and writes the same row, but from a separate
  process with no broker handle. It takes effect the next time `sqi-server`
  starts, not immediately.

Use the REST path when you need a worker off the farm right now (a
compromised host, a decommissioned machine). The CLI path is for offline
maintenance — revoking a credential before a server has ever started with
it, or as part of a startup script.

`DELETE /api/v1/workers/{id}` — removing the worker record itself — revokes
its credential too, through the same synchronous path as the first bullet
above, and does so **before** deleting the worker row, not after. This
exists because `workers.manage` (what deleting a worker requires) does not
imply `workers.enroll` (what revoking a credential directly requires) — the
split is deliberate, so that the ability to delete a worker never doubles as
the ability to mint join tokens — and without the cascade, an operator who
can decommission a machine would have no way at all to cut its broker
access. A worker with no credential (broker authentication disabled, or a
worker that was never enrolled) is deleted exactly as before; a credential
that is already revoked is treated the same way.

The ordering matters: `store.DeleteWorker` never rejects with a conflict —
removability was already decided by an earlier check — so revoking first
never wastes a revocation on a delete that was always going to be refused.
If the revoke fails, nothing has happened yet: the worker row is intact, the
request answers 500, and it is safe to retry. If the delete then fails after
a successful revoke, the worker row survives but its broker access is
already cut — the safe direction to fail in — and retrying `DELETE
/workers/{id}` simply re-revokes (a no-op the second time) and tries the
delete again. Deleting first and revoking after was tried and rejected: a
failure in the revoke's own store write, not just a broker-reload failure,
would leave the worker row gone, the credential never revoked, and nothing
left to reap it — the operator would be told 204 while the machine kept live
broker access permanently, with no other permission available to fix it.

### Key rotation and re-enrollment

Worker-ID uniqueness applies only to **active** credentials: a revoked
worker ID can be enrolled again with a brand-new key (`sqi-worker keygen
--force`, then a fresh `sqi-server worker enroll` or join token). Public
keys, by contrast, are **globally unique forever** — once a key has been
enrolled, even to a since-revoked credential, it can never be enrolled
again. To rotate a worker's key:

1. `sqi-server worker revoke <worker-id>` (or the REST path, if the old
   credential should be disconnected immediately).
2. `sqi-worker keygen --force` on the worker host — this overwrites
   `worker.nk` with a new keypair and prints the new public key, plus the
   exact `sqi-server worker enroll` command for step 3.
3. `sqi-server worker enroll --worker-id <worker-id> --public-key <new-key>`
   on the server host, using the public key `keygen` just printed.
4. **Restart `sqi-server`.** Like `worker revoke`, `worker enroll` is an
   offline command: it writes the database from a process with no broker
   handle, and the broker's authorized-key set is built once at startup. A
   running server therefore still refuses the new key until it restarts.
5. Restart `sqi-worker`. It finds the new seed already on disk (`keygen`
   wrote it directly) and connects with it — no REST enrollment call happens
   on this path, since a credential file already exists.

Running `keygen --force` before revoking the old credential is safe but
will not help: the worker ID stays bound to the *old* public key on the
server until it is revoked, so the enroll command in step 3 fails until
step 1 has run. To rotate via a fresh join token instead of `keygen`,
remove `worker.nk` before restarting the worker — with no credential file
present it re-enrolls exactly as it did on first boot.

### What an enrolled worker's credential may do

A worker's broker permissions are generated from the worker ID recorded at
enrollment, and cover only that worker's own subtree:

- **Publish:** `task.status.<id>.*`, `task.logs.<id>.*`,
  `worker.register.<id>`, `worker.heartbeat.<id>`, `worker.deregister.<id>`,
  `worker.diag.<id>`, `work.lease.<id>.*`.
- **Subscribe:** `_INBOX_<id>.>` — this worker's own reply inboxes — and
  `task.cancel.>`.

Everything else is denied. A worker cannot subscribe to `task.status.>` or
`task.logs.>`, so it cannot observe another worker's status or log traffic.

The reply-inbox prefix is **per worker**, not nats.go's process-global
`_INBOX`. That matters because a work lease is core-NATS request/reply: the
assignment batch — command lines, embedded file contents, job and task
parameters, environment variables, the path map and the run-as-user account
— is delivered to the requester's reply inbox and nothing but the subscribe
permission guards it. A single `_INBOX.>` grant would let any enrolled
worker read every other worker's assignments (and `sqi-server`'s own
JetStream API replies) without leasing a task itself. Each worker therefore
connects with `nats.CustomInboxPrefix("_INBOX_<worker-id>")` and is granted
only that subtree.

### The auth-off asymmetry

With broker authentication **off — the permanent default** — the worker ID
is still present as a token in every worker-to-server subject (see
[NATS subjects and streams](architecture.md#nats-subjects-and-streams)), but
**nothing enforces it**. Any client that can reach the broker may publish
under any worker ID it chooses. The scheduler still parses that ID out of
the subject and compares it against the task attempt's recorded owner, and
still discards a mismatch — but with authentication off, that check is not a
security boundary. It catches **honest bugs**, not attackers: a stale
worker, a version mismatch, a client publishing to the wrong subject by
accident. Do not read those provenance checks as proof that cross-worker
forgery is prevented in the default configuration — it is not. Only
`nats.auth.enabled: true`, which authenticates the connection itself, closes
that gap.

`sqi-server` emits a startup `WARN` when `nats.addr` binds to a non-loopback
address and broker authentication is off, naming both remediations (turn on
`nats.auth.enabled`, or bind `nats.addr` to `127.0.0.1`).

### An accepted authorization gap: `task.cancel.>`

This is the **only** subject a worker's credential can reach outside its own
subtree. Workers subscribe to `task.cancel.<taskID>` per-task, at the moment a task is
assigned, not for the lifetime of the connection. NATS permissions are
static per credential, so a worker's subscribe permission cannot be
narrower than the whole `task.cancel.>` subtree — there is no way to grant
"only the cancel subjects for tasks I currently hold" without a permission
reload on every assignment, or a NATS auth callout. The practical effect:
**any enrolled worker can observe the cancel signal for any task**, not only
its own. This is accepted as low severity — a cancel message for a task the
worker does not hold is simply inert, it triggers no action — but it is a
real, deliberate gap rather than an oversight, and is recorded here rather
than left to be rediscovered.

## Model

Every request carries a `Principal` in its context. When auth is off, the
middleware injects an anonymous principal with the superuser flag set, so
authorization checks are bypassed. Authentication is pluggable: an
`Authenticator` resolves a request's credentials to a `Principal`. Today the
only non-anonymous `Authenticator`s are the session-cookie and API-key ones
described below.

Externally-verified credentials do **not** implement that interface.
[LDAP/AD](#ldap--active-directory) (C1) attaches at `POST /auth/login`, and
[OIDC/SSO](#oidc--sso) (C2) attaches at its own callback route; both verify the
credential once and then mint an ordinary session, so no request path binds
against an external identity provider. See [It attaches at login, not at every
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

**`auth.enabled` does not gate `/debug/pprof/`.** The Go runtime profiling
endpoints are mounted on the root router, outside `/api/v1` entirely
(`internal/api/router.go`), so the auth middleware, the CSRF guard and the
rate limiter all miss them whether auth is on or off. They are opt-in
(`http.enable_pprof`, default `false`) and the server logs a `WARN` at boot
when they are enabled. Treat that warning as literal. A pprof profile does not
dump memory contents — it will not hand out session tokens, the LDAP bind
password or the OIDC client secret — but it does expose internal code paths,
allocation sites, live goroutine stacks, build and module metadata, and the
process command line to anyone who can reach the port. Worse, because these
routes sit outside the rate limiter, `/debug/pprof/profile?seconds=N` and
`/debug/pprof/trace?seconds=N` accept an arbitrary duration from an
unauthenticated caller, which is a straightforward availability lever. Enable
pprof only on a network you would already trust with an auth-off deployment,
or put a reverse proxy in front of `/debug/`.

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

As of component B1, every mutating route and several read routes are gated by a
role→permission policy. There are four
built-in roles (no custom-role builder — YAGNI):

- **admin** — full access, including user management, API-key management for
  any account, and configuration-adjacent surfaces.
- **operator** — runs the farm: all jobs, workers, farm infrastructure
  (farms/queues/storage/compute/usage-pools), products/presets, and
  diagnostics (server log).
- **user** — submit and control **their own** jobs; manage their own API
  keys; read-only on infrastructure. This is the only role without
  `jobs.read.all`, so it neither sees nor may mutate a job owned by someone
  else — note the counter-intuitive consequence that `read-only` sees every
  job while `user` sees only its own.
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
| `isolation.manage` — set a queue's `run_as_user`/`run_as_group` | ❌ | ❌ | ❌ | ✅ |

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
| `PATCH /api/v1/auth/me` | Sets `display_name`. Returns the same principal shape as `GET /auth/me`. A body omitting `display_name` is a successful no-op returning the caller's identity unchanged, not an error. |
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
job's owner receives nothing for it rather than everything. The `diagnostics`
subject is gated separately at subscribe time on `diagnostics.read`, so a
`read-only` or `user` principal is refused the server-log feed on the socket
exactly as it is on `GET /api/v1/diagnostics/logs`.

## Task isolation

`Job identity` (above) is an HTTP-layer concept: it records who owns a job in
the database. Task isolation is the OS-layer counterpart: it controls which
**OS account** a task's process actually runs as on the worker. The two are
independent — a job's `Owner` does not have to match, and generally will not
match, the `run_as_user` its queue configures.

A queue may set `run_as_user` (and optionally `run_as_group`) via
`POST /api/v1/queues` or `PUT /api/v1/queues/{id}`. When set, the scheduler
attaches that **username only** — never a credential — to every task
assignment for that queue's tasks (`AssignMsg.Isolation` /
`protocol.IsolationSpec`, `internal/worker/protocol/protocol.go`). This is
deliberate: worker↔server transport has no authentication at all today (see
[Known gaps](#known-gaps)), so nothing secret can be allowed to travel on
that channel. The worker resolves the username to a real OS credential
locally and runs both job-code launch sites under it — an OpenJD
environment's `onEnter`/`onExit` actions and a task's own actions — with a
filtered environment and a session working directory private to that user.
This runs on both POSIX (Linux/macOS) and Windows. On Windows the worker
itself must run as a service under LocalSystem (or hold
`SeAssignPrimaryTokenPrivilege` directly) — an elevated Administrator shell
does not have that privilege by default. See
[`docs/worker-configuration.md`](worker-configuration.md) for the `isolation`
worker-config block, the per-platform requirements, and the
environment-allowlist mechanics.

**Before enabling `run_as_user` on any queue, see the worker upgrade requirement
documented in [`docs/configuration.md`](configuration.md#important-worker-upgrade-required):**
old workers silently ignore isolation, creating partial and silent enforcement
across a mixed-version farm.

### `isolation.manage` is a separate, admin-only permission

Setting a queue's `run_as_user`/`run_as_group` — including sending an explicit
`null` to clear a previously-set value — requires the `isolation.manage`
permission. **`infra.manage`, which the operator role holds and which governs
every other queue field, is deliberately not sufficient.** Choosing the OS
identity that arbitrary, worker-supplied job code executes under is an
escalation surface, not ordinary queue configuration, so it gets its own gate
held only by `admin` (see the role matrix above).

**Omitting `run_as_user`/`run_as_group` from a `PUT` body is not the same as
sending them.** A `PUT` is normally full-replace, but these two fields are a
deliberate exception: omitting the key preserves whatever is already stored
and requires no permission at all, while a body that includes the key —
even as `null` — is treated as an attempt to change it and requires
`isolation.manage`. Without this exception, an operator with only
`infra.manage` editing a queue's priority would silently strip out an admin's
`run_as_user` on every save, since the operator's client has no reason to
round-trip a field it cannot see the point of. (Server-side, this preserve
semantics is implemented as a single atomic `UPDATE` rather than a
read-modify-write, to close a lost-update race between a preserving write and
a concurrent admin write.)

### Enabling isolation raises the worker daemon's own privilege

**This is the single most important fact about this feature, and it is
counter-intuitive: turning isolation on makes the worker's own daemon process
*more* privileged, not less.** On POSIX, dropping privileges to become another
user (`setuid`/`setgid`/`setgroups`) is itself an operation that requires
starting as root — `isolation.Provider.Capable()` returns an error unless the
worker's effective uid is 0. An operator who runs `sqi-worker` unprivileged
today and enables isolation expecting a pure reduction in the blast radius of
job code has it backwards: the trade is that the **daemon** gains root so that
individual **tasks** can lose it. Weigh that trade for your environment before
enabling `isolation.required` or configuring any queue's `run_as_user` — it is
not a strict security improvement over an unprivileged, unisolated worker; it
is a different risk shape (one root daemon, many unprivileged task processes)
traded for another (one unprivileged daemon, all tasks running as that same
account).

### Privileged accounts and groups are refused outright

Independent of, and in addition to, the supplementary-group stripping below,
`isolation.Provider.Resolve` refuses several requests outright rather than
silently narrowing them:

- **`run_as_user` naming a known-privileged account** — `root`,
  `Administrator`, `Administrators`, `SYSTEM`, `LocalSystem`,
  `NETWORK SERVICE`, and `LOCAL SERVICE` — by name, and any account whose
  **uid is 0**, regardless of name. Name matching is case-insensitive and
  first strips a `DOMAIN\` / `HOST\` / `.\` qualifier and a trailing
  `@domain` UPN suffix, so `.\Administrator`, `CORP\Administrator` and
  `Administrator@corp.example.com` are all the same refused name. The list
  is not configurable: an operator-overridable version would defeat the
  point, since the whole risk is a queue pointing at an account more
  privileged than the daemon.
- **`run_as_group` naming a known-privileged group** — `root`, `wheel`,
  `admin`, `sudo`, `sudoers`, `adm`, `docker`, `disk`, `shadow`, `staff`,
  `administrators` — by name. This is a check against the group **you
  explicitly asked for**, not the target account's ambient memberships (see
  the next section) — a queue cannot select `docker` or `wheel` as the
  isolation group even if the target account happens to belong to it.
- **A `run_as_user` account whose *primary* group is gid 0** — refused even
  when `run_as_group` is not set at all, since the primary group is
  determined by the account, not by queue config, and gid 0 is refused
  unconditionally regardless of which name a platform gives it (`root` on
  Linux, `wheel` on macOS/BSD).

This is why `wheel` never appears as a group job code can reach on macOS/BSD:
it **is** gid 0 there, so it is refused both as an explicit `run_as_group`
target and, via the gid-0 check just above, as anyone's primary group.

### A target account's existing group memberships reach job code

Isolation strips gid 0 (`root`) from a target account's supplementary group
list unconditionally, wherever it appears. It does **not** strip any other,
named group the account already belongs to — `docker`, `disk`, `shadow`, or
any in-house privileged group. This is by design, not an
oversight: those memberships are the account's own, pre-existing access, and
supplementary groups on a render-farm account typically exist specifically to
grant project-storage access (an NFS-exported group, for example). Silently
stripping every named group to be "safe" would silently break exactly the
access those groups exist to provide, on every job, with no way to tell
whether a given group was load-bearing.

The consequence is the operator's to manage, not sqi's: **do not point a
queue's `run_as_user` at an OS account that belongs to `docker`** — group
membership in `docker` is a well-known one-step escalation to root (a
container can bind-mount the host root and chroot into it), so isolating into
that account grants job code exactly the privilege isolation exists to deny.
The same caution applies to `disk` and `shadow`. Audit the target account's
group memberships (`id <user>`) before assigning it to a queue, the same way
you would audit any account you grant OS-level access to.

### Environment allowlist and `env_passthrough`

An isolated task's environment is filtered when its session is created:
starting from a minimal base (`PATH`, `HOME`/`USERPROFILE`, `TMPDIR`, and a
handful of others rewritten to the target user rather than the daemon's own),
only daemon-environment variables whose **name** matches a
`isolation.env_passthrough` glob are added. This governs **only** what the
isolated task inherits from the worker daemon's own environment — variables a
job supplies itself (an OpenJD `Environment.variables` block, an
`openjd_env` export, a task template variable) always pass through untouched,
regardless of `env_passthrough`, because they are the job's own data, not
daemon leakage.

A render farm's licensing model almost always needs some of this escape
hatch — worked example:

```yaml
isolation:
  env_passthrough:
    - "foundry_LICENSE"
    - "ARNOLD_LICENSE"
    - "solidangle_LICENSE"
```

**A broad glob defeats the allowlist entirely.** `env_passthrough: ["*"]` or
even something that looks narrower, like `"*KEY*"`, re-opens exactly the
daemon-environment leak the filter exists to close — every variable the
daemon process happens to have (credentials, tokens, internal service
addresses) now reaches job code again. Write globs as specific as the actual
variable names in use; a missing license variable in a task's log is the
expected, and correct, symptom of an allowlist that is too tight — widen it
by name, not by wildcard. See
[`docs/worker-configuration.md`](worker-configuration.md) for the full
`isolation` config reference.

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

**Auth-off behavior.** With `auth.enabled=false`, every request is the
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

Each account carries `users.auth_source` — `local`, `ldap`, or (from C2)
`oidc`. It is set when the account is created and is **immutable** — no route
can change it. `POST /auth/login` reads it and consults exactly one backend:
the stored argon2id hash, or the directory. Never both, never in sequence. An
`oidc` account has no password path at all; it signs in through
[the SSO routes](#oidc--sso).

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
subsequent login. The two spellings deliberately do not create two records —
and since accounts match on the identifier rather than the name, that holds
across a directory rename too.

**`username_attr` should still name a directory-controlled, unique attribute.**
It is no longer what sqi matches an account on — `unique_id_attr` is — but it
is the value written to `users.username`, which is unique in sqi's own store,
and it is what binds to `Job.Owner`/`Job.Submitter`. Point it at something
*users can edit themselves* — `mail` is the obvious trap — and one user can
claim the name another account already holds, which **blocks that account's
next login** on a username collision until an operator renames one of the two
rows. That is a denial of service, and a confusing one to diagnose; it is not a
privilege escalation, because a collision is refused rather than adopted and no
account is ever inherited. `sAMAccountName` and `uid` are the right kind of
attribute; a self-service directory field is not.

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

Both password routes key on "not a local account", so an
[SSO account](#oidc--sso) behaves identically. That single rule is also what
the users API reports as `password_editable` — the companion to
`role_editable` — so a client can hide its set-password control instead of
letting someone type a password and discover the 409. Field and guard are
computed from one server-side predicate and cannot drift apart.

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

## OIDC / SSO

As of component C2, `sqi-server` can sign users into the **web UI** through an
OAuth2/OpenID Connect identity provider — Keycloak, Microsoft Entra ID, Okta,
or anything else that publishes a discovery document. Enable it with
`auth.oidc.enabled` on top of `auth.enabled`; the block is inert unless both
are true. Every field is catalogued in
[`docs/configuration.md`](configuration.md#authoidc).

**Scope: this is a browser login mechanism, and nothing else.** The callback
mints the same server-side session cookie a password login mints, and
`auth.Chain(apikey, session)` is untouched. sqi never accepts a provider's
token as a per-request API credential, and there is no device-authorization
grant. Headless clients — the SDK, submitters, CI — keep using
[API keys](#api-keys); SSO does not reach them.

### The flow

Three routes, all public by necessity:

| Route | Purpose |
|---|---|
| `GET /api/v1/auth/providers` | What the login page renders: whether password login is offered, and the SSO button's label and login URL. Exposes neither the issuer nor the client secret. |
| `GET /api/v1/auth/oidc/login` | Starts the authorization-code flow (PKCE) and redirects to the provider. |
| `GET /api/v1/auth/oidc/callback` | Redeems the code, validates the ID token, resolves the account, mints the session, and redirects to `/`. |

They cannot sit behind the auth middleware — their whole purpose is to produce
the principal that middleware would demand. What stands in for it:

- **The flow state lives in an HMAC-signed, `HttpOnly` cookie**, carrying the
  `state` value, the nonce, and the PKCE verifier. There is no server-side
  table and no cleanup job. The signing key is generated **per boot and never
  persisted**, so a server restart invalidates logins that are in flight —
  accepted, because the alternative is a persisted key.
- **The state cookie is cleared unconditionally, before any early return**, so
  a replayed callback finds nothing waiting for it. The signed payload also
  carries its own issued-at, checked server-side, so an expired state is
  refused whether or not the browser honored the cookie's expiry.
- **The post-login destination is a constant.** No "return to" parameter is
  honored — an attacker-chosen destination on a page that has just minted a
  session is how open redirects become account takeovers.
- **Every failure redirects to `/?sso_error=1`** and says nothing else. The
  reason goes to the server log only; a reason on the wire would turn the
  callback into an enumeration oracle. If SSO is failing, the server log is
  where the answer is — `internal/auth/oidc` itself logs nothing on token
  rejection, so the route logs every refusal.

Discovery is **lazy**: the issuer's document is fetched on first use, not at
boot, and is retried after a failure. A briefly unreachable provider must not
stop a render farm's scheduler from starting. Configuration faults still abort
boot — a missing `client_id` fails validation, an unreachable issuer does not.

### A worked setup (Keycloak)

Keycloak is the provider sqi's integration test actually drives, so it is the
one worked here. Register a confidential client in your realm with the standard
flow enabled, a redirect URI of `https://sqi.example.com/api/v1/auth/oidc/callback`,
and — this part is easy to miss — **a protocol mapper that puts group
membership into the `groups` claim**. Keycloak emits no groups at all without
one, and a token with no groups still validates, so every user silently lands on
`default_role`.

```yaml
auth:
  enabled: true
  oidc:
    enabled: true
    issuer: "https://keycloak.example.com/realms/farm"
    client_id: "sqi"
    # client_secret via SQI_AUTH_OIDC_CLIENT_SECRET
    redirect_url: "https://sqi.example.com/api/v1/auth/oidc/callback"
    scopes: ["openid", "profile", "email"]
    username_claim: "preferred_username"
    display_name_claim: "name"
    groups_claim: "groups"
    role_source: "directory"
    role_map:
      - group: "/farm-admins"
        role: admin
      - group: "/farm-operators"
        role: operator
    default_role: "read-only"
    reauth_mode: "after_logout"
    logout_mode: "local"
```

`redirect_url` must be the absolute URL of *this server's* callback route, and
must be registered at the provider byte-for-byte. `client_secret` is redacted
in `sqi-server config print`; prefer `SQI_AUTH_OIDC_CLIENT_SECRET` over writing
it to disk.

### Claim → role mapping

`role_map` is an **ordered** list of group → role rules read from the claim
named by `groups_claim`, and **the first match wins** — order is how you
express precedence, so put `admin` above `operator`. This is the same mapper
LDAP uses (`internal/auth/rolemap`), so the two cannot drift apart on
precedence. A role naming anything other than `admin`, `operator`, `user`, or
`read-only` fails config validation at boot.

`default_role` applies when no rule matches; setting it **empty rejects the
login entirely**, which is how a deployment requires group membership to sign
in at all. The default is `read-only`.

`"groups"` is **not** a standard OIDC claim. Whether membership needs a scope,
a provider-side mapper, or both varies by provider, and there is no error when
it is absent — the token validates and the user lands on `default_role`. If
every SSO user is arriving with your `default_role`, check the claim before
anything else. (This is the OIDC counterpart of LDAP's
[`memberOf` trap](#your-directory-must-populate-memberof), and it fails the
same silent way.)

### Just-in-time provisioning and identity

An unknown SSO identity is a provisioning event: sqi maps a role from the
claims and creates a local record with `auth_source: oidc`, its stored
`password_hash` an unusable placeholder. There is no import step.

**Accounts are matched on the `sub` claim, never on the username** — the same
rule and the same code path as
[LDAP's stable identifier](#accounts-are-matched-on-a-stable-identifier-not-a-username).
`sub` is the provider's stable, non-reusable subject identifier; it is stored
in `users.external_id`. A username or email changed at the provider therefore
reaches the same sqi account, and a recycled email address does *not* inherit
the account it used to belong to. An identity whose `sub` is empty is refused
outright, with an `ERROR` in the log, rather than falling back to name
matching — the fallback is the hazard.

Display name is read from `display_name_claim` **once, at creation, and never
re-synced**, so a self-service `PATCH /api/v1/auth/me` edit persists. The trade
is that a name changed at the provider does not propagate.

### `role_source` — who owns an SSO user's role

| Value | Role on login | `PATCH /users/{id}` role edit |
|---|---|---|
| `directory` (default) | Recomputed from claims on **every** login | **409 Conflict** |
| `local` | Seeded from claims at provisioning only | Allowed |

One value drives both halves, for the reason given under
[LDAP's `role_source`](#role_source--who-owns-a-users-role): splitting them
means an admin edits a role, gets a 200, and the next login silently reverts
it.

`auth.oidc.role_source` and `auth.ldap.role_source` are tracked **separately**
— an operator may trust one provider's groups and not the other's — and the
`role_editable` field the users API returns is computed per account from
whichever source owns it.

**Keep a local admin account.** Every word of
[Keep a local admin account](#keep-a-local-admin-account) applies here: rename
the admin group at the provider and, in `directory` mode, every SSO admin is
demoted to `default_role` at their next login, with role edits on those
accounts returning 409 and nobody left holding `users.manage`. A local account
is the only thing unaffected by a provider-side change.

### Re-authentication and logout are two different things

They answer different problems and are configured independently. Conflating
them is the usual mistake.

**`reauth_mode`** decides whether the *next* SSO login is allowed to be silent
— it sends `prompt=login` to the provider.

| Value | Behavior |
|---|---|
| `after_logout` (default) | Re-prompt only on the login that follows an explicit logout. |
| `always` | Re-prompt on every login. |
| `never` | Never re-prompt; silent re-login is always permitted. |

This — not `logout_mode` — is the answer to the shared-workstation problem.
Without it, clearing sqi's session while the provider still considers the
person signed in is precisely how the next person at that machine gets signed
in as the last one.

**`logout_mode`** decides whether logging out of sqi also ends the session *at
the provider*.

| Value | Behavior |
|---|---|
| `local` (default) | Clear the sqi session only. |
| `provider` | Also return the provider's RP-initiated logout URL, which the web navigates to. The sqi session ends immediately either way; the provider leg is where the browser goes next. Requires `post_logout_redirect_url`. |

`local` is the default because a provider logout signs the user out of every
company tool that trusts that provider — heavy-handed for someone mid-task
elsewhere.

Under `provider`, `POST /api/v1/auth/logout` returns
`{"redirect_url": "…"}`; every other logout returns `{}`. If the provider
advertises no `end_session_endpoint`, or discovery is unreachable, or the
advertised endpoint does not parse, sqi **degrades to a local logout and logs
at `ERROR`** — never silently.

**sqi does not store ID tokens**, so the end-session request uses `client_id` +
`post_logout_redirect_uri` and never `id_token_hint`. That is deliberate, and
measurement strengthened the case rather than weakening it:

- The token would be the **first recoverable secret in the schema**. Session
  tokens are stored as hashes, passwords as argon2id hashes. "Nothing in this
  database can be read back out and used" is an invariant worth more than a
  logout convenience.
- **Keycloak accepts an expired ID token as a hint** — verified well past
  `exp`, and the session still died silently. A stored token is therefore a
  session-termination capability that **does not decay**; "it expires in
  minutes" is not a mitigation.
- **The hint works from a client holding no provider cookies.** A leaked token
  can end that user's provider session from anywhere, by anyone holding it.
- **The realistic leak path is accident, not theft** — a debug log line
  dumping a session row, or a future session-listing endpoint. Avoiding it
  would require a redaction rule every future contributor remembers, which is
  where this class of leak actually originates.

The cost is stated under
[Provider logout is weaker than it looks](#provider-logout-is-weaker-than-it-looks).

### Limits, stated plainly

#### Revocation lag is the same as LDAP's

For the same reason, too: login is the only moment sqi talks to the provider —
see [Revocation lag](#revocation-lag). **A user disabled or deleted at the
provider keeps their sqi session until it expires** — `auth.session.ttl`, 7
days by default. They cannot get a *new* session; the one they hold keeps
working.

Shorten `auth.session.ttl` if that window matters. To cut someone off
immediately, disable the account in sqi (`PATCH /api/v1/users/{id}` with
`disabled: true`): the session authenticator re-checks the user record on every
request, and the local `disabled` flag overrides the provider for both session
checks and login.

#### Role changes apply at next login only

Under `role_source: directory` the role is recomputed from claims **at login**.
A user moved out of the admin group at the provider keeps `admin` in sqi for
the remaining life of their session. Disabling the account in sqi is again the
immediate lever.

#### Provider logout is weaker than it looks

`logout_mode: provider` **on Keycloak is a confirmation prompt, not a silent
provider logout.** Measured against Keycloak 26.0.7 by `make test-oidc`:

- The end-session request carrying `client_id` and `post_logout_redirect_uri`
  with no `id_token_hint` **is accepted** — HTTP 200, both parameters honored,
  no error.
- **But the logout does not complete.** Keycloak answers with an interactive
  logout-confirmation page, and **the provider session stays live behind it**
  (verified independently with a `prompt=none` probe that still returned a
  code). The session ends only once a human posts that confirmation.

This is not a security hole — sqi's own session is genuinely revoked either
way, and it is revoked before the redirect is ever handed to the browser — but
it is weaker than an operator would infer from the option's name. In practice
`logout_mode: provider` ends the sqi session immediately and then takes the
user to the provider to confirm signing out everywhere. If someone abandons the
browser at that page, they are out of sqi and still signed in to every other
tool.

**The confirmation page is a security control, not a Keycloak quirk.** An
end-session request carrying only a client identifier could have been
constructed by anyone, so Keycloak asks the human before acting on it.
`id_token_hint` is the proof that the caller was party to the session, and
supplying it is exactly what buys the skip: with the hint, Keycloak answers
`302` straight to the post-logout URI and the session is dead with no
interaction at all (`client_id` is not even required alongside it). sqi does
not hold that proof, by choice — see the reasons under
[Re-authentication and logout are two different things](#re-authentication-and-logout-are-two-different-things).
The confirmation click is the price of not keeping a non-decaying,
exfiltratable logout capability at rest. It is a deliberate trade, not an
oversight.

**Entra ID and Okta have not been measured.** Whether either completes an
end-session request without an `id_token_hint` is **unverified here**; Okta has
historically been reported to require the hint, but that is recollection, not
an observation, and it is not a claim this document is willing to make. Test
your own provider before relying on `logout_mode: provider`, and treat
`reauth_mode` as the control that actually protects a shared workstation.

#### `reauth_mode: after_logout` is defeatable

The "this browser logged out" marker is an ordinary cookie. Clearing browser
storage removes it, and the next login is silent again. It is adequate against
the next colleague to sit down at a shared workstation; it is useless against
someone with developer tools open. **`reauth_mode: always` is the hard
guarantee** — it re-prompts every login regardless of what the browser is
carrying, at the cost of SSO's silent-login convenience.

#### A username collision blocks the second user's login

Two identities that map to the same `username_claim` value — or an SSO user
whose name is already taken by a local or LDAP account — cannot both exist.
The second one to log in is **refused with the usual 401**, and a `WARN` names
the collision in the server log. An admin renames one of the accounts to
resolve it.

Refusal is the intended outcome, not a gap. Auto-disambiguating (`alice2`)
would create accounts whose names no longer match the provider — and since the
username is what binds to `Job.Owner`, that is a mismatch that quietly spreads
into job ownership. Adopting the existing account is worse still: it is
username matching, the thing identifier matching exists to remove. An error an
operator can act on beats both.

#### Only the open-source providers are covered by CI

`make test-oidc` runs the whole SSO path against a **real Keycloak** in a
throwaway container, and `make test-ldap` runs the LDAP path against a **real
OpenLDAP**. Both run in CI on every change. That is the honest extent of it:

- **Active Directory's `objectGUID` path is not proven by the OpenLDAP
  container.** OpenLDAP is exercised with `entryUUID`; AD's binary `objectGUID`
  and its hex encoding are covered by unit tests against a fake, not by a live
  domain controller.
- **Okta and Entra ID are not proven by the Keycloak container.** Nothing in CI
  contacts either. Claim shapes, group-claim delivery, and end-session behavior
  all differ between providers, and those are exactly the places this
  integration is sensitive.

Both are hosted or licensed products that cannot be provisioned in CI. **Test a
new deployment against your own provider before relying on it.**

### How this is tested

The unit tests drive a fake identity provider. It signs real tokens, so a
validation mistake surfaces — but a fake returns whatever the test asks for, so
it cannot show what a *real* provider omits. That is the gap
`test/integration/oidc_test.go` closes, driving a real browser-shaped flow
against a real Keycloak: the authorization-code round trip and group → role
mapping (including the silent `default_role` downgrade when the groups mapper
is missing), a rename at the provider keeping the same account, state-mismatch
refusal, `prompt=login` forcing re-authentication, and the end-session behavior
recorded above.

```sh
make test-oidc        # needs Docker (or colima/podman); skips cleanly without it
```

See [Testing against a real directory or identity
provider](development.md#testing-against-a-real-directory-or-identity-provider) for the
`SQI_TEST_OIDC_ISSUER` escape hatch and why a skip verifies nothing.

## Known gaps

- **Broker authentication is off by default, and that default is permanent —
  not a placeholder for a later phase.** See
  [Broker authentication (transport)](#broker-authentication-transport)
  above for the full model: the credential, both enrollment paths,
  revocation, key rotation, and — stated plainly there — the asymmetry that
  while `nats.auth.enabled` is off, the worker ID carried in every subject is
  unenforced, so the scheduler's provenance checks catch bugs rather than
  attackers, and any host that can reach the embedded NATS broker's port
  (`4222` by default) can register as a worker and receive task assignments.
  Turning broker authentication on does not encrypt the transport either —
  task payloads, assignments and log chunks stay cleartext regardless.
  `sqi-server` emits a startup WARN when the broker address is non-loopback
  and broker auth is off, precisely because an operator reading "I flipped
  `auth.enabled` to `true`, so the server is now locked down" should not
  assume that also locked down the worker transport — the two flags are
  independent and both must be set.
- **Task isolation is implemented and integration-tested on both platforms.**
  See [Task isolation](#task-isolation) above for the current state: a queue's
  `run_as_user` runs job code as a distinct OS user on Linux/macOS workers, and
  as a distinct logon session on Windows workers via `LogonUserW`. Both suites
  run against real OS accounts — `make test-isolation` as root in a container,
  `make test-isolation-windows` against real local accounts with its privileged
  tier as SYSTEM — and both pass. The POSIX NSS (LDAP/AD-backed account)
  fallback path, however, has only ever been exercised against canned command
  output, never a real directory server — see
  [`docs/worker-configuration.md`](worker-configuration.md) for that caveat in
  full.
- **Windows staging has no TOCTOU re-check on stage-out, and isolation is what
  makes it reachable.** On POSIX, `internal/worker/staging`'s `builtinCopy`
  re-checks for a symlink swap or a hardlink-count change (`O_NOFOLLOW`,
  hardlink-count) between its own `Lstat` and the elevated daemon's subsequent
  read of the same path. Windows has no equivalent re-check yet. Because a
  session directory is now genuinely ACL-secured to the target account, a task
  running under that account has write access to its own session directory
  and can in principle race a symlink/junction swap into the window between
  `builtinCopy`'s `Lstat` and the daemon's later read during stage-out. An
  unisolated worker has nothing to gain from winning that race — its tasks
  already run as the daemon's own account — so isolation being enabled on
  Windows is precisely what makes this reachable. Not yet fixed; tracked for
  a follow-up before this is considered hardened.
- **Per-user concurrent task caps.** A hard per-owner ceiling on running tasks was scoped for
  Phase 3 and deferred (2026-07-20) with no driver behind it. Nothing bounds a single user's
  farm consumption today: `max_concurrent_tasks` on farms and queues caps the container, not the
  person, so one user can still fill a queue's whole allowance. If that becomes a real problem,
  it is worth deciding between a hard cap and fair-share scheduling before building either.
