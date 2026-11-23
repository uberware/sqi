# Authentication

Authentication is off by default: every request is served as an anonymous
superuser and nothing is gated, which suits a trusted local network.

## The opt-in gate

The single switch is `auth.enabled` (config file `auth.enabled`, env
`SQI_AUTH_ENABLED`, flag `--auth-enabled`; default `false`).

Setting `auth.enabled` to `true` and restarting locks the server down. Every
REST request and the WebSocket upgrade require a valid credential: a session
cookie or a Bearer API key (`auth.Chain(apikey, session)`). See
[Local accounts](#local-accounts), [Login & sessions](#login--sessions) and
[API keys](#api-keys) for what that means in practice, and
[First-admin bootstrap](#first-admin-bootstrap) for how to get your first
credential.

## Broker authentication (transport)

`auth.enabled` gates the HTTP REST API and the WebSocket upgrade. It does
nothing to the worker transport. A second, independent switch,
`nats.auth.enabled` (config file `nats.auth.enabled`, env
`SQI_NATS_AUTH_ENABLED`; default `false`), gates the NATS broker that
workers connect to, whether it is the broker embedded in `sqi-server` or an
external cluster (`nats.mode: external`). The two flags protect different
surfaces and are turned on separately; flipping one does not flip the other.

### What it protects, and what it does not

Broker authentication addresses two things: an unauthorized host attaching to
the broker and registering as a worker, and one enrolled worker forging
traffic that claims to come from another. It does not encrypt the transport:
task payloads, assignment contents and log chunks travel over the connection
in cleartext whether broker authentication is on or off, so anyone who can
read the network can read what crosses it.

Encrypting that transport is a separate, independent setting: TLS on the
broker (`nats.embedded.tls`, or an external cluster's own TLS). See
[`docs/tls.md`](tls.md). A production farm needs both: authentication decides
who may connect, and TLS keeps what crosses the wire private.

### The credential: an nkey per worker

Each worker holds an Ed25519 nkey keypair. The worker generates it locally
(only the public key leaves the machine), and the private seed
is written to `nats.credential_file`, mode `0600`, which defaults to
`<data_dir>/worker.nk` but can be pointed anywhere. Both self-service
enrollment over REST and `sqi-worker keygen` honor a custom path: `keygen`
loads the worker's own configuration (the root `-c/--config` file,
`SQI_WORKER_*` environment variables, and built-in defaults, the same as
`sqi-worker start`) and writes the seed wherever `credential_file` resolves.
See
[`docs/worker-configuration.md`](worker-configuration.md#natscredential_file)
for the full field reference. Connecting to the broker is challenge-response
(the broker sends a nonce, the worker signs it with the seed), so nothing
replayable crosses the wire. The worker holds the same kind of credential
whichever broker mode the server runs in, and cannot tell the two apart.

### How a connection is authorized

With `nats.auth.enabled` on, the broker hands every connection other than
`sqi-server`'s own to `sqi-server` through the NATS auth callout. The same
code answers in both broker modes:

1. `sqi-server` checks that the worker presented a valid user public key and
   that its signature over the broker's nonce verifies.
2. It looks the key up among the active credentials in its database, at
   that moment, and re-checks the worker ID recorded with it.
3. It answers with a short-lived authorization (a signed NATS user JWT) that
   places the worker in sqi's account with that worker's own permissions (see
   [What an enrolled worker's credential may do](#what-an-enrolled-workers-credential-may-do))
   and names the connection after the worker ID.

Anything else refuses the connection: an unknown or revoked key, a bad
signature, a database error, or `sqi-server` not answering at all. The
callout never admits a worker on an error.

Because the database is read at every connect, enrollment takes effect at the
worker's next connect on a running server, by either path below, and needs no
server restart.

The authorization lasts `nats.auth.user_ttl` (default 1 hour, between 1
minute and 24 hours). At expiry the broker disconnects the worker, which
reconnects at once and is authorized afresh, so a healthy worker sees one
brief reconnect per `user_ttl` and a revoked one is refused.

A refused worker retries for 2 minutes before it exits. The broker shows
the worker the same "Authorization Violation" whether its key was revoked or
`sqi-server` could not answer (a database restart, or in external mode a
`sqi-server` restart), so a worker holding a credential keeps retrying,
logging a WARN at each refusal, and exits only once the refusals have lasted
2 minutes. When the worker is starting, a refusal can also arrive as a
connection reset (the broker closes the connection before the worker has read
the refusal, which Windows reports as a reset); the worker retries those for
the same 2 minutes and logs a WARN for each. A worker with no credential gets
no grace: on a broker that requires one it exits at the first refusal.

Embedded mode needs nothing beyond `nats.auth.enabled: true`: the broker
inside `sqi-server` is set up with the callout, and the keys it signs with are
generated in memory at every start. External mode also needs the cluster
configured for it and the `nats.external.callout` keys on `sqi-server`;
`sqi-server nats external init` generates both (see
[Running on an external NATS cluster](operations.md#running-on-an-external-nats-cluster)).
An external cluster issues the nonce workers sign only when it has nkey users
of its own, which the generated configuration provides. A cluster without
them sends no nonce, and every worker presenting a key then fails on its own
side with `nats: nkeys not supported by the server` before `sqi-server` is
asked anything: the cluster's configuration lacks the generated accounts
fragment.

### Getting a credential

Two ways, both ending in the same store write:

- **Join token (self-service).** An operator mints a TTL-bounded token
  (`sqi-server worker token issue`, see the database-path resolution rule
  below, or `POST /api/v1/workers/join-tokens` when `auth.enabled` is on,
  gated on the `workers.enroll` permission, admin-only by default) and hands it to
  the worker via `nats.join_token_file` (preferred) or `nats.join_token`. On
  first boot with no credential file present, the worker generates its
  keypair and calls `POST /api/v1/workers/enroll` with the token, its worker
  ID and its public key. That endpoint takes no session or API key, since the
  token is the credential, and exists only while `nats.auth.enabled` and
  `nats.auth.enrollment_endpoint_enabled` are both true. Tokens default to a
  1-hour TTL (bounded 1 minute to 24 hours) and are single-use by default
  (`nats.auth.join_token_single_use`).
- **Manual pre-provisioning.** Run `sqi-worker keygen` on the worker host,
  with its normal config in place: it writes the seed and prints the
  public key and the exact `sqi-server worker enroll --worker-id … --public-key
  …` command to run on the server. Pass `--data-dir` to override
  `worker.data_dir` explicitly for a one-off run against a different
  directory. No token, REST call or `POST /api/v1/workers/enroll` route is
  needed. This is the path for an air-gapped worker, or a site that sets
  `nats.auth.enrollment_endpoint_enabled: false` and wants no self-service
  enrollment surface. `sqi-server worker enroll`
  writes the database directly, from a process with no broker handle, and
  needs no server restart: a running server admits the key at the worker's
  next connect.

> `sqi-server worker …` resolves its database path the same way `backup`
> and `migrate` do. Highest priority first: an explicit `--db`;
> `store.sqlite_path` (the root `-c/--config` file and
> `SQI_STORE_SQLITE_PATH`); the `SQI_SQLITE_PATH` environment variable; then
> `sqi.db`. Unlike `migrate`, these subcommands never create or migrate the
> database: a path with no database there is an error naming the resolved
> path.
> See [`docs/operations.md`](operations.md#worker-broker-credentials) for
> the full command reference.

Turning `nats.auth.enabled` on is a staged change. It takes a server
restart, and from then on every connection is checked: a worker with no
credential exits at its first refusal, and one whose key is not enrolled
exits after the 2-minute grace. The reverse also holds: a broker with
authentication off accepts anonymous connections only, and refuses a
connection that presents a key (the worker's error says so and tells the
operator to remove its credential file). A worker presents a key whenever its
`nats.credential_file` exists, so a worker must not be restarted onto a new
seed while the farm is still auth-off. The safe order:

1. **Enroll every worker, without restarting it.** With authentication off
   the enrollment endpoint is not mounted, so join tokens cannot be redeemed
   at this point; use the manual path: `sqi-worker keygen` on each worker host (it
   writes the seed and prints the enroll command; the running worker keeps
   its anonymous connection, because it loaded its credential at start), then
   `sqi-server worker enroll` on the server. Confirm each one with
   `sqi-server worker list`. A worker that will enroll itself with a join
   token instead gets its token file at this step (`sqi-server worker token issue`
   works with authentication off) and enrolls at its first start after
   step 2.
2. **Set `nats.auth.enabled: true` and restart the server.** On the embedded
   broker, running workers lose their anonymous connections at the restart,
   their reconnects without a key are refused, and each one exits. On an
   external cluster this step also switches the cluster to the auth-callout
   configuration `sqi-server nats external init` generates.
3. **Restart every worker.** Each presents its seed, or redeems its join
   token first, and is admitted.

Enrollment runs over REST, never over NATS: the broker refuses
unauthenticated connections, so it cannot be the channel a worker obtains its
first credential over. `nats.server_url` (the server's HTTP base URL) must
therefore be set on the worker whenever a join token is configured; it is not
derived from mDNS discovery. A worker with a join token and no `server_url`
fails enrollment at once, naming that field.

See [`docs/worker-deployment.md`](worker-deployment.md) for both paths
walked end to end, and [`docs/worker-configuration.md`](worker-configuration.md)
for every `nats.*` field.

### Revocation: two paths, deliberately distinct

Revoking a credential marks it revoked in the database. From that moment the
callout refuses the key at every new connect. What differs is how soon a
connection the worker already holds is closed:

- `DELETE /api/v1/workers/{id}/credential` runs inside the server process,
  which holds the broker handle: it writes the store, then disconnects the
  worker's live connections, and the worker's reconnect is refused.
  - **Embedded broker: immediate, always.** The worker is disconnected
    before the request returns.
  - **External broker with
    [`nats.external.callout.system_creds_file`](configuration.md#natsexternalcalloutsystem_creds_file):
    immediate.** `sqi-server` asks every node of the cluster for the
    worker's connections and has each one closed, before the request
    returns. If a node does not answer, the request fails with a 500, since
    the worker may still be connected there; retry it.
  - **External broker without a system credential: bounded.** `sqi-server`
    cannot reach another client's connection on the cluster, so the request
    returns once the store is written, and the worker keeps its current
    connection until its authorization expires, within `nats.auth.user_ttl`
    (1 hour by default). `sqi-server` logs a WARN at startup naming that
    bound.

  The immediate cases take a few seconds: after closing what it finds, the
  revocation waits out the broker's auth timeout (2 s by default, 3 s with
  TLS, on the embedded broker) and closes anything an authorization already
  in flight placed in the meantime.
- `sqi-server worker revoke <id>` is an offline CLI command: it opens
  the database directly and writes the same row, but from a separate
  process with no broker handle. It closes nothing: it takes effect at the
  worker's next connect, which is within `nats.auth.user_ttl` at the latest.

With broker authentication on, a REST revocation also stops the scheduler
leasing the worker new work. That includes a lease request the worker sent
before the revoke and that was still waiting for work, and, in the bounded
case above, every request from the connection it keeps until its
authorization expires; only a request already being leased when the revoke
lands can still complete. In the immediate cases the worker's in-flight tasks
return to the queue once its heartbeats stop and the heartbeat sweep takes it
offline; in the bounded case the worker stays connected, so it finishes and
reports the tasks it holds. A worker that enrolls again and registers with
its new credential is leased work as before. The server logs the revocation,
and its end, at Info. The record of a REST revocation lives in the server
process: after a restart or a failover to another server, a worker still
holding a connection in the bounded case can be leased work again until its
authorization expires. The CLI path above records nothing in a running
server.

Use the REST path when you need a worker off the farm at once (a
compromised host, a decommissioned machine). The CLI path is for offline
maintenance: revoking a credential while no server runs, or as part of a
script.

A revoked worker's reconnect is refused, and the worker exits after the
2-minute grace described under
[How a connection is authorized](#how-a-connection-is-authorized).

`DELETE /api/v1/workers/{id}`, which removes the worker record, revokes its
credential too, through the same synchronous path as the first bullet above,
before deleting the worker row. `workers.manage` (what deleting a worker
requires) does not imply `workers.enroll` (what revoking a credential directly
requires), so that deleting a worker never confers the ability to mint join
tokens; the cascade is what lets an operator who can decommission a machine
also cut its broker access. A worker with no credential (broker authentication disabled, or a
worker that was never enrolled) is deleted with nothing to revoke; a credential
that is already revoked is treated the same way.

Removability is decided by a check before the revoke, which looks only at the
worker's status (offline), so in the common case a revoked worker is then
deleted. The delete itself is guarded: inside its own write it applies the
same rule and also refuses a worker that still holds an assigned or running
task. The delete can therefore be refused after the revoke, with a 409
Conflict, in two cases: a worker that comes back between the revoke and the
delete, and an offline worker that was leased a task after going offline
(going offline reclaims a worker's tasks). Both are listed in the known gaps
in [architecture.md](architecture.md#known-gaps). In both, access is cut, the
worker row is kept, and the worker can be enrolled again under a new key (see
below). If the revoke fails, nothing has changed: the worker row is intact,
the request answers 500, and it is safe to retry. If the delete fails after a
successful revoke, the worker row survives with its broker access cut, and
retrying `DELETE /workers/{id}` re-revokes (a no-op the second time) and tries
the delete again. Revoking first means no failure can leave the worker row
deleted while its credential stays active.

### Key rotation and re-enrollment

Worker-ID uniqueness applies only to active credentials: a revoked worker ID
can be enrolled again with a new key (`sqi-worker keygen --force`, then a
fresh `sqi-server worker enroll` or join token). Public keys are unique across
all credentials, revoked ones included: a key that has been enrolled once can
never be enrolled again. To rotate a worker's key:

1. `sqi-server worker revoke <worker-id>` (or the REST path, if the old
   credential's connection should be closed immediately).
2. `sqi-worker keygen --force` on the worker host, with its normal config in
   place. This overwrites the seed with a new keypair and prints the new
   public key, plus the exact `sqi-server worker enroll` command for step 3.
   `keygen` loads configuration the same way `sqi-worker start` does (the
   root `-c/--config` file, `SQI_WORKER_*` environment variables, and
   built-in defaults), so running it on the worker host with its own config
   resolves `worker.data_dir` and `nats.credential_file` the same way the
   worker itself does. `keygen` also states, in its output, whether the
   worker ID it printed was loaded from an existing `worker.id` or freshly
   generated. A freshly generated ID here means `--data-dir` or
   `worker.data_dir` points at the wrong directory, since the command should
   rotate the key of the worker named in step 1. Pass `--data-dir` to override the directory explicitly for a
   one-off run.
3. `sqi-server worker enroll --worker-id <worker-id> --public-key <new-key>`
   on the server host, using the public key `keygen` just printed.
4. Restart `sqi-worker`. It finds the new seed already on disk (`keygen`
   wrote it directly) and connects with it, and the running server admits
   it without a restart. No REST enrollment call happens on this path, since
   a credential file already exists.

Running `keygen --force` before revoking the old credential is safe but
does not rotate anything on its own: the worker ID stays bound to the old
public key on the server until it is revoked, so the enroll command in step 3 fails until
step 1 has run. To rotate via a fresh join token instead of `keygen`,
remove `worker.nk` before restarting the worker; with no credential file
present it re-enrolls as it did on first boot.

### What an enrolled worker's credential may do

A worker's broker permissions are generated from the worker ID recorded at
enrollment, and cover only that worker's own subtree:

- **Publish:** `task.status.<id>.*`, `task.logs.<id>.*`,
  `worker.register.<id>`, `worker.heartbeat.<id>`, `worker.deregister.<id>`,
  `worker.diag.<id>`, `work.lease.<id>.*`.
- **Subscribe:** `_INBOX_<id>.>` (this worker's own reply inboxes),
  `task.cancel.<id>`, where the server sends every cancel for this worker's
  runs, and `worker.control.<id>`, where the server sends this worker's power
  commands for a [capacity group](capacity.md#power-control-on-the-worker) and
  asks it to register again after marking it offline.
- **Reply once to a request it received:** a power command is a core-NATS
  request, and its reply goes to the requester's reply subject, which no fixed
  publish permission can name. The worker may publish one message, within a
  minute, to the reply subject of a message delivered to it, and nothing else;
  the grant opens no subject by itself. The only messages delivered to a worker
  are those on the subscriptions above, all addressed to it, so the only reply
  subjects it can reach are the server's own control request and the publish
  acknowledgement of its own cancel.

Everything else is denied. A worker cannot subscribe to `task.status.>`,
`task.logs.>` or another worker's `task.cancel.<id>` or `worker.control.<id>`,
so it cannot observe another worker's status, log, cancel or power traffic.

The reply-inbox prefix is per worker, not nats.go's process-global `_INBOX`,
because a work lease is core-NATS request/reply: the assignment batch (command
lines, embedded file contents, job and task parameters, environment variables,
the path map and the run-as-user account) is delivered to the requester's
reply inbox, and only the subscribe permission guards it. A single `_INBOX.>` grant would let any enrolled
worker read every other worker's assignments (and `sqi-server`'s own
JetStream API replies) without leasing a task itself. Each worker therefore
connects with `nats.CustomInboxPrefix("_INBOX_<worker-id>")` and is granted
only that subtree.

### The auth-off asymmetry

With broker authentication off (the default), the worker ID is still a token
in every worker-to-server subject (see
[NATS subjects and streams](architecture.md#nats-subjects-and-streams)), but
nothing enforces it: any client that can reach the broker may publish under
any worker ID it chooses. The scheduler parses that ID out of the subject,
compares it against the task attempt's recorded owner and discards a
mismatch, but with authentication off that check catches faults (a stale
worker, a version mismatch, a client publishing to the wrong subject), not
attackers. Cross-worker forgery is prevented only with
`nats.auth.enabled: true`, which authenticates the connection itself.

`sqi-server` emits a startup `WARN` when broker authentication is off and the
broker may be reachable from other hosts: on the embedded broker when
`nats.embedded.addr` binds to a non-loopback address, naming both remediations
(turn on `nats.auth.enabled`, or bind `nats.embedded.addr` to `127.0.0.1`); on
an external broker always, since `sqi-server` cannot tell who else can reach
a cluster it does not run.

With authentication off there is no callout: the broker maps every anonymous
connection into sqi's account with no permission limits, in both modes.

## Model

Every request carries a `Principal` in its context. When auth is off, the
middleware injects an anonymous principal with the superuser flag set, so
authorization checks are bypassed. Authentication is pluggable: an
`Authenticator` resolves a request's credentials to a `Principal`. The
only non-anonymous `Authenticator`s are the session-cookie and API-key ones
described below.

Externally-verified credentials do not implement that interface.
[LDAP/AD](#ldap--active-directory) attaches at `POST /auth/login`, and
[OIDC/SSO](#oidc--sso) attaches at its own callback route; both verify the
credential once and then mint an ordinary session, so no request path binds
against an external identity provider. See [It attaches at login, not at every
request](#it-attaches-at-login-not-at-every-request).

`Principal` carries `Subject` (opaque user id), `Username` (login name, the
value bound to `Job.Owner`/`Job.Submitter`), `DisplayName`, `Roles`, `Kind`,
and `Superuser`.

`GET /auth/me` returns both `roles` and `permissions`. Clients should gate on
`permissions`: it is computed server-side from the policy matrix, so an
externally-mapped role from an LDAP or OIDC provider needs no
client-side change. A superuser principal (the anonymous identity used when
auth is disabled) reports the full permission set, so every control stays
enabled in an auth-off deployment.

A `Principal`'s `roles` field is populated (a user's single stored role, e.g.
`["admin"]`) and is enforced: every mutating route
and several read routes are gated by a role→permission policy. See
[Roles & permissions](#roles--permissions) for the matrix.

The REST resource routes are gated by the auth middleware; the WebSocket
upgrade is gated by its own hook; the health/readiness/metrics probes and the
OpenAPI spec are always public.

`auth.enabled` does not gate `/debug/pprof/`. The Go runtime profiling
endpoints are mounted on the root router, outside `/api/v1` entirely
(`internal/api/router.go`), so the auth middleware, the CSRF guard and the
rate limiter all miss them whether auth is on or off. They are opt-in
(`http.enable_pprof`, default `false`) and the server logs a `WARN` at boot
when they are enabled. A pprof profile does not dump memory contents (it does
not expose session tokens, the LDAP bind password or the OIDC client secret),
but it does expose internal code paths, allocation sites, live goroutine
stacks, build and module metadata, and the process command line to anyone who
can reach the port. Because these routes sit outside the rate limiter,
`/debug/pprof/profile?seconds=N` and `/debug/pprof/trace?seconds=N` also accept
an arbitrary duration from an unauthenticated caller, which lets anyone who can
reach the port tie up the server. Enable
pprof only on a network you would already trust with an auth-off deployment,
or put a reverse proxy in front of `/debug/`.

## Local accounts

A local account (`internal/store` `User`) has a username (case-insensitive
unique), a display name, a `role` (`admin`, `operator`, `user`, or
`read-only`; defaults to `user` if omitted), and a `disabled` flag. Passwords
are hashed with argon2id (OWASP-baseline parameters: 19 MiB memory, 2
iterations, 1 thread; `internal/auth/password/password.go`) and never
returned by any endpoint.

Accounts are created two ways:

- **Bootstrap**: exactly one admin account, seeded at startup. See
  [First-admin bootstrap](#first-admin-bootstrap).
- **The `/users` REST API** (and the web Admin → Users page):
  `POST /api/v1/users`, `GET /api/v1/users`, `GET /api/v1/users/{id}`,
  `PATCH /api/v1/users/{id}` (display name / role / disabled),
  `PUT /api/v1/users/{id}/password`, `DELETE /api/v1/users/{id}`.

Disabling a user (`disabled: true`) takes effect immediately: the session
authenticator re-checks the user record on every request and rejects a
disabled account's session (`internal/auth/session/session.go`), and
`POST /auth/login` refuses a disabled account with the same generic 401 as a
bad password. Deleting a user cascades to its sessions; see
[Login & sessions](#login--sessions).

## Roles & permissions

Every mutating route and several read routes are gated by a role→permission
policy. There are four built-in roles (there is no custom-role builder):

- **admin** — full access, including user management, API-key management for
  any account, and configuration-adjacent surfaces.
- **operator** — runs the farm: all jobs, workers, farm infrastructure
  (farms/queues/storage/compute/usage-pools), products/presets, and
  diagnostics (server log).
- **user** — submit and control their own jobs; manage their own API
  keys; read-only on infrastructure. This is the only role without
  `jobs.read.all`, so it neither sees nor may mutate a job owned by someone
  else: `read-only` sees every job while `user` sees only its own.
- **read-only** — reads the operational surface; no mutations anywhere;
  cannot see diagnostics or the user list, but can manage its own API
  keys.

| Permission | read-only | user | operator | admin |
|---|:-:|:-:|:-:|:-:|
| jobs.read | ✅ | ✅ | ✅ | ✅ |
| `jobs.read.all` — see jobs owned by anyone | ✅ | ❌ | ✅ | ✅ |
| jobs.write | ❌ | ✅ | ✅ | ✅ |
| `jobs.submit_as` — set `Owner` to another user | ❌ | ❌ | ✅ | ✅ |
| workers.read | ✅ | ✅ | ✅ | ✅ |
| workers.manage | ❌ | ❌ | ✅ | ✅ |
| infra.read (farms/queues/storage/compute/usage-pools/capacity groups and providers) | ✅ | ✅ | ✅ | ✅ |
| infra.manage | ❌ | ❌ | ✅ | ✅ |
| products.read (products/presets) | ✅ | ✅ | ✅ | ✅ |
| products.manage | ❌ | ❌ | ✅ | ✅ |
| diagnostics.read (server log) | ❌ | ❌ | ✅ | ✅ |
| users.read | ❌ | ❌ | ❌ | ✅ |
| users.manage | ❌ | ❌ | ❌ | ✅ |
| apikeys.self (own keys) | ✅ | ✅ | ✅ | ✅ |
| apikeys.admin (anyone's keys) | ❌ | ❌ | ❌ | ✅ |
| `isolation.manage` — set a queue's `run_as_user`/`run_as_group` | ❌ | ❌ | ❌ | ✅ |
| `workers.enroll` — mint worker join tokens; revoke a worker's broker credential | ❌ | ❌ | ❌ | ✅ |

`apikeys.admin` is enforced by `GET /users/{id}/api-keys` and
`DELETE /users/{id}/api-keys/{keyId}`; see [API keys](#api-keys).

A denied request returns 403 with an RFC-7807 problem-details body, and
is recorded to the audit log (`AuditEntry.Actor`) as well as the server's own
diagnostic log. With `auth.enabled=false` (the default), the anonymous
superuser principal bypasses every check.

**Last-admin guard.** The last enabled admin account can't be deleted,
disabled, or demoted to a non-admin role: any of those requests fails with
409 Conflict, so no one can lock everyone out of user management.

## Self-service account changes

Any authenticated principal may change its own display name and password.
Both routes resolve their target from the session (there is no id in the
path), so they cannot reach another account. The web surfaces them at `/account`, linked from the
sidebar identity control.

| Route | Effect |
|---|---|
| `PATCH /api/v1/auth/me` | Sets `display_name`. Returns the same principal shape as `GET /auth/me`. A body omitting `display_name` is a successful no-op returning the caller's identity unchanged, not an error. |
| `PUT /api/v1/auth/password` | Verifies the current password, then sets the new one. |

Behavior:

- **Only `display_name` is accepted.** `role`, `disabled`, and `username`
  are absent from the request type, so a body carrying them is ignored and
  the route cannot change a role.
- **A wrong current password is 403, not 401.** The caller is authenticated
  and only failed a re-auth check; a 401 would trip the web's login
  interceptor and sign them out mid-form.
- **Changing a password evicts every session for the account, then
  re-issues one for the caller**: other devices are signed out while the
  device that made the change stays signed in. API keys are not revoked:
  they are an independent credential, so a password change does not break a
  user's automation. Revoke them explicitly if that is what you want.

With `auth.enabled=false` both routes return 409 Conflict (the anonymous
superuser has no account record to change), and the web hides the Account
link.

## Job identity

`POST /api/v1/jobs` and `POST /api/v1/products/{name}/jobs` resolve the
`Owner` and `Submitter` persisted on a job from the authenticated principal
and whatever the client supplied, following this precedence:

1. Submitter is always the principal's username. A client value is discarded
   without an error, so a client that sends one keeps working when auth is
   switched on.
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
as it is on `GET /api/v1/diagnostics/logs`.

## Task isolation

`Job identity` (above) is an HTTP-layer concept: it records who owns a job in
the database. Task isolation is the OS-layer counterpart: it controls which
OS account a task's process runs as on the worker. The two are independent: a
job's `Owner` need not match the `run_as_user` its queue configures, and
usually does not.

A queue may set `run_as_user` (and optionally `run_as_group`) via
`POST /api/v1/queues` or `PUT /api/v1/queues/{id}`. When set, the scheduler
attaches that username, and never a credential, to every task assignment for
that queue's tasks (`AssignMsg.Isolation` / `protocol.IsolationSpec`,
`internal/worker/protocol/protocol.go`). Both worker↔server transport
protections are opt-in and off by default (authentication via `nats.auth`, see
[Broker authentication (transport)](#broker-authentication-transport), and
encryption via TLS on the broker, see [`docs/tls.md`](tls.md)), so the
assignment channel is treated as readable in every deployment and carries no
secret. The worker resolves the username to a real OS credential
locally and runs both job-code launch sites under it — an OpenJD
environment's `onEnter`/`onExit` actions and a task's own actions — with a
filtered environment and a session working directory private to that user.
This runs on both POSIX (Linux/macOS) and Windows. On Windows the worker
itself must run as a service under LocalSystem (or hold
`SeAssignPrimaryTokenPrivilege` directly); an elevated Administrator shell
does not have that privilege by default. See
[`docs/worker-configuration.md`](worker-configuration.md) for the `isolation`
worker-config block, the per-platform requirements, and the
environment-allowlist mechanics.

Workers must run the same release as the server: the server discards the
messages of a worker that speaks a different wire-protocol version and offers
it no work (see
[Workers without isolation support](configuration.md#workers-without-isolation-support)).

### `isolation.manage` is a separate, admin-only permission

Setting a queue's `run_as_user`/`run_as_group`, including sending an explicit
`null` to clear a stored value, requires the `isolation.manage` permission.
`infra.manage`, which the operator role holds and which governs every other
queue field, is not sufficient: choosing the OS identity that job code runs
under is an escalation surface, so it has its own gate, held only by `admin`
(see the role matrix above).

Omitting `run_as_user`/`run_as_group` from a `PUT` body is not the same as
sending them. A `PUT` is otherwise full-replace, but for these two fields
omitting the key preserves whatever is stored and requires no permission,
while a body that includes the key, even as `null`, is an attempt to change it
and requires `isolation.manage`. An operator with only `infra.manage` can
therefore edit a queue's priority without clearing an admin's `run_as_user`.
The preserving write is a single atomic `UPDATE`, so it cannot overwrite a
concurrent admin write.

### Enabling isolation raises the worker daemon's own privilege

Turning isolation on makes the worker's own daemon process more privileged.
On POSIX, becoming another user (`setuid`/`setgid`/`setgroups`) requires
starting as root: `isolation.Provider.Capable()` returns an error unless the
worker's effective uid is 0. The daemon gains root so that individual tasks
can run without it. Compared with an unprivileged, unisolated worker, this
trades one unprivileged daemon whose tasks all run as its account for one
root daemon with many unprivileged task processes. Weigh that for your
environment before enabling `isolation.required` or configuring any queue's
`run_as_user`.

### Privileged accounts and groups are refused

Independent of, and in addition to, the supplementary-group stripping below,
`isolation.Provider.Resolve` refuses several requests rather than narrowing
them:

- **`run_as_user` naming a known-privileged account**: `root`,
  `Administrator`, `Administrators`, `SYSTEM`, `LocalSystem`,
  `NETWORK SERVICE`, and `LOCAL SERVICE` by name, and any account whose
  uid is 0, regardless of name. Name matching is case-insensitive and
  first strips a `DOMAIN\` / `HOST\` / `.\` qualifier and a trailing
  `@domain` UPN suffix, so `.\Administrator`, `CORP\Administrator` and
  `Administrator@corp.example.com` are all the same refused name. The list
  is not configurable, since the risk it guards against is a queue pointing
  at an account more privileged than the daemon.
- **`run_as_group` naming a known-privileged group**: `root`, `wheel`,
  `admin`, `sudo`, `sudoers`, `adm`, `docker`, `disk`, `shadow`, `staff`,
  `administrators`, by name. This checks the group the queue asks for, not
  the target account's existing memberships (see the next section): a queue
  cannot select `docker` or `wheel` as the isolation group even if the target
  account belongs to it.
- **A `run_as_user` account whose primary group is gid 0**, even when
  `run_as_group` is not set, since the primary group is determined by the
  account, not by queue config. gid 0 is refused whatever name a platform
  gives it (`root` on Linux, `wheel` on macOS/BSD).

So `wheel` is never a group job code can reach on macOS/BSD: it is gid 0 there, so it is refused both as an explicit `run_as_group`
target and, via the gid-0 check just above, as anyone's primary group.

### A target account's existing group memberships reach job code

Isolation strips gid 0 (`root`) from a target account's supplementary group
list wherever it appears. It does not strip any other group the account
belongs to, such as `docker`, `disk`, `shadow`, or an in-house privileged
group: those memberships are the account's own access, and supplementary
groups on a render-farm account usually exist to grant project-storage access
(an NFS-exported group, for example), which stripping them would break.

Managing this is the operator's job: do not point a queue's `run_as_user` at
an OS account that belongs to `docker`. Membership in `docker` is a one-step
escalation to root (a container can bind-mount the host root and chroot into
it), so isolating into that account grants job code the privilege isolation
exists to deny.
The same caution applies to `disk` and `shadow`. Audit the target account's
group memberships (`id <user>`) before assigning it to a queue, the same way
you would audit any account you grant OS-level access to.

### Environment allowlist and `env_passthrough`

An isolated task's environment is filtered when its session is created:
starting from a minimal base (`PATH`, `HOME`/`USERPROFILE`, `TMPDIR`, and a
handful of others rewritten to the target user rather than the daemon's own),
only daemon-environment variables whose name matches an
`isolation.env_passthrough` glob are added. This governs only what the
isolated task inherits from the worker daemon's own environment. Variables a
job supplies itself (an OpenJD `Environment.variables` block, an
`openjd_env` export, a task template variable) always pass through,
regardless of `env_passthrough`.

License servers usually need some variables passed through, for example:

```yaml
isolation:
  env_passthrough:
    - "foundry_LICENSE"
    - "ARNOLD_LICENSE"
    - "solidangle_LICENSE"
```

A broad glob defeats the allowlist. `env_passthrough: ["*"]`, or a pattern
that looks narrower such as `"*KEY*"`, passes daemon variables (credentials,
tokens, internal service addresses) to job code. Write globs as specific as
the variable names in use; a missing license variable in a task's log is the
symptom of an allowlist that is too tight, so widen it by name, not by
wildcard. See
[`docs/worker-configuration.md`](worker-configuration.md) for the full
`isolation` config reference.

## Login & sessions

`POST /api/v1/auth/login` takes `{"username", "password"}` and, on success,
mints a server-side session and sets it as a cookie via `Set-Cookie`. On
failure it returns a 401 with an identical body whether the username is
unknown, the password is wrong, or the account is disabled, so the endpoint
can't be used to enumerate valid usernames. The unknown-user path runs a
dummy argon2id verify so its response time matches a real check.

The cookie:

| Attribute | Value |
|---|---|
| Name | `auth.session.cookie_name`, default `sqi_session` |
| `HttpOnly` | always set; client-side JS cannot read it |
| `SameSite` | always `Lax` |
| `Secure` | per `auth.session.cookie_secure`: `"auto"` (default), `"true"`, or `"false"` |
| `Max-Age` | `auth.session.ttl` in seconds, default `168h` (7 days) |

`cookie_secure` is a three-valued string, not a bool, because sqi's default
deployment is a trusted, plain-HTTP LAN (`http.addr` defaults to
`0.0.0.0:8080`, and TLS is opt-in; see [`docs/tls.md`](tls.md)). `"auto"` sets
`Secure` when the request arrived over TLS or carries
`X-Forwarded-Proto: https`, which suits a TLS-terminating proxy that sets that
header. `"false"` forces `Secure` off for plain HTTP, and `"true"` forces it on
where auto-detection would miss it.

Session expiry is absolute, not sliding: `auth.session.ttl` sets the
lifetime from creation, and using a session does not extend it. A session
becomes invalid at exactly `created_at + ttl` regardless of activity; the
user must log in again.

`POST /api/v1/auth/logout` deletes the session server-side and clears the
cookie; it always returns `200` with a JSON body (even if the cookie was
already invalid). The body is `{}` for a local logout, and carries
`redirect_url` (the identity provider's RP-initiated logout URL) when SSO is
configured with `auth.oidc.logout_mode=provider`.
`GET /api/v1/auth/me` returns the current `Principal`: `401` if
unauthenticated, otherwise the resolved subject/display name/roles/kind. This
is the single endpoint the web UI polls to decide shell-vs-login; see
[`docs/web-development.md`](web-development.md).

Deleting a user (`DELETE /api/v1/users/{id}`) cascades to its sessions at the
database level (`sessions.user_id REFERENCES users(id) ON DELETE CASCADE`):
every session belonging to a deleted user is revoked in the same transaction
as the delete.

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
set a real password via `PUT /api/v1/users/{id}/password` (or the web
Admin → Users page); the bootstrap password is meant to be temporary.

Bootstrap behavior:

- **Idempotent and non-destructive.** It only runs when the `users` table is
  empty. Once any user exists, bootstrapped or otherwise, it is a no-op, even if the bootstrap env vars are still set; it never
  overwrites an existing account's password.
- **Empty and unconfigured does not fail closed.** If auth is enabled, the
  table is empty, and neither bootstrap variable is set, the server logs a
  `WARN` ("auth is enabled but no users exist and no bootstrap credentials
  are configured…") and starts. No one can log in, so every authenticated
  route is unusable until an operator sets the bootstrap env vars and
  restarts, or seeds a user directly.
- **A half-set pair is a startup validation error**, not a warning: setting
  only `auth.bootstrap.username` or only `auth.bootstrap.password` (e.g. a
  typo'd env var name) fails config validation and the server does not
  start, so it never tries to create an admin with an empty password.

## CSRF & CORS

A session cookie is an ambient credential: once set, the browser attaches it
to every request to this origin, including ones a malicious third-party page
initiates (CSRF) and the WebSocket upgrade (cross-site WebSocket hijacking is
the same vector applied to `/api/v1/ws`). An `Authorization` header is never
attached automatically, so only cookie-authenticated requests need these
defenses.

sqi's model, mounted only when auth is enabled:

- The cookie itself carries `SameSite=Lax`, which blocks the cookie
  from being sent on most cross-site subrequests (though not top-level
  navigations, which `Lax` still allows).
- `internal/middleware/csrf.go` enforces an Origin check on unsafe methods
  for cookie-authenticated requests: `GET`/`HEAD`/`OPTIONS` are never checked
  (they must not have side effects); a request that does not carry the
  session cookie passes through unchecked; an unsafe-method request that does
  carry the cookie must present an `Origin` (falling back to `Referer`'s
  origin) that is either same-origin or in an explicit allow-list, or it is
  rejected with `403`. A
  cookie-bearing unsafe request with neither header at all is rejected too:
  browsers always send one or the other on such requests, so their total
  absence isn't trusted.
- CORS enables the credentialed cross-origin case at the browser level:
  `Access-Control-Allow-Credentials` is only ever sent when auth is enabled.
  A wildcard `*` origin cannot be combined with credentials (browsers reject
  the combination), so if the configured origin list contains `"*"` while
  auth is enabled, the router drops the wildcard and logs an error.

The normal deployment is same-origin (`sqi-server` serves the built web UI
itself, `web/dist/` embedded via `internal/ui`), so none of this affects the
shipped UI in its default configuration. It only matters for a
separately-hosted UI (a different scheme/host/port) that wants to call a
`sqi-server` instance's cookie-authenticated API cross-origin.

> **A WebSocket is authenticated once, at upgrade.** Authentication and
> owner-scope resolution happen when `/api/v1/ws` is upgraded and are not
> re-evaluated for the life of the connection. Disabling the account, revoking
> its session, or changing its role does not drop a live WebSocket: the
> connection keeps delivering until the client closes it or it hits the idle
> timeout (`wsIdleTimeout`, 5 minutes, in `internal/api/ws.go`). It keeps
> receiving only what it was authorized to receive at upgrade. REST
> re-authenticates and re-authorizes every request, so a revoked credential
> stops working on the next call.

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
with a `http.cors_origins` validation error, since go-chi/cors could never
match such a value. Wildcard patterns other than the bare `"*"` are rejected
too: an entry containing an embedded `*` (e.g. `https://*.example.com` or
`https://app.example.com*`) is refused at startup, because go-chi/cors would
honor it as a prefix/suffix match, with credentials once auth is enabled,
letting an attacker-registrable origin (like
`https://app.example.com.evil.io`) use a victim's session cookie. Name every
allowed origin explicitly.

Origins are scheme-sensitive, so enabling TLS invalidates `http://` entries.
The Origin check compares scheme, host and port: `http://ui.example.com` and
`https://ui.example.com` are different origins. If you turn on
`http.tls` ([`docs/tls.md`](tls.md)), or move behind a TLS-terminating proxy,
update every entry here to `https://` at the same time or credentialed
cross-origin requests from the UI return `403`. The same applies to the
same-origin path: it resolves the request's scheme from `r.TLS` or
`X-Forwarded-Proto`, so a proxy that does not set that header makes a
same-origin `https://` request look cross-origin.

Leaving the list empty uses the default, `["*"]`. The wildcard drop above
applies: with auth enabled, `"*"`, explicit or defaulted, is dropped and
credentialed cross-origin requests are refused. A separately-hosted UI must therefore name its origin
explicitly here. Same-origin deployments need none of this.

## Headless / SDK auth

sqi's headless credential is the API key, covered below.
`internal/server/server.go`'s `selectAuth` wires
`auth.Chain(keyAuthn, sessAuthn)`: a Bearer API key is tried first, and the
session cookie is the fallback for browser requests.

The Python SDK (`clients/python`) supports this:
`SqiClient(base_url, token=...)` sends `Authorization: Bearer <token>`,
falling back to the `$SQI_TOKEN` then `$SQI_API_KEY` environment variables
when `token` isn't passed explicitly, and a 401/403 response raises the typed
`SqiAuthError` (`clients/python/src/sqi_client/errors.py`). The submitter
framework (`clients/submitter`) resolves a key the same way, one tier
simpler: an `api_key` argument, then `$SQI_API_KEY`, then the `api_key` key
in `~/.sqi/submitter.json` (see `clients/submitter/README.md`). With
`auth.enabled=true`, issue yourself a key (`POST /api/v1/api-keys` or the web
Admin → API Keys page) and pass it via `token=`/`$SQI_TOKEN`/`$SQI_API_KEY`
(SDK) or `api_key=`/`$SQI_API_KEY`/`submitter.json` (submitter) for
headless use.

## API keys

An API key is a per-user, `sqi_`-prefixed Bearer credential for scripts, the
SDK, and DCC submitters: the headless counterpart to the browser session
cookie above.

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

The `secret` field (the full raw key) is present only in this create
response. Every other response (`GET /api/v1/api-keys`, the list on the web
Admin → API Keys page) omits it and shows the `prefix` instead, so copy the
secret down before navigating away; it cannot be recovered later, only
revoked and reissued.

**Presentation.** Clients send `Authorization: Bearer <key>`. Keys are the
credential for headless access; browser sessions stay cookie-based as
described above. See [Headless / SDK auth](#headless--sdk-auth) for how the
SDK and submitter pick a key up from an argument, environment variable, or
settings file.

**Storage & security.** The raw key is generated as 256 bits of random data,
base64url-encoded, and prefixed `sqi_`. Only its hex SHA-256 digest
(`internal/auth/password.HashToken`) is stored in the `api_keys` table,
never the raw key, alongside a 12-character display `prefix` taken from the
start of the raw key, used to tell keys apart in the list view without
revealing the secret. `last_used_at` is updated on successful authentication,
throttled to at most once per minute per key so a busy key doesn't write on
every request (`internal/auth/apikey.Authenticator`'s `touchThreshold`).
Optional `expires_at` is enforced at authentication time:
`GetAPIKeyUserByTokenHash` only matches rows that are unexpired (and
unrevoked), so an expired key stops authenticating the moment it lapses,
with no separate sweep.

**Revocation.** `DELETE /api/v1/api-keys/{id}` is a soft revoke (it sets
`revoked_at` rather than deleting the row) and takes effect at once: the same
"unrevoked" filter that enforces expiry rejects the next request bearing that
key.

**Scope.** `POST`/`GET`/`DELETE /api/v1/api-keys` are self-scoped: they
resolve the caller's own user id and only ever see or touch that user's
keys (`apikeys.self`, held by every role).

Admins additionally hold `apikeys.admin`, which unlocks two cross-user
routes:

| Route | Effect |
|---|---|
| `GET /api/v1/users/{id}/api-keys` | List that user's keys (metadata only, never a secret). |
| `DELETE /api/v1/users/{id}/api-keys/{keyId}` | Revoke one of that user's keys. |

There is no admin create. An admin may see and revoke another person's keys,
but no route mints a credential someone else is accountable for.

Revocation stays owner-scoped: a `keyId` that does not belong to the named
user returns 404, so the scoping is the authorization check. The web surfaces this at `/users/{id}/api-keys`, reachable from
the per-row "API keys" action on Admin → Users.

**Auth-off behavior.** With `auth.enabled=false`, every request is the
anonymous superuser principal, which has no real user id to own a key
against, so all three `/api-keys` endpoints reject with `409 Conflict`
("API keys require authentication to be enabled").

**CSRF.** A Bearer request carries no cookie, so the CSRF guard in
[CSRF & CORS](#csrf--cors) never engages for it: that guard only inspects
requests that carry the session cookie in the first place, and a
Bearer-authenticated request has nothing for it to check.

## LDAP / Active Directory

`sqi-server` can verify passwords against an LDAP or
Active Directory server instead of its own store. Enable it with
`auth.ldap.enabled` on top of `auth.enabled`: LDAP adds to the auth system
rather than replacing it, and an auth-off server never contacts a directory
whatever `auth.ldap.*` says. Every field is catalogued in
[`docs/configuration.md`](configuration.md#authldap).

### It attaches at login, not at every request

LDAP is a login-time credential verifier, not an `Authenticator` alongside
the session and API-key ones, so the directory is not a dependency of every
page load and SDK poll.

Instead, `POST /api/v1/auth/login` checks the password against the directory
and then mints the same server-side session a local account gets. After
that nothing is LDAP-specific: `auth.Chain(apikey, session)` is unchanged,
and the session cookie, its TTL, roles and permissions, job owner binding,
and WebSocket scoping all behave as documented above. A directory outage
blocks new logins; it does not disturb sessions already issued.

The practical consequence is [revocation lag](#revocation-lag), below.

### Per-account routing

Each account carries `users.auth_source`: `local`, `ldap`, or `oidc`. It is
set when the account is created, and no route can change it.
`POST /auth/login` reads it and consults exactly one backend:
the stored argon2id hash, or the directory. Never both, never in sequence. An
`oidc` account has no password path at all; it signs in through
[the SSO routes](#oidc--sso).

The two are not chained: if a failed local login fell through to a directory
bind, every wrong password in sqi would become a failed bind against a real
DN, and in Active Directory repeated bad binds lock the directory account, so
a brute force against sqi would lock accounts across the organization.

The same routing means a local account shadows a same-named directory
account. If `alice` exists locally, a directory `alice` can never log in, and
the directory is not contacted. A directory login never adopts an existing
local record, since that would let anyone who can create a directory account
named `admin` inherit the local admin. Both cases return the same generic
401; the users page (Admin → Users) shows each account's source, which is
usually the quickest way to see why a login is refused.

### Just-in-time provisioning

An unknown username with LDAP enabled is a provisioning event: sqi binds
against the directory, maps a role from the returned groups, and creates a
local record with `auth_source: ldap`. There is no import step and no
pre-registration. The stored `password_hash` is an unusable placeholder, not
a copy of the directory password.

The display name is read from `display_name_attr` once, at creation, and
never re-synced, so a directory user's self-service display-name edit
(`PATCH /api/v1/auth/me`) persists across logins. A name changed in the
directory does not propagate; an admin edits it, or the user does.

### Accounts are matched on a stable identifier, not a username

`unique_id_attr` is required whenever LDAP is enabled and has no default.
Set it to `objectGUID` on Active Directory, or `entryUUID` on OpenLDAP and
other RFC 4530 servers. No single value is correct on both, and a server can
expose both.

sqi stores that value in `users.external_id` and matches every later login on
it, never on the username. Directories recycle login names and email
addresses, so matching on the name would let a new hire given a departed
admin's name log into that admin's account, and a rename at the directory
would orphan the account and provision a duplicate.

Two consequences:

- **A directory rename is transparent.** The entry keeps its identifier, so
  the same sqi account is reached under the new name.
- **A recycled username is refused, not adopted.** A new directory entry
  with an old name has a new identifier, so provisioning runs and collides
  on the taken username. The login fails until an operator renames or removes
  one of the two accounts.

Active Directory returns `objectGUID` as raw binary, which sqi hex-encodes
before storing. The encoding must not change: a different one would orphan
every account already stored.

#### Accounts with no stored identifier

An LDAP account whose `external_id` is empty cannot log in: the identity
lookup misses, provisioning collides on the username, and the result is a
401 every time. Adopting such a row would be username matching under another
name. Delete and recreate the account; the next login re-provisions it with
the directory's identifier. The server logs an `ERROR` naming the account and
this remedy each time one is refused; the 401 itself is identical to every
other login failure, so the log is the only signal.

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
the search on an anonymous connection, for a world-readable directory.
Setting `bind_password` without `bind_dn` is rejected at boot, since the
password would otherwise be discarded and the search would go out
anonymously.

**Alias / UPN login works.** With `user_filter: "(userPrincipalName=%s)"` and
`username_attr: "sAMAccountName"`, a user typing `alice@example.com` is
provisioned as `alice` and is recognized as that same account on every
subsequent login. The two spellings do not create two records, and since
accounts match on the identifier rather than the name, that holds across a
directory rename too.

**`username_attr` should name a directory-controlled, unique attribute.**
sqi does not match accounts on it (`unique_id_attr` is for that), but it is
the value written to `users.username`, which is unique in sqi's own store,
and it is what binds to `Job.Owner`/`Job.Submitter`. If it names an attribute
users can edit themselves (`mail`, for example), one user can claim the name
another account holds, which blocks that account's next login on a username
collision until an operator renames one of the two rows. That is a denial of
service, not a privilege escalation: a collision is refused rather than
adopted, and no account is inherited. `sAMAccountName` and `uid` are the right kind of
attribute; a self-service directory field is not.

**Nested groups are search-mode only.** `nested_groups: true` expands
transitive membership via the AD matching-rule OID; template bind reads the
flat `memberOf` attribute and cannot do it, so config validation rejects the
combination. About the expansion:

- It runs on the service-account connection, not the user's, so the
  service account needs read access to the group tree.
- `base_dn` must cover the group tree as well as the user tree. The
  expansion reuses `base_dn` as its search base. Scoping it narrowly (say
  `base_dn: "OU=Users,DC=example,DC=com"` while groups live under
  `OU=Groups`) produces a search that succeeds and matches nothing, with no
  error. Set `base_dn` to a subtree containing both (commonly the
  domain root, `DC=example,DC=com`).
- If it fails, or returns nothing while flat `memberOf` had values, sqi
  falls back to flat `memberOf` and logs a `WARN` rather than failing the
  login. A user who holds `admin` only through a nested group is then granted
  a lower role for that session, and the warning in the server log is the
  only signal. If your admin group is nested, treat that `WARN` as an alert.
- The matching rule is Active-Directory-only. OpenLDAP 2.6 does not reject
  the unknown rule: it rewrites the filter to `(?=undefined)` and answers
  success with zero entries. On a non-AD directory `nested_groups: true`
  therefore never expands anything; it takes the fallback path above on every
  login, logging a `WARN` each time. Leave it `false` unless you are on AD.

### Your directory must populate `memberOf`

Both bind modes read group membership from the `memberOf` attribute on the
user's own entry. Active Directory populates it natively. OpenLDAP does not;
it requires the `memberof` overlay to be configured on the database:

```
overlay memberof
memberof-group-oc groupOfNames
memberof-member-ad member
memberof-memberof-ad memberOf
```

Without it, every search and bind succeeds, the user authenticates, and sqi
sees no groups, so every account lands on `default_role`. There is no warning
for this, because a user in no groups looks the same as a directory that
never populates `memberOf`. If every LDAP user is arriving with your `default_role`, check
the overlay before anything else.

Check from the command line before configuring sqi; the attribute must come
back for a user you expect to be in a group:

```sh
ldapsearch -x -H ldap://ldap.example.com -D "<bind_dn>" -W \
  -b "dc=example,dc=com" "(uid=alice)" memberOf
```

The account that reads it must also be allowed to: in search mode that is the
service account (or the anonymous connection, if `bind_dn` is unset), and in
template mode it is the user, reading their own entry.

### Group → role mapping

`role_map` is an ordered list of group-DN → role rules, and the first match
wins, so put `admin` above `operator`. A role naming anything other than
`admin`, `operator`, `user`, or `read-only` fails config validation at boot.

`default_role` applies when no rule matches. Setting it to empty rejects the
login, which is how a deployment requires group membership to sign in. The default is `read-only`.

### `role_source` — who owns a user's role

| Value | Role on login | `PATCH /users/{id}` role edit |
|---|---|---|
| `directory` (default) | Recomputed from groups on every login | 409 Conflict |
| `local` | Seeded from groups at JIT-create only | Allowed |

One value drives both columns, so a role edit the API accepts is never
reverted by the next login.

Switching `local` → `directory` overwrites every manual role assignment: at
each user's next login their role is recomputed from their groups, one user
at a time as people log back in. The server logs the active `role_source` at
boot, which records when the mode changed.

### Keep a local admin account

In `directory` mode, keep a local admin account. If the admin group is
renamed or deleted, every LDAP admin maps to `default_role` at their next
login and is demoted. There is then no admin in the system and no way to fix
it through the UI: role edits on directory accounts are 409, and nobody with
`users.manage` is left to make them.

A local account (such as the [bootstrap admin](#first-admin-bootstrap)) is
unaffected by any directory-side change and can always log in and
repair the configuration.

**Known gap:** the [last-admin guard](#roles--permissions) does not cover
this. It refuses the last admin's deletion, disablement, or demotion through
the API. A group renamed in Active Directory reaches the same end state
outside the API, where sqi cannot detect it; the local admin account is the
mitigation.

### Revocation lag

Login is the only moment sqi talks to the directory, so a user disabled or
deleted in the directory keeps their sqi session until it expires
(`auth.session.ttl`, 7 days by default). They cannot get a new session, but
the one they hold keeps working.

Shorten `auth.session.ttl` if that window matters. To cut a user off
at once, disable the account in sqi (`PATCH /api/v1/users/{id}` with
`disabled: true`): the session authenticator re-checks the user record on
every request, so it takes effect on the next call, and the local `disabled`
flag overrides the directory for both session checks and login.

sqi does not auto-disable accounts when a directory lookup fails, so a DC
outage cannot disable every account on the farm.

### Timing

The 401 bodies are byte-identical across every failure path: unknown user,
wrong local password, wrong directory password, directory unreachable, no
role matched, collision with a local account, disabled account,
unrecognized `auth_source`. The latencies are not identical.

The local-only equalization described under
[Login & sessions](#login--sessions) works because both sides of that
comparison are argon2id derivations of matching cost. A directory bind is a
network round trip that no local computation can match, so an observer
timing `/auth/login` can distinguish, at minimum, an immediate rejection (a
locally disabled account, a shadowed local account) from a local password
check from a directory round trip. Whether that allows account enumeration
depends on your directory's own timing: a bind against a nonexistent DN
typically takes a different time from one against a real DN, and sqi cannot
conceal that.

The mitigation for the timing channel is rate limiting; see the next section
for what the built-in limiter does.

### Brute force: what sqi does, and does not, protect against

There is no login-specific throttle. The only control is the generic per-IP
token bucket applied to all of `/api/v1` (`internal/api/router.go`):
20 requests/second sustained, burst 40, keyed on client IP. Nothing counts
failures, backs off after a wrong password, or locks an account, and nothing
distinguishes `/auth/login` from a job listing.

Twenty per second is roughly 1.7 million login attempts per day, per source
IP, so treat it as no brute-force control. It keeps one client from
saturating the API; it is not a credential defense.

`http.rate_limit.enabled: false` turns even that off (the load harness does
so on the servers it starts). Leave it on for any server untrusted clients
can reach: with it off, nothing throttles `/auth/login` or
`/workers/enroll`.

**Failed logins against directory accounts hit your directory.** For an
account whose `auth_source` is `ldap`, every wrong password produces a real
bind against a real DN. In Active Directory that increments `badPwdCount`, so
an unauthenticated attacker who can name your users can drive them into
domain-wide lockout, locking them out of Windows, email and everything else
as well as sqi. [Per-account routing](#per-account-routing) prevents this for
local accounts only.

**Unknown usernames also cost the directory.** With LDAP enabled, an
unrecognized username takes the just-in-time provisioning path, which
consults the directory before failing, so `/auth/login` turns
unauthenticated HTTP requests into domain-controller round trips.

If any of this matters to you, put a control in front of sqi that counts
failures per account and per source and backs off: a login-specific
throttle, fail2ban on the access log, or a WAF rule on
`POST /api/v1/auth/login`. sqi does not ship one. Do not expose an
LDAP-enabled deployment to the internet without it.

### A hung directory

go-ldap offers no context-aware dial, so request cancellation cannot abort a
login in progress: a client that gives up does not free the server-side
attempt. `auth.ldap.timeout` (default `10s`) bounds it, applied to the TCP
connect and to each subsequent request leg. Set it to the longest you are
willing to have a request block.

### Directory accounts have no local password

There is no password in sqi for a directory account to change, so the
password routes refuse:

| Route | On an `ldap` account |
|---|---|
| `PUT /api/v1/users/{id}/password` (admin) | 409 Conflict |
| `PUT /api/v1/auth/password` (self-service) | 409 Conflict |
| `PATCH /api/v1/auth/me` (display name) | Works |

Both password routes key on "not a local account", so an
[SSO account](#oidc--sso) behaves identically. The same rule is what the
users API reports as `password_editable` (the companion to `role_editable`),
so a client can hide its set-password control. Field and guard are computed
from one server-side predicate.

The admin-side 409 also stops an admin writing a genuine argon2id hash onto a
directory account. Login routes on `auth_source` and does not consult it, but
a usable credential left in the row would become a bypass if that routing
changed.

`PATCH /auth/me` is not guarded, for the reason given under
[just-in-time provisioning](#just-in-time-provisioning): the display name is
seeded from the directory once and never re-synced, so a self-service edit
has nothing to conflict with and persists.

### Turning LDAP off strands the accounts it created

Disabling `auth.ldap.enabled` removes no accounts. Every row provisioned with
`auth_source: ldap` stays as it is, and every route that touches it refuses:

| Action on a stranded `ldap` account | Result |
|---|---|
| Login | 401: no verifier is configured, so no credential can satisfy it |
| `PUT /users/{id}/password` | 409: a directory account has no local password |
| `PATCH /users/{id}` role edit (under `role_source: directory`) | 409: the role is directory-owned |

The account cannot be repaired in place. Delete and recreate it as a local
account. There is no conversion endpoint, because changing an account's
`auth_source` is the operation that would let a directory entry inherit a
local account's privileges (see [per-account routing](#per-account-routing)).

Display names and role assignments do not survive that round trip, and the
user's sessions are deleted with the old row. If you are migrating away from
LDAP, plan it as a re-provisioning exercise, not a config flag.

### How this is tested

Most tests of this feature drive a fake LDAP connection. Those cover sqi's own
logic (routing, provisioning, role mapping, collisions, the equalized 401),
but a fake cannot catch a mistake in the go-ldap wire usage:
a wrong search scope, a misnamed attribute, a filter a real server rejects, or
a server that answers an unsupported request in a way the fake never would.

`test/integration/ldap_test.go` therefore runs the whole login path against a
real OpenLDAP server in a throwaway container, in every supported
configuration (search-then-bind with a service account, template bind, and
anonymous search-then-bind), plus group→role mapping and precedence, `default_role`,
JIT provisioning, role re-sync under `role_source: directory`, filter and DN
injection, and empty-password binds. It runs in CI on every change, on both
amd64 and arm64.

```sh
make test-ldap        # needs Docker (or colima/podman); skips cleanly without it
```

Point it at a directory you already have, including a real Active Directory,
with `SQI_TEST_LDAP_URL`, and it uses that instead of starting a container. The
fixture tree it expects is the `seedLDIF` constant in that file.

OpenLDAP does not reject the AD-only nested-group matching rule; it answers
success with zero entries. Letting that empty result replace a user's real
groups would demote them to `default_role` with no error. The suite covers
that case, which no fake reproduces.

Test a new deployment against your own directory before relying on it.
Directories differ in the places this integration is sensitive to: whether `memberOf` is populated, what the service account is allowed to
read, and how an unsupported matching rule is answered.

## OIDC / SSO

`sqi-server` can sign users into the web UI through an OAuth2/OpenID Connect
identity provider: Keycloak, Microsoft Entra ID, Okta, or anything else that
publishes a discovery document. Enable it with
`auth.oidc.enabled` on top of `auth.enabled`; the block is inert unless both
are true. Every field is catalogued in
[`docs/configuration.md`](configuration.md#authoidc).

**Scope: this is a browser login mechanism only.** The callback mints the same
server-side session cookie a password login mints, and
`auth.Chain(apikey, session)` is unchanged. sqi never accepts a provider's
token as a per-request API credential, and there is no device-authorization
grant. Headless clients (the SDK, submitters, CI) use
[API keys](#api-keys).

### The flow

Three routes, all public by necessity:

| Route | Purpose |
|---|---|
| `GET /api/v1/auth/providers` | What the login page renders: whether password login is offered, and the SSO button's label and login URL. Exposes neither the issuer nor the client secret. |
| `GET /api/v1/auth/oidc/login` | Starts the authorization-code flow (PKCE) and redirects to the provider. |
| `GET /api/v1/auth/oidc/callback` | Redeems the code, validates the ID token, resolves the account, mints the session, and redirects to `/`. |

They cannot sit behind the auth middleware, since they produce the principal
that middleware requires. What stands in for it:

- **The flow state lives in an HMAC-signed, `HttpOnly` cookie**, carrying the
  `state` value, the nonce, and the PKCE verifier. There is no server-side
  table and no cleanup job. The signing key is generated at each start and
  never persisted, so a server restart invalidates logins that are in flight.
- **The state cookie is cleared unconditionally, before any early return**, so
  a replayed callback finds nothing waiting for it. The signed payload also
  carries its own issued-at, checked server-side, so an expired state is
  refused whether or not the browser honored the cookie's expiry.
- **The post-login destination is a constant.** No "return to" parameter is
  honored, so the callback cannot be used as an open redirect from a page
  that has just minted a session.
- **Every failure redirects to `/?sso_error=1`** and says nothing else. The
  reason goes to the server log only; a reason on the wire would turn the
  callback into an enumeration oracle. If SSO is failing, look in the server
  log: `internal/auth/oidc` itself logs nothing on token rejection, so the
  route logs every refusal.

Discovery is lazy: the issuer's document is fetched on first use, not at
boot, and is retried after a failure, so a briefly unreachable provider does
not stop the server from starting. Configuration faults still abort boot: a
missing `client_id` fails validation, an unreachable issuer does not.

### A worked setup (Keycloak)

Keycloak is the provider sqi's integration test drives. Register a
confidential client in your realm with the standard flow enabled, a redirect
URI of `https://sqi.example.com/api/v1/auth/oidc/callback`, and a protocol
mapper that puts group membership into the `groups` claim. Keycloak emits no
groups without one, and a token with no groups still validates, so every user
lands on `default_role`.

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

`redirect_url` must be the absolute URL of this server's callback route, and
must be registered at the provider byte-for-byte. `client_secret` is redacted
in `sqi-server config print`; prefer `SQI_AUTH_OIDC_CLIENT_SECRET` over writing
it to disk.

### Claim → role mapping

`role_map` is an ordered list of group → role rules read from the claim
named by `groups_claim`, and the first match wins, so put `admin` above
`operator`. LDAP uses the same mapper (`internal/auth/rolemap`), so the two
apply precedence the same way. A role naming anything other than `admin`, `operator`, `user`, or
`read-only` fails config validation at boot.

`default_role` applies when no rule matches; setting it empty rejects the
login, which is how a deployment requires group membership to sign in. The
default is `read-only`.

`"groups"` is not a standard OIDC claim. Whether membership needs a scope, a
provider-side mapper, or both varies by provider, and there is no error when
it is absent: the token validates and the user lands on `default_role`. If
every SSO user arrives with your `default_role`, check the claim first. (This
is the OIDC counterpart of LDAP's
[`memberOf` requirement](#your-directory-must-populate-memberof), and fails
the same way, with no error.)

### Just-in-time provisioning and identity

An unknown SSO identity is a provisioning event: sqi maps a role from the
claims and creates a local record with `auth_source: oidc`, its stored
`password_hash` an unusable placeholder. There is no import step.

**Accounts are matched on the `sub` claim, never on the username**, by the
same rule and code path as
[LDAP's stable identifier](#accounts-are-matched-on-a-stable-identifier-not-a-username).
`sub` is the provider's stable, non-reusable subject identifier; it is stored
in `users.external_id`. A username or email changed at the provider therefore
reaches the same sqi account, and a recycled email address does not inherit
the account it used to belong to. An identity whose `sub` is empty is refused,
with an `ERROR` in the log, rather than matched by name.

Display name is read from `display_name_claim` once, at creation, and never
re-synced, so a self-service `PATCH /api/v1/auth/me` edit persists. A name
changed at the provider does not propagate.

### `role_source` — who owns an SSO user's role

| Value | Role on login | `PATCH /users/{id}` role edit |
|---|---|---|
| `directory` (default) | Recomputed from claims on every login | 409 Conflict |
| `local` | Seeded from claims at provisioning only | Allowed |

One value drives both columns, for the reason given under
[LDAP's `role_source`](#role_source--who-owns-a-users-role).

`auth.oidc.role_source` and `auth.ldap.role_source` are set separately (an
operator may trust one provider's groups and not the other's), and the
`role_editable` field the users API returns is computed per account from
whichever source owns it.

[Keep a local admin account](#keep-a-local-admin-account) applies here too:
if the admin group is renamed at the provider, in `directory` mode every SSO
admin is demoted to `default_role` at their next login, role edits on those
accounts return 409, and nobody is left holding `users.manage`. A local
account is unaffected by a provider-side change.

### Re-authentication and logout are two different things

They answer different problems and are configured independently.

**`reauth_mode`** decides whether the next SSO login may be silent, by
sending `prompt=login` to the provider.

| Value | Behavior |
|---|---|
| `after_logout` (default) | Re-prompt only on the login that follows an explicit logout. |
| `always` | Re-prompt on every login. |
| `never` | Never re-prompt; silent re-login is always permitted. |

This, not `logout_mode`, protects a shared workstation: without it, clearing
sqi's session while the provider still considers the person signed in lets
the next person at that machine sign in as the last one.

**`logout_mode`** decides whether logging out of sqi also ends the session at
the provider.

| Value | Behavior |
|---|---|
| `local` (default) | Clear the sqi session only. |
| `provider` | Also return the provider's RP-initiated logout URL, which the web navigates to. The sqi session ends immediately either way; the provider leg is where the browser goes next. Requires `post_logout_redirect_url`. |

`local` is the default because a provider logout signs the user out of every
company tool that trusts that provider.

Under `provider`, `POST /api/v1/auth/logout` returns
`{"redirect_url": "…"}`; every other logout returns `{}`. If the provider
advertises no `end_session_endpoint`, or discovery is unreachable, or the
advertised endpoint does not parse, sqi falls back to a local logout and
logs at `ERROR`.

sqi does not store ID tokens, so the end-session request uses `client_id` +
`post_logout_redirect_uri` and never `id_token_hint`. The reasons:

- The token would be the only recoverable secret in the schema. Session
  tokens are stored as hashes, passwords as argon2id hashes, so nothing in
  the database can be read back out and used.
- Keycloak accepts an expired ID token as a hint, so a stored token would be
  a session-termination capability that does not decay with the token's
  `exp`.
- The hint works from a client holding no provider cookies, so a leaked
  token can end that user's provider session from anywhere.
- The likely leak path is accidental, such as a debug log line dumping a
  session row, and avoiding it would need a redaction rule every contributor
  remembers.

The cost is stated under
[Provider logout is weaker than it looks](#provider-logout-is-weaker-than-it-looks).

### Limits, stated plainly

#### Revocation lag is the same as LDAP's

Login is the only moment sqi talks to the provider (see
[Revocation lag](#revocation-lag)), so a user disabled or deleted at the
provider keeps their sqi session until it expires (`auth.session.ttl`, 7
days by default). They cannot get a new session; the one they hold keeps
working.

Shorten `auth.session.ttl` if that window matters. To cut someone off
at once, disable the account in sqi (`PATCH /api/v1/users/{id}` with
`disabled: true`): the session authenticator re-checks the user record on every
request, and the local `disabled` flag overrides the provider for both session
checks and login.

#### Role changes apply at next login only

Under `role_source: directory` the role is recomputed from claims at login.
A user moved out of the admin group at the provider keeps `admin` in sqi for
the remaining life of their session. Disabling the account in sqi takes
effect at once.

#### Provider logout is weaker than it looks

On Keycloak (the version `make test-oidc` runs), `logout_mode: provider`
shows a confirmation prompt rather than logging out silently:

- The end-session request carrying `client_id` and `post_logout_redirect_uri`
  with no `id_token_hint` is accepted (HTTP 200, both parameters honored, no
  error).
- The logout does not complete. Keycloak answers with an interactive
  logout-confirmation page, and the provider session stays live behind it.
  The session ends only once a person confirms.

sqi's own session is revoked either way, before the redirect is handed to the
browser, but the option is weaker than its name suggests: `logout_mode:
provider` ends the sqi session and then takes the user to the provider to
confirm signing out everywhere. Someone who leaves the browser at that page
is out of sqi and still signed in to every other tool.

The confirmation page is a security control: an end-session request carrying
only a client identifier could have been constructed by anyone, so Keycloak
asks the person before acting on it. `id_token_hint` proves the caller was
party to the session, and supplying it skips the prompt (Keycloak then
answers `302` straight to the post-logout URI). sqi does not hold that proof;
see the reasons under
[Re-authentication and logout are two different things](#re-authentication-and-logout-are-two-different-things).

Whether Entra ID or Okta completes an end-session request without an
`id_token_hint` is untested; Okta is reported to require the hint. Test your
own provider before relying on `logout_mode: provider`, and treat
`reauth_mode` as the control that protects a shared workstation.

#### `reauth_mode: after_logout` is defeatable

The "this browser logged out" marker is an ordinary cookie. Clearing browser
storage removes it, and the next login is silent again. It protects against
the next colleague at a shared workstation, not against someone with
developer tools open. `reauth_mode: always` re-prompts on every login
regardless of what the browser carries, at the cost of SSO's silent login.

#### A username collision blocks the second user's login

Two identities that map to the same `username_claim` value, or an SSO user
whose name is already taken by a local or LDAP account, cannot both exist.
The second one to log in is refused with the usual 401, and a `WARN` names
the collision in the server log. An admin renames one of the accounts to
resolve it.

sqi does not auto-disambiguate (`alice2`), because the username binds to
`Job.Owner` and must match the provider, and it does not adopt the existing
account, because that would be username matching.

#### Only the open-source providers are covered by CI

`make test-oidc` runs the whole SSO path against a real Keycloak in a
throwaway container, and `make test-ldap` runs the LDAP path against a real
OpenLDAP. Both run in CI on every change. Coverage stops there:

- **Active Directory's `objectGUID` path is not covered by the OpenLDAP
  container.** OpenLDAP is exercised with `entryUUID`; AD's binary `objectGUID`
  and its hex encoding are covered by unit tests against a fake, not by a live
  domain controller.
- **Okta and Entra ID are not covered by the Keycloak container.** Nothing in
  CI contacts either. Claim shapes, group-claim delivery, and end-session
  behavior differ between providers, and those are the places this
  integration is sensitive to.

Both are hosted or licensed products that cannot be provisioned in CI. Test a
new deployment against your own provider before relying on it.

### How this is tested

The unit tests drive a fake identity provider. It signs real tokens, so a
validation mistake surfaces, but a fake returns whatever the test asks for,
so it cannot show what a real provider omits. `test/integration/oidc_test.go`
covers that, driving a browser-shaped flow against a real Keycloak: the authorization-code round trip and group → role
mapping (including the `default_role` downgrade when the groups mapper
is missing), a rename at the provider keeping the same account, state-mismatch
refusal, `prompt=login` forcing re-authentication, and the end-session behavior
recorded above.

```sh
make test-oidc        # needs Docker (or colima/podman); skips cleanly without it
```

See [Testing against a real directory or identity
provider](development.md#testing-against-a-real-directory-or-identity-provider) for the
`SQI_TEST_OIDC_ISSUER` override and why a skip verifies nothing.

## Known gaps

- **Broker authentication is off by default.** See
  [Broker authentication (transport)](#broker-authentication-transport)
  above for the model: the credential, both enrollment paths, revocation, key
  rotation, and the asymmetry that while `nats.auth.enabled` is off, the
  worker ID carried in every subject is unenforced, so the scheduler's
  provenance checks catch faults rather than attackers, and any host that can
  reach the broker's client port (`4222` by default on the embedded broker)
  can register as a worker and receive task assignments. Broker
  authentication does not encrypt the transport: task payloads, assignments
  and log chunks stay cleartext unless TLS is also configured on the broker
  ([`docs/tls.md`](tls.md)). `auth.enabled` does not secure the worker
  transport; the two flags are independent and both must be set.
  `sqi-server` emits a startup WARN when broker auth is off and the broker
  may be reachable from other hosts (an embedded broker on a non-loopback
  address, or any external broker).
- **On an external broker without a system credential, revocation takes
  effect within `nats.auth.user_ttl`.** With
  `nats.external.callout.system_creds_file` unset, `sqi-server` cannot close
  another client's connection on a cluster it does not run, so a revoked
  worker keeps its current connection until its authorization expires
  (within `nats.auth.user_ttl`, 1 hour by default), and only its reconnect is
  refused. `sqi-server` logs a WARN at startup naming that bound. Configure
  the system credential (`sqi-server nats external init` generates one) to
  make external revocation immediate.
- **Broker authentication on an external cluster needs config-file
  accounts.** The auth callout is set up from the accounts fragment
  `sqi-server nats external init` generates. A cluster run in NATS operator
  (JWT) mode can host sqi only with `nats.auth.enabled` off, with
  `sqi-server` connecting through `nats.external.creds_file`.
- **The offline `sqi-server worker revoke` applies at the worker's next
  connect.** It writes the database from a process with no broker handle, so
  it closes no connection: a running worker is refused at its next connect,
  which is within `nats.auth.user_ttl` at the latest. Use
  `DELETE /api/v1/workers/{id}/credential` on a running server to disconnect
  a worker at once.
- **An immediate revocation can still miss one connection, for at most
  `nats.auth.user_ttl`.** `DELETE /api/v1/workers/{id}/credential` closes the
  worker's connections, waits out the broker's auth timeout, and closes them
  again, so an authorization that was already in flight when the revocation
  began is closed too. One that read the database before the revocation was
  written but whose connection the broker placed only after that final pass
  survives: it lasts until its authorization expires, within
  `nats.auth.user_ttl`, and its reconnect is refused.
- **Task isolation's POSIX NSS fallback is not tested against a directory.**
  [Task isolation](#task-isolation) runs job code as a distinct OS user on
  Linux/macOS workers and as a distinct logon session on Windows workers via
  `LogonUserW`, and both are tested against real OS accounts
  (`make test-isolation`, `make test-isolation-windows`). The POSIX NSS path
  for LDAP/AD-backed accounts is tested only against canned command output,
  not a real directory server; see
  [`docs/worker-configuration.md`](worker-configuration.md).
- **`DELETE /workers/{id}/credential` stays mounted with `auth.enabled=false`.**
  Like every permission-gated route, it is open to the anonymous superuser
  when `auth.enabled` is off. With `auth.enabled=false` and
  `nats.auth.enabled=true` (a supported combination), an unauthenticated
  caller can therefore revoke any worker's broker credential, and repeated
  calls can take the whole farm off the broker one worker at a time. This
  affects availability, not privilege: revoking removes access and cannot
  attach new compute or obtain a credential. Every other destructive worker
  route (`disable`, `DELETE /workers/{id}`) is exposed the same way in that
  configuration. The route is not special-cased, so that auth off gates
  nothing.
- **No per-user concurrent task caps.** There is no per-owner ceiling on
  running tasks: `max_concurrent_tasks` on farms and queues caps the farm or
  queue, not the person, so one user can fill a queue's whole allowance.
