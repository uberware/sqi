# sqi-server Configuration Reference

`sqi-server` is configured through four layers applied in order, with later
layers overriding earlier ones:

1. **Built-in defaults** — sensible values for local development.
2. **Config file** — YAML or JSON. With no `--config`, six paths are tried in
   order and only the first that exists is read:
   `./config/sqi-server.yaml`, `./config/sqi-server.json`,
   `~/.sqi/sqi-server.yaml`, `~/.sqi/sqi-server.json`,
   `/etc/sqi/sqi-server.yaml`, `/etc/sqi/sqi-server.json`. Pass an explicit
   path with `--config /path/to/file` (a path that does not exist is an
   error, unlike the search). A Windows service does not search: it refuses
   to start without `--config` (see
   [Windows service](operations.md#windows-service)). A key the server does
   not know, such as a misspelled one, is an error that names its line, and
   so is a second YAML document after a `---` line, which would otherwise go
   unread.
3. **Environment variables** — prefixed `SQI_`, e.g. `SQI_HTTP_ADDR`.
4. **CLI flags** — highest priority. `--config`, `--log-level` and
   `--log-format` are available on every subcommand; the remaining flags
   (`--http-addr`, `--http-cors-origins`, `--openjd-enforce-limits`,
   `--auth-enabled`, `--auth-validate-job-owner`) are on `serve`.

Print the effective merged configuration at any time with:

```sh
sqi-server config print
```

A commented example file is at
[`config/sqi-server.example.yaml`](https://github.com/uberware/sqi/blob/main/config/sqi-server.example.yaml).
It omits the `auth` block and `http.cors_origins`; both are documented below.

Duration values use Go syntax: `30s`, `1m30s`, `500ms`, `2h`, etc.

---

## `http` — REST API and WebSocket listener

### `http.addr`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `"0.0.0.0:8080"` |
| **Env var** | `SQI_HTTP_ADDR` |
| **CLI flag** | `--http-addr` |

TCP address the HTTP server listens on. Use `127.0.0.1:8080` to restrict to
loopback only.

Must be `host:port`. A host given as a name rather than an IP literal is
resolved at startup, so an unresolvable hostname fails config validation and
the server does not start; `0.0.0.0`, `::` and an empty host are accepted
without resolution. The same rule applies to [`nats.embedded.addr`](#natsembeddedaddr).

```yaml
http:
  addr: "0.0.0.0:8080"
```

---

### `http.enable_pprof`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `false` |
| **Env var** | `SQI_HTTP_ENABLE_PPROF` |
| **CLI flag** | *(none — set via config file or env var)* |

Expose Go runtime profiling endpoints at `/debug/pprof/`. Profiling data
reveals memory layout, goroutine stacks, and CPU hotspots, so never enable
this on a server reachable from untrusted networks. Enable it temporarily on a
loopback-only instance for performance diagnosis.

```yaml
http:
  enable_pprof: false
```

---

### `http.rate_limit.enabled`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `true` |
| **Env var** | `SQI_HTTP_RATE_LIMIT_ENABLED` |
| **CLI flag** | *(none — set via config file or env var)* |

Apply the per-IP rate limit to API requests. With it on, a client that sends
requests faster than the limit allows is answered with `429 Too Many Requests`.

Turn it off only behind a trusted network or for load testing. With it off
nothing throttles a client that floods the API, and a load generator driving
the server from one address does not hit the limit.

```yaml
http:
  rate_limit:
    enabled: true
```

---

### `http.cors_origins`

| | |
|---|---|
| **Type** | `[]string` |
| **Default** | `[]` (empty — treated as `["*"]`) |
| **Env var** | `SQI_HTTP_CORS_ORIGINS` (comma-separated) |
| **CLI flag** | `--http-cors-origins` |

Browser origins the CORS middleware allows. This matters only for a
separately-hosted web UI calling this server cross-origin; when `sqi-server`
serves the embedded UI itself, requests are same-origin and need none of this.

Each entry must be `scheme://host[:port]`, or the bare wildcard `"*"`. A
trailing slash, a path, a query, a fragment, or embedded whitespace is
rejected at startup with an `http.cors_origins` validation error, because
go-chi/cors could never match such a value. A wildcard pattern
(`https://*.example.com`, `https://app.example.com*`) is rejected too:
go-chi/cors would treat an embedded `*` as a prefix or suffix match, which
with auth enabled would extend credentialed access to every matching origin.
Name explicit origins.

With `auth.enabled=true` a wildcard is dropped at startup, because browsers
reject `Access-Control-Allow-Credentials` combined with `*`. The server logs a
warning, or an informational line when the wildcard came from the empty-list
default. An empty list means `["*"]` and is dropped the same way, so a
separately-hosted UI must name its origin here for credentialed cross-origin
requests to work. See [auth.md § CSRF & CORS](auth.md#csrf--cors).

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

### `http.tls.enabled`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `false` |
| **Env var** | `SQI_HTTP_TLS_ENABLED` |

Terminate TLS in-process on the REST/WebSocket listener. The listener
upgrades in place, so there is no plaintext port when this is on. A plaintext
request to the TLS port receives a `400`.

Requires [`http.tls.cert_file`](#httptlscert_file) and
[`http.tls.key_file`](#httptlskey_file); the server refuses to start without
both. See [`docs/tls.md`](tls.md) for the full model, including the
coordinated restart that enabling this requires on a running farm.

```yaml
http:
  tls:
    enabled: true
    cert_file: "/etc/sqi/certs/server.crt"
    key_file: "/etc/sqi/certs/server.key"
```

---

### `http.tls.cert_file`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` |
| **Env var** | `SQI_HTTP_TLS_CERT_FILE` |

PEM certificate served on the HTTP listener: leaf first, then any
intermediates. Generate a pair with `sqi-server tls init`.

Validated at startup: a missing file, a key that does not match, or an
already-expired certificate is a startup error naming this key. A
certificate expiring within 30 days logs a WARN. Setting
this while `http.tls.enabled` is `false` logs a WARN that the server is
serving plaintext.

---

### `http.tls.key_file`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` |
| **Env var** | `SQI_HTTP_TLS_KEY_FILE` |

PEM private key matching [`http.tls.cert_file`](#httptlscert_file). Should be
mode `0600`; `sqi-server tls init` writes it that way.

Certificates are read once at startup, so rotating them requires a restart.

---

## `nats` — NATS JetStream broker

sqi-server and its workers talk through a NATS JetStream broker.
[`nats.mode`](#natsmode) chooses where it runs:

- **`embedded`** (the default): a broker inside `sqi-server`. Nothing to
  install or configure; this is the zero-config single-binary farm. Its
  settings are under `nats.embedded`.
- **`external`**: a NATS server or cluster you run. `sqi-server` connects to
  it, provisions its streams there, and keeps running when one node of the
  cluster is lost. Its settings are under `nats.external`. Setting one up is
  walked through in
  [Running on an external NATS cluster](operations.md#running-on-an-external-nats-cluster).

`nats.auth` applies in both modes. The `nats.embedded` block is ignored in
external mode and `nats.external` in embedded mode.

In both modes sqi keeps its streams, consumers and workers in one NATS
account that nothing else uses (`SQI`, or
[`nats.external.account`](#natsexternalaccount)), and its auth-callout
connection in a second account, `AUTH`. See
[Broker topologies](architecture.md#broker-topologies).

The embedded broker's `addr`, `data_dir`, `max_store_mb` and `tls` keys
belong under `nats.embedded`. A configuration file with any of them directly
under `nats` fails at startup with an error naming the `nats.embedded` key.
Likewise a set `SQI_NATS_ADDR`, `SQI_NATS_DATA_DIR`, `SQI_NATS_MAX_STORE_MB`
or `SQI_NATS_TLS_*` variable fails startup with an error naming its
`SQI_NATS_EMBEDDED_*` equivalent.

### `nats.mode`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `"embedded"` |
| **Accepted** | `embedded`, `external` |
| **Env var** | `SQI_NATS_MODE` |

`embedded` runs the broker inside `sqi-server`. `external` connects to the
broker listed in [`nats.external.urls`](#natsexternalurls). Changing the mode
of a farm that has run is a stop-the-farm operation; see
[Switching broker mode](operations.md#switching-broker-mode).

```yaml
nats:
  mode: external
```

---

### `nats.auth.enabled`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `false` |
| **Env var** | `SQI_NATS_AUTH_ENABLED` |

Requires every worker to present a per-worker nkey credential. When `true`,
the broker sends each connection to `sqi-server` through the NATS auth
callout, and `sqi-server` admits only a key enrolled in its database. When
`false` (the default), the broker accepts any anonymous connection and refuses
one that presents a key, so a worker must not hold a credential file on an
auth-off farm; see the warning under
[`nats.embedded.addr`](#natsembeddedaddr), and the order for turning it on
under [Broker authentication](auth.md#getting-a-credential). Independent of
`auth.enabled`, which gates human users and API keys on the REST API; this
setting gates workers on the broker.

The same setting works in both modes. In embedded mode it needs nothing else.
In external mode it also needs the
[`nats.external.callout`](#natsexternalcalloutnkey_seed_file) keys, which
`sqi-server nats external init` generates. See
[Broker authentication](auth.md#broker-authentication-transport).

```yaml
nats:
  auth:
    enabled: true
```

---

### `nats.auth.user_ttl`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `1h` |
| **Range** | `1m` – `24h` |
| **Env var** | `SQI_NATS_AUTH_USER_TTL` |

How long the authorization `sqi-server` issues a worker at connect stays
valid. At expiry the broker disconnects the worker, which reconnects at once
and is authorized again against the database. A worker revoked in the
meantime is refused at that reconnect.

This is the bound on revocation wherever `sqi-server` cannot disconnect a
worker itself: an [external broker without a system
credential](#natsexternalcalloutsystem_creds_file), and the offline
`sqi-server worker revoke` command. Each expiry costs every worker one
reconnect, so a shorter value means more reconnects. For prompt revocation on
an external broker, configure the system credential instead of shortening
this. Validated whether or not `nats.auth.enabled` is on.

```yaml
nats:
  auth:
    user_ttl: 30m
```

---

### `nats.auth.join_token_ttl`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `1h` |
| **Range** | `1m` – `24h` |
| **Env var** | `SQI_NATS_AUTH_JOIN_TOKEN_TTL` |

How long a newly issued worker join token remains valid. Below a minute an
operator cannot realistically get the token onto a machine and boot it; the
ceiling keeps a token, which mints a worker credential, from becoming a
long-lived secret. Ignored while `nats.auth.enabled` is `false`.

A [capacity group's token](capacity.md#sized-groups-with-broker-authentication-on)
has a lifetime of its own, chosen when it is minted (30 days by default, 90 at
most), and is unaffected by this key. Capacity groups themselves are managed
through the REST API and the Admin hub and have no server configuration keys.

```yaml
nats:
  auth:
    join_token_ttl: 30m
```

---

### `nats.auth.join_token_single_use`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `true` |
| **Env var** | `SQI_NATS_AUTH_JOIN_TOKEN_SINGLE_USE` |

Consumes a join token on first successful enrollment. Leaving this `true` is
strongly recommended; set it `false` only for image-baked fleets that enroll
many identical machines from one token.

```yaml
nats:
  auth:
    join_token_single_use: false
```

---

### `nats.auth.enrollment_endpoint_enabled`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `true` |
| **Env var** | `SQI_NATS_AUTH_ENROLLMENT_ENDPOINT_ENABLED` |

Mounts `POST /api/v1/workers/enroll`. Meaningful only when
`nats.auth.enabled` is `true`. Set `false` at a site that provisions every
worker credential by hand and wants no enrollment surface at all.

```yaml
nats:
  auth:
    enrollment_endpoint_enabled: false
```

---

### `nats.embedded.addr`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `"0.0.0.0:4222"` |
| **Env var** | `SQI_NATS_EMBEDDED_ADDR` |

TCP address the embedded broker binds to. Defaults to all interfaces so that
workers which discover the server over mDNS can connect to the broker at the
advertised LAN host. Set this to `"127.0.0.1:4222"` to restrict the broker to
loopback (single-machine only). `sqi-server`'s own connections to the
embedded broker run in-process and never use this listener.

Broker authentication is a separate opt-in setting, independent of
`auth.enabled`; see [`nats.auth.enabled`](#natsauthenabled). While it is off
(the default), any host that can reach this port can register as a worker and
receive task assignments, whatever `auth.enabled` says, and `sqi-server` logs
a startup WARN when this address is not loopback. See
[Known gaps](auth.md#known-gaps).

```yaml
nats:
  embedded:
    addr: "0.0.0.0:4222"
```

---

### `nats.embedded.advertise_url`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` |
| **Env var** | `SQI_NATS_EMBEDDED_ADVERTISE_URL` |

The broker URL that mDNS discovery tells workers to use, in the `nats_url`
record. Empty derives `nats://<host>.local:<port>` (`tls://` when
[`nats.embedded.tls.enabled`](#natsembeddedtlsenabled) is on) from this
machine's mDNS host name and the port the broker bound. Set it when workers
must reach the broker by another name or address, such as a DNS name the
broker certificate covers.

Validated at load: it must parse as `nats://host:port` or `tls://host:port`,
carry no credentials (`user:password@` or `token@`, which mDNS would publish
in plaintext), not name port 0, its scheme must match
`nats.embedded.tls.enabled` (`tls://` when TLS is on, `nats://` when it is
off), and `nats_url=` plus the URL must fit the 255-byte limit of one DNS TXT
string. Validated even when
[`discovery.enabled`](#discoveryenabled) is off; used only when it is on.

```yaml
nats:
  embedded:
    advertise_url: "tls://sqi-server.example:4222"
```

---

### `nats.embedded.data_dir`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `"data/nats"` |
| **Env var** | `SQI_NATS_EMBEDDED_DATA_DIR` |

Directory used by JetStream for file-backed stream persistence. Created at
startup if it does not exist. Relative paths are resolved from the working
directory at the time `sqi-server` starts. For production, use an absolute path
on fast local storage.

It holds only messages in transit, so it is not part of a backup;
`sqi-server restore` renames it to `<dir>.pre-restore-<time>` instead (see
[Restore](operations.md#restore)).

```yaml
nats:
  embedded:
    data_dir: "/var/lib/sqi/nats"
```

---

### `nats.embedded.max_store_mb`

| | |
|---|---|
| **Type** | `int` (megabytes) |
| **Default** | `1024` |
| **Env var** | `SQI_NATS_EMBEDDED_MAX_STORE_MB` |

Maximum disk space JetStream may use, for sqi's account and for the broker as
a whole. Messages leave sqi's streams by age (the log stream keeps chunks for
96 hours); when the cap is reached first, the broker refuses new messages
until space frees. Increase this on farms with many active jobs or high log
volume. An external broker's storage limits are set on its account by the
cluster's operator instead.

```yaml
nats:
  embedded:
    max_store_mb: 4096
```

---

### `nats.embedded.tls.enabled`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `false` |
| **Env var** | `SQI_NATS_EMBEDDED_TLS_ENABLED` |

Require TLS on every connection to the embedded broker.

This encrypts the worker transport: task assignments (command lines, embedded
file contents, parameters, environment, the isolation username), status
messages and log chunks. Broker authentication
([`nats.auth.enabled`](#natsauthenabled)) does not encrypt anything; the two
settings are independent and address different threats, and a production farm
should enable both.

Requires [`nats.embedded.tls.cert_file`](#natsembeddedtlscert_file) and
[`nats.embedded.tls.key_file`](#natsembeddedtlskey_file). Workers must be
configured with the matching CA (the worker key `nats.tls_ca_file`); see
[`docs/tls.md`](tls.md).

```yaml
nats:
  embedded:
    tls:
      enabled: true
      cert_file: "/etc/sqi/certs/server.crt"
      key_file: "/etc/sqi/certs/server.key"
```

---

### `nats.embedded.tls.cert_file`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` |
| **Env var** | `SQI_NATS_EMBEDDED_TLS_CERT_FILE` |

PEM certificate the broker presents to connecting workers. May be the same
file as [`http.tls.cert_file`](#httptlscert_file) or a different one: the two
listeners are configured independently, so an operator can serve the API with
a publicly-issued certificate and the broker with a private farm CA.

Validated at startup on the same terms as `http.tls.cert_file`.

---

### `nats.embedded.tls.key_file`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` |
| **Env var** | `SQI_NATS_EMBEDDED_TLS_KEY_FILE` |

PEM private key matching [`nats.embedded.tls.cert_file`](#natsembeddedtlscert_file).

---

### `nats.embedded.tls.client_ca_file`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` |
| **Env var** | `SQI_NATS_EMBEDDED_TLS_CLIENT_CA_FILE` |

When set, every connecting worker must present a client certificate signed by
this CA (mutual TLS). Issue per-worker certificates with
`sqi-server tls init --client <worker-id>`.

This adds to the per-worker nkey credential and does not replace it. The
certificate gates who may open a connection; the nkey identifies the worker and
drives subject permissions and revocation. A valid farm certificate on its own
authenticates nobody, and sqi does not bind a certificate to a worker ID.

Requires [`nats.embedded.tls.enabled`](#natsembeddedtlsenabled). Setting this
on a plaintext broker is a startup error, since client certificates cannot be
verified without a TLS handshake.

---

### `nats.external.urls`

| | |
|---|---|
| **Type** | `[]string` |
| **Default** | `[]` |
| **Env var** | `SQI_NATS_EXTERNAL_URLS` (comma-separated) |

The broker's client URLs. Required in external mode. List every node of a
cluster: `sqi-server` connects to one and moves to another when it is lost.
Every URL must use the same scheme, all `tls://` or all `nats://`. With
`nats://` URLs, no [`nats.external.tls.*`](#natsexternaltlsca_file) key may be
set.

At startup `sqi-server` keeps trying every URL until
[`nats.external.connect_timeout`](#natsexternalconnect_timeout), then fails
naming the URLs it tried and the last error. It refuses a broker older than
the [supported nats-server versions](operations.md#supported-nats-server-versions).
After startup it reconnects without limit. URLs are logged, and printed by
`sqi-server config print`, with any credentials (`user:password@` or
`token@`) replaced by `REDACTED`.

With [`advertise_urls`](#natsexternaladvertise_urls) empty and
[`discovery.enabled`](#discoveryenabled) on, discovery advertises the first
entry, so it is validated as an advertised URL: no credentials, not port 0,
and `nats_url=` plus the URL within 255 bytes. To keep credentials in `urls`,
set a credential-free `advertise_urls`.

```yaml
nats:
  mode: external
  external:
    urls: ["tls://nats-a:4222", "tls://nats-b:4222", "tls://nats-c:4222"]
```

---

### `nats.external.advertise_urls`

| | |
|---|---|
| **Type** | `[]string` |
| **Default** | `[]` (use `urls`) |
| **Env var** | `SQI_NATS_EXTERNAL_ADVERTISE_URLS` (comma-separated) |

The broker URLs mDNS discovery tells workers to use, when they must reach the
cluster at different addresses than `sqi-server` does (a load balancer in
front of it, say). Discovery advertises only the first entry, in the
`nats_url` record; empty advertises the first entry of `urls`. A worker that
should know every node lists them in its own `nats.url` instead.

Validated at load, even when [`discovery.enabled`](#discoveryenabled) is
off: every entry must parse as `nats://host:port` or `tls://host:port` with
the same scheme as `urls`, carry no credentials (`user:password@` or
`token@`, which mDNS would publish in plaintext), and not name port 0, and
`nats_url=` plus the first entry must fit the 255-byte limit of one DNS TXT
string.

---

### `nats.external.account`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `"SQI"` |
| **Env var** | `SQI_NATS_EXTERNAL_ACCOUNT` |

The NATS account sqi's streams, consumers, guard bucket and workers live in.
It must be dedicated to sqi: `sqi-server`'s data connection may publish and
subscribe to everything in it, which is safe only because nothing else shares
the account. Letters, digits, `_` and `-` only; `AUTH`, `SYS` and `$G` are
refused, because the generated cluster configuration defines the first two
and the third is nats-server's global account. Pass the same name to
`sqi-server nats external init --account`.

Only one `sqi-server` can run on an account at a time; see
[One server per broker account](operations.md#one-server-per-broker-account).

---

### `nats.external.nkey_seed_file`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` |
| **Env var** | `SQI_NATS_EXTERNAL_NKEY_SEED_FILE` |

The nkey seed `sqi-server`'s data connection authenticates with: the
`sqi-data.nk` file `sqi-server nats external init` writes. Its public key must
be a user in [`nats.external.account`](#natsexternalaccount), and, with broker
authentication on, listed in the callout's `auth_users`, which the generated
fragment does. Mutually exclusive with
[`nats.external.creds_file`](#natsexternalcreds_file). Leaving both empty
connects with no credential, which works only on a broker that accepts
anonymous connections into the account. The file must exist and be readable at
startup.

---

### `nats.external.creds_file`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` |
| **Env var** | `SQI_NATS_EXTERNAL_CREDS_FILE` |

A NATS `.creds` file (user JWT plus seed) for the data connection, for a
cluster that runs in NATS operator (JWT) mode, with broker authentication
off: sqi's auth callout is supported only on clusters configured with
config-file accounts. Mutually
exclusive with [`nats.external.nkey_seed_file`](#natsexternalnkey_seed_file).

---

### `nats.external.stream_replicas`

| | |
|---|---|
| **Type** | `int` |
| **Default** | `1` |
| **Accepted** | `1`, `3`, `5` |
| **Env var** | `SQI_NATS_EXTERNAL_STREAM_REPLICAS` |

How many copies the cluster keeps of each of sqi's streams and of the guard
bucket. With `3`, a three-node cluster loses no message when one node is lost.
A change on an existing stream is applied at the next start and logged at
INFO with the old and new values; lowering it is allowed and logged with what
it costs. At startup `sqi-server` waits, up to
[`nats.external.connect_timeout`](#natsexternalconnect_timeout), for the
cluster to have enough nodes to place the streams. The embedded broker always
uses `1`.

---

### `nats.external.connect_timeout`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `30s` |
| **Env var** | `SQI_NATS_EXTERNAL_CONNECT_TIMEOUT` |

How long startup may take to reach a broker, find JetStream on the account and
provision the streams. After startup, reconnects are unbounded. Also bounds
the broker steps of `sqi-server restore`.

---

### `nats.external.tls.ca_file`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` |
| **Env var** | `SQI_NATS_EXTERNAL_TLS_CA_FILE` |

PEM CA bundle that verifies the cluster's certificate, in place of the system
roots. Empty, with `tls://` URLs, verifies against the system roots. Workers
need the same CA in their `nats.tls_ca_file`.

### `nats.external.tls.cert_file` / `nats.external.tls.key_file`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` |
| **Env var** | `SQI_NATS_EXTERNAL_TLS_CERT_FILE`, `SQI_NATS_EXTERNAL_TLS_KEY_FILE` |

A client certificate and its key, for a cluster that requires mutual TLS. Set
both or neither. A certificate expiring within 30 days logs a WARN at startup.

### `nats.external.tls.server_name`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` |
| **Env var** | `SQI_NATS_EXTERNAL_TLS_SERVER_NAME` |

The name the cluster's certificate is verified against, when it is not the
host in the URL being dialed.

---

### `nats.external.callout.nkey_seed_file`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` |
| **Env var** | `SQI_NATS_EXTERNAL_CALLOUT_NKEY_SEED_FILE` |

The nkey seed of `sqi-server`'s auth-callout connection, a user in the `AUTH`
account (`sqi-auth.nk` from `sqi-server nats external init`). Required in
external mode when [`nats.auth.enabled`](#natsauthenabled) is `true`.

### `nats.external.callout.issuer_seed_file`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` |
| **Env var** | `SQI_NATS_EXTERNAL_CALLOUT_ISSUER_SEED_FILE` |

The account nkey seed (`SA…`) `sqi-server` signs its authorization answers
with (`issuer.nk`). The cluster's `auth_callout` block names its public key as
the issuer. Required in external mode when `nats.auth.enabled` is `true`. A
user seed (`SU…`) here is the usual mistake, and the startup error says so.

### `nats.external.callout.system_creds_file`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` |
| **Env var** | `SQI_NATS_EXTERNAL_CALLOUT_SYSTEM_CREDS_FILE` |

Optional. The nkey seed of a user in the cluster's system account
(`sqi-sys.nk`). Despite the key's name it is a raw nkey seed, not a `.creds`
file: with config-file accounts nats-server authenticates nkey users by their
signature alone. With it, revoking a worker disconnects it from every node at
once. Without it, a revoked worker stays connected until its authorization
expires, within [`nats.auth.user_ttl`](#natsauthuser_ttl), and `sqi-server`
logs a WARN at startup saying so. See
[Revocation](auth.md#revocation-two-paths-deliberately-distinct).

At startup `sqi-server` asks every node through this connection for its auth
timeout; a seed that is not a system-account user fails startup naming
`system_creds_file`.

---

## `store` — State store (SQLite or PostgreSQL)

The state store holds every job, task, worker, farm, queue, usage pool,
account and audit entry. It is embedded SQLite by default, or an external
PostgreSQL server (16 or later) when `store.driver` is `postgres`. Creating
the database, the DSN, backups and the one-server rule are covered in
[Running on PostgreSQL](operations.md#running-on-postgresql).

### `store.driver`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `"sqlite"` |
| **Env var** | `SQI_STORE_DRIVER` |

Which store to use: `sqlite` (embedded, a single file) or `postgres` (an
external PostgreSQL 16 or later). Any other value fails validation at
start-up.

```yaml
store:
  driver: postgres
  postgres:
    dsn: "postgres://sqi@db.example.com:5432/sqi?sslmode=verify-full"
```

On `postgres`, `store.sqlite_path` and `store.checkpoint_interval` are
ignored, and the server logs a start-up warning for each one set explicitly
in the config file or the environment (`store.sqlite_path is ignored when
store.driver is postgres`).

> The `migrate`, `backup`, `restore` and `worker` subcommands choose their
> store from the same configuration, and take flags that override it. Highest
> priority first: an explicit `--dsn` (a PostgreSQL database), then an explicit `--db`
> (a SQLite file), then `store.driver` from the resolved configuration (the
> root `-c/--config` file and `SQI_STORE_DRIVER`), with `store.postgres.dsn`
> for PostgreSQL and the [`store.sqlite_path`](#storesqlite_path) layering
> for SQLite. A `store.driver` other than `sqlite` or `postgres` is an error.
> Without `-c`, a configuration that fails to load is normally a warning and
> the command falls back to SQLite; when `SQI_STORE_DRIVER` names anything
> but `sqlite`, it is an error instead. `restore` and `store copy` never fall
> back: they refuse unless every target is named on the command line.

---

### `store.sqlite_path`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `"sqi.db"` |
| **Env var** | `SQI_STORE_SQLITE_PATH` |

Path to the SQLite database file, as a file path (a `file:` URI is refused).
Created at startup if it does not exist. A symlink in the path is resolved:
the server and every command (`backup`, `restore`, `store copy`, `migrate`)
work on the file it names, so `restore` through a symlink replaces the
database and leaves the link in place.
Schema migrations run automatically at startup. For production, use an absolute
path on a local SSD. Ignored when `store.driver` is `postgres`, except as the
default source of [`sqi-server store copy`](operations.md#moving-to-postgresql).

The server also creates `<sqlite_path>.lock` beside the database (for example
`sqi.db.lock`; with symlinks resolved, so the lock file sits beside the real
database file whichever name you give it) and holds an operating-system lock
on it while it runs, so a second server on the same file is refused;
`migrate up`, `restore` and `store copy` take it too. Leave the file in place.
The guard is the lock held on it, which the operating system releases when
the server exits or crashes, so a leftover file is never stale. On a network
filesystem the lock is only as reliable as that filesystem's locking.
See [One server per database](operations.md#one-server-per-database).

```yaml
store:
  sqlite_path: "/var/lib/sqi/sqi.db"
```

> The `migrate`, `backup`, `restore`, `store copy` and `worker` subcommands
> resolve their database path the same way the server does. Highest
> priority first: an explicit `--db` flag (`--from` for `store copy`); this
> key (the root `-c/--config` file and
> `SQI_STORE_SQLITE_PATH`); the legacy `SQI_SQLITE_PATH` environment
> variable, which still works but prints a deprecation notice to stderr when
> it is what decided the path; and finally the built-in `"sqi.db"` default.
> `migrate up` creates the database when it does not already exist. `backup`
> and `worker` (`sqi-server worker token issue|enroll|revoke|list`; see
> [`docs/operations.md`](operations.md#worker-broker-credentials)) never
> create one: pointing either at a database that does not exist is an error
> naming the resolved path.

---

### `store.checkpoint_interval`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `"5m"` |
| **Env var** | `SQI_STORE_CHECKPOINT_INTERVAL` |

How often the background goroutine runs `PRAGMA wal_checkpoint(PASSIVE)` to
fold committed WAL frames back into the main database file. Without periodic
checkpointing the WAL grows unboundedly under write load. A final checkpoint
always runs on clean shutdown regardless of this setting, and that one uses
`TRUNCATE` so the WAL is left at zero bytes.

The periodic checkpoint is `PASSIVE` because it never waits on a reader: it
moves what it can and the next tick takes the rest. A `TRUNCATE` checkpoint
blocked by a reader's WAL snapshot would hold the single write connection for
the 5s `busy_timeout`, stalling job submission and task-status writes.

Set to a large value (e.g. `"24h"`) to disable periodic checkpointing while
keeping the shutdown checkpoint. Must be `> 0`. Ignored when `store.driver` is
`postgres`.

```yaml
store:
  checkpoint_interval: "10m"
```

---

### `store.postgres.dsn`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` |
| **Env var** | `SQI_STORE_POSTGRES_DSN` |

The PostgreSQL connection string. Required when `store.driver` is `postgres`
(an empty one fails validation); unused otherwise. It may be a URL or a
keyword/value string:

```yaml
store:
  postgres:
    # URL form
    dsn: "postgres://sqi:secret@db.example.com:5432/sqi?sslmode=verify-full"
    # keyword/value form
    # dsn: "host=db.example.com port=5432 user=sqi password=secret dbname=sqi sslmode=verify-full"
```

Set `sslmode` explicitly. `verify-full` is the one that checks the server's
certificate and host name; the driver's default, `prefer`, falls back to an
unencrypted connection without saying so. The password may also come from
`PGPASSWORD` or a password file, as with any libpq client, which keeps it out
of the configuration altogether.

The password is never printed: `sqi-server config print` and the server's
start-up log replace `password` (and `sslpassword`) with `redacted` in a URL
and `<redacted>` in a keyword/value DSN. A keyword/value DSN is parsed the way
the driver parses it, so it may print with different spacing or quoting but
the same settings; one that cannot be parsed prints as `<redacted>`. A URL the
redaction cannot read exactly as the driver does prints as its scheme and a
placeholder (`postgres://<redacted>`) and nothing more. That covers a URL with
more than one `@`, or an `@` that does not end the user name (one in a query
value, say); any `#`; any `;` in the query; a `password` key that is
percent-encoded or padded with spaces; a query pair without `=`; or a query
value holding a second `=`. In a URL, percent-encode any `@` or `#` (in the
password too) and any `;` or `=` inside a query value (`%40`, `%23`, `%3B`,
`%3D`); otherwise `config print` hides the whole DSN and
[`sqi-server backup`](operations.md#backing-up-postgresql) refuses it.

---

### `store.postgres.max_conns`

| | |
|---|---|
| **Type** | `int` |
| **Default** | `16` |
| **Env var** | `SQI_STORE_POSTGRES_MAX_CONNS` |

The size of the server's write connection pool, from 2 to 1000; a value
outside that range fails validation. One of these connections is held for the
server's whole life by its [single-server
lock](operations.md#one-server-per-database), so the default leaves 15 for
writes. The server also opens a read pool of 4 connections, so it can hold up
to `max_conns + 4` connections in all: keep that within the PostgreSQL
server's `max_connections`, alongside everything else that connects to it.
Unused when `store.driver` is `sqlite`, whose write pool is always a single
connection.

```yaml
store:
  postgres:
    max_conns: 32
```

---

## `log` — Structured logging

### `log.level`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `"info"` |
| **Accepted values** | `debug`, `info`, `warn`, `error` |
| **Env var** | `SQI_LOG_LEVEL` |
| **CLI flag** | `--log-level` |

Minimum log level to emit. `debug` adds verbose request tracing and
scheduler internals, which is noisy in production.

```yaml
log:
  level: "info"
```

---

### `log.format`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `"json"` |
| **Accepted values** | `json`, `text` |
| **Env var** | `SQI_LOG_FORMAT` |
| **CLI flag** | `--log-format` |

Log output format. `json` is structured and machine-parseable; use it in
production so log aggregators (Loki, Datadog, Splunk, etc.) can index fields.
`text` is human-readable, for local development.

```yaml
log:
  format: "json"
```

---

### `log.file`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` (log to stderr) |
| **Env var** | `SQI_LOG_FILE` |
| **CLI flag** | — |

Path of a file to write log output to instead of stderr. The file is rotated by
size (see `log.max_size_mb` and `log.max_backups`). Relative paths resolve
against the working directory; a Windows service runs in the directory that
holds its config file. Empty keeps logging on stderr.

The directory must already exist: sqi-server does not create it, and startup
fails with a `log.file` validation error naming the missing directory. The path
must name a file: one that ends with a path separator, or names an existing
directory, fails validation too.

When `sqi-server` runs as a Windows service and `log.file` is empty, logs go to
`%ProgramData%\sqi\logs\<service-name>.log` instead, since a service has no
stderr to write to. That default directory is created for you.

```yaml
log:
  file: "D:\\sqi-logs\\sqi-server.log"
```

---

### `log.max_size_mb`

| | |
|---|---|
| **Type** | `int` |
| **Default** | `100` |
| **Env var** | `SQI_LOG_MAX_SIZE_MB` |
| **CLI flag** | — |

Size in megabytes at which `log.file` is rotated. Must be `> 0`. Has no effect
while `log.file` is empty and the server is not running as a Windows service.

```yaml
log:
  max_size_mb: 100
```

---

### `log.max_backups`

| | |
|---|---|
| **Type** | `int` |
| **Default** | `5` |
| **Env var** | `SQI_LOG_MAX_BACKUPS` |
| **CLI flag** | — |

How many rotated files (`<file>.1` through `<file>.N`) are kept. `0` keeps
none: the file is discarded and started fresh when it reaches
`log.max_size_mb`. Must be `>= 0`.

```yaml
log:
  max_backups: 5
```

---

## `scheduler` — Task assignment loop

### `scheduler.heartbeat_timeout`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `"30s"` |
| **Env var** | `SQI_SCHEDULER_HEARTBEAT_TIMEOUT` |

Duration after which a worker that has not sent a heartbeat is declared offline.
Its in-flight tasks are reclaimed and re-queued for assignment. For high-latency
networks or heavily loaded worker hosts, increase to `60s` or more. Must be
`> 0`.

```yaml
scheduler:
  heartbeat_timeout: "45s"
```

---

### `scheduler.tick_interval`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `"500ms"` |
| **Env var** | `SQI_SCHEDULER_TICK_INTERVAL` |

> **Reserved.** This key is parsed and validated (it must be `> 0`), but the
> scheduler does not read it: the assignment loop runs on a fixed 1s interval
> (`AssignInterval`), so setting it has no effect.

Reserved meaning: how often the assignment loop wakes to match ready tasks to
idle workers.

```yaml
scheduler:
  tick_interval: "500ms"
```

---

### `scheduler.max_tasks_per_worker`

| | |
|---|---|
| **Type** | `int` |
| **Default** | `1` |
| **Env var** | `SQI_SCHEDULER_MAX_TASKS_PER_WORKER` |

> **Reserved.** This key is parsed and validated (it must be `≥ 1`), but the
> scheduler does not read it: task concurrency on a worker is governed by
> CPU-core commitment (`CPUCount − committed`), so setting it has no effect.

Reserved meaning: the maximum number of tasks simultaneously assigned to a
single worker.

```yaml
scheduler:
  max_tasks_per_worker: 1
```

---

### `scheduler.offline_worker_retention`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `24h` |
| **Env var** | `SQI_SCHEDULER_OFFLINE_WORKER_RETENTION` |

How long a worker may remain offline before the retention sweep hard-deletes its
record, bounding the growth of the worker list on farms with ephemeral nodes
(e.g. cloud workers that spin up for a burst of work and are then destroyed). The
sweep runs on the heartbeat-sweep tick and only removes workers in the `offline`
state; `online` and administratively `disabled` workers are never auto-removed,
and neither is a member of a named [capacity group](capacity.md). An ephemeral
capacity-group member (a cloud machine that joined by group token) is removed
much sooner by the capacity sweep, after the group's `boot_timeout_seconds`, with
its credential revoked. A worker that reconnects after removal re-registers. Set to `0` to disable
automatic removal (workers can still be removed manually from the web UI).

```yaml
scheduler:
  offline_worker_retention: "24h"
```

---

### `scheduler.job_retention`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `168h` (7 days) |
| **Env var** | `SQI_SCHEDULER_JOB_RETENTION` |

How long a terminal job is retained before the retention sweep hard-deletes it
and all of its data (steps, tasks, attempts, logs). The sweep runs on the
heartbeat-sweep tick and removes completed and canceled jobs whose completion
time is older than this window; failed jobs are governed by
`job_retention_include_failed`. Active jobs are never auto-deleted. Set to `0`
to disable automatic deletion (jobs can still be deleted manually).

```yaml
scheduler:
  job_retention: "168h"
```

---

### `scheduler.job_retention_include_failed`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `false` |
| **Env var** | `SQI_SCHEDULER_JOB_RETENTION_INCLUDE_FAILED` |

When `true`, the retention sweep also removes failed jobs older than
`job_retention`. Default keeps failed jobs for post-mortem debugging. No effect
when `job_retention` is `0`.

```yaml
scheduler:
  job_retention_include_failed: false
```

---

### `scheduler.unschedulable_grace`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `30s` |
| **Env var** | `SQI_SCHEDULER_UNSCHEDULABLE_GRACE` |

How long a `ready` task may wait with no eligible online worker before it is
flagged "unschedulable", which the API and UI surface so operators can spot
stuck work. Set to `0` to disable the sweep.

```yaml
scheduler:
  unschedulable_grace: "30s"
```

See
[Why isn't my job running? — Unschedulable tasks](observability.md#why-isnt-my-job-running--unschedulable-tasks)
for what the flag means, where it surfaces (task `unschedulable_reason`, job
`task_counts.unschedulable`, the job-detail badge), and how it clears.

---

### `scheduler.reconcile_grace`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `2m`, or `scheduler.heartbeat_timeout` when that is longer (only when this key is not set) |
| **Env var** | `SQI_SCHEDULER_RECONCILE_GRACE` |

How long a live worker may go without listing, in its heartbeats, an attempt
the store says it holds before the heartbeat sweep reclaims that attempt: the
task goes back to `ready` and runs again, with no retry used. This recovers a
task whose completion report was lost or whose database was restored by hand.
Nothing is reclaimed during the first `reconcile_grace` after the server
starts. Each reclaim is logged at Warn and counted in
`sqi_scheduler_tasks_reconciled_total`.

The same sweep sends a cancel to a worker whose heartbeats still list an
attempt the store has closed (a cancel the worker missed, or a task fenced by
a restore), on every sweep while they do.

`0` disables reconciliation, including that cancel. Any other value must be at
least `scheduler.heartbeat_timeout`, or start-up fails validation. When the key is
set neither in the file nor in the environment, the default follows a raised
`heartbeat_timeout`; set explicitly, it is used exactly as given.

If the server falls more than `reconcile_grace` behind in processing status
reports, a task that has already finished can be reclaimed and run again
(never lost or recorded twice); raise this value if
`sqi_scheduler_tasks_reconciled_total` climbs while no worker was lost.
Workers from v0.3.0 and earlier do not report their attempts and are never
reconciled. See [Reconciliation](operations.md#reconciliation).

```yaml
scheduler:
  reconcile_grace: "2m"
```

---

## Retry & failure limits

Worker-reported task failures auto-retry with backoff up to a per-task attempt
ceiling, and a job auto-parks (pauses) once its cumulative genuine failures
reach a failure limit. Three server keys set the farm-wide defaults for this
policy.

### `scheduler.default_max_attempts`

| | |
|---|---|
| **Type** | `int` |
| **Default** | `3` |
| **Env var** | `SQI_SCHEDULER_DEFAULT_MAX_ATTEMPTS` |

Farm-wide default number of genuine attempts a task may make before it goes
terminal-`failed`. Must be `≥ 1`; `1` disables auto-retry (a single failure is
immediately terminal).

```yaml
scheduler:
  default_max_attempts: 3
```

### `scheduler.retry_delay`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `"30s"` |
| **Env var** | `SQI_SCHEDULER_RETRY_DELAY` |

Backoff applied before a failed task re-enters the `ready` queue for
re-lease. `0` means immediate re-queue.

```yaml
scheduler:
  retry_delay: "30s"
```

### `scheduler.default_failure_limit`

| | |
|---|---|
| **Type** | `int` |
| **Default** | `0` |
| **Env var** | `SQI_SCHEDULER_DEFAULT_FAILURE_LIMIT` |

Farm-wide default ceiling on a job's cumulative genuine task failures. Once
reached, the job is auto-parked (`status=paused`, with a `park_reason` such as
`"failure limit reached (N)"`). `0` disables the limit, so a job never
auto-parks on failure count alone.

```yaml
scheduler:
  default_failure_limit: 0
```

### Precedence: Server → Farm → Queue → Job

The effective value for each of the three knobs above is resolved
independently as the first non-null of, in order: Job → Queue → Farm →
server default. A farm sets a studio-wide policy, a queue can narrow it for
a class of work, and a job can override it for one submission. Any tier left
unset (`null`) falls through to the next.

Farm and queue overrides are set via the farm/queue REST resources'
`max_attempts` / `retry_delay_seconds` / `failure_limit` fields (and the
matching Python SDK keyword arguments on `create_farm`/`update_farm` and
`create_queue`/`update_queue`). A job's overrides use the same three names,
but on `POST /api/v1/jobs` they are query parameters, alongside `priority`,
because the body is the raw OpenJD template:

```sh
curl -X POST "http://localhost:8080/api/v1/jobs?max_attempts=5&retry_delay_seconds=10&failure_limit=20" \
  -H 'Content-Type: application/yaml' \
  --data-binary @job.yaml
```

Overrides are validated at the API boundary: `max_attempts` must be >= 1,
`retry_delay_seconds` and `failure_limit` must be >= 0 (an explicit
`failure_limit` of 0 disables an inherited limit). Out-of-range or
non-integer values are rejected with `400`. The job detail response
(`GET /api/v1/jobs/{id}`) reports both the configured per-job overrides and
`effective_retry`, the fully resolved policy the job runs under.

A manual `POST /api/v1/tasks/{id}/retry` or `POST /api/v1/jobs/{id}/retry`
resets a task/job's failure counters, independent of this policy. See
[the task state machine](architecture.md#state-machine-task-status) for how
retry, exhaustion, and auto-park interact, and
[Retry and auto-park metrics](observability.md#retry-and-auto-park-metrics)
for the Prometheus counters.

> **Upgrading from v0.1.0.** v0.1.0 had no auto-retry. With the default
> `scheduler.default_max_attempts` of `3`, a failed task now gets up to 2 more
> attempts. To keep v0.1.0's behavior, set `scheduler.default_max_attempts: 1`
> (or `SQI_SCHEDULER_DEFAULT_MAX_ATTEMPTS=1`).

---

## `discovery` — mDNS service advertisement

### `discovery.enabled`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `true` |
| **Env var** | `SQI_DISCOVERY_ENABLED` |

When `true`, `sqi-server` broadcasts a `_sqi._tcp` mDNS record so that workers
and the `sqi` CLI can discover it on the local network without manual address
configuration. Disable it where multicast is unavailable, as in most cloud
VPCs and container networks and many VLANs.

The record's `nats_url` field is the broker URL a discovering worker connects
to, used as given: its `nats://` or `tls://` scheme tells the worker whether
the broker requires TLS. In embedded mode it is
[`nats.embedded.advertise_url`](#natsembeddedadvertise_url), or
`nats://<host>.local:<port>` (`tls://` with broker TLS on); in external mode
the first [`nats.external.advertise_urls`](#natsexternaladvertise_urls) entry,
else the first [`nats.external.urls`](#natsexternalurls) entry.

```yaml
discovery:
  enabled: true
```

---

### `discovery.instance_name`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `"sqi-server"` |
| **Env var** | `SQI_DISCOVERY_INSTANCE_NAME` |

The mDNS service instance name advertised on the network. Each `sqi-server` on
the same subnet should use a distinct name to avoid collisions when running
multiple farms on the same local network.

Must not be empty, whatever `discovery.enabled` says: setting it to `""`
fails config validation and the server does not start, even with discovery
off. To disable advertisement, set `discovery.enabled: false` and leave this
key alone.

```yaml
discovery:
  instance_name: "studio-farm-primary"
```

---

## `openjd` — OpenJD submission and validation

### `openjd.enforce_limits`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `true` |
| **Env var** | `SQI_OPENJD_ENFORCE_LIMITS` |
| **CLI flag** | `--openjd-enforce-limits` |

When `true`, submitted job templates are checked against the OpenJD quantitative
limits (maximum name lengths, parameter-definition counts, per-parameter value
counts, and host-requirement counts), plus one sqi-defined limit the OpenJD
specification does not set: a maximum of 100 steps per job template. A
template exceeding 100 steps is rejected with an error at `/steps`. Set to
`false` only for existing templates that break these limits, including ones
with more than 100 steps, and cannot be updated. The resource-exhaustion
guards (the per-range value cap, the 1,000,000-task cap per step and the
1,000,000-task cap per job) apply whatever this is set to.

For an `EXPR` template, `false` lifts the step cap only partly. A template
declaring `extensions: [EXPR]` is also subject to the template-wide
expression budget
([`openjd.expr_template_positions`](#openjdexpr_template_positions), default
10,000), which is a resource-exhaustion guard and stays on. That budget counts
expression positions, so a many-step `EXPR` template is accepted only while
its total positions stay under the limit. At about 70 positions per step (the
shape the default was sized against) that is around 143 steps: a 300-step
`EXPR` template with realistic content is rejected either way, while a
300-step template whose steps carry almost no expressions passes. See
[`docs/openjd-extensions/expr.md`](openjd-extensions/expr.md) for what the
budget bounds.

```yaml
openjd:
  enforce_limits: true
```

---

### EXPR expression limits — read this before changing any of them

Four keys bound what one submitted job template may spend inside the
OpenJD expression-language checker (the `EXPR` extension; see
[`docs/openjd-extensions/expr.md`](openjd-extensions/expr.md)). They exist
because `POST /api/v1/jobs` accepts a template of up to 4 MiB and, with auth
off (the default), accepts it anonymously, so checking a template is
attacker-chosen work on a synchronous request path.

Four routes are bounded by these keys and by
[`openjd.expr_submission_deadline`](#openjdexpr_submission_deadline), and all
four can answer `503` for a deadline. Three accept a client-supplied template
body: `POST /api/v1/jobs`, `POST /api/v1/products` and
`PUT /api/v1/products/{name}` (the last two when a custom product is installed
or edited). The fourth, `POST /api/v1/products/{name}/jobs`, submits a
template the operator already installed, but the parameters are the client's
and every expression is re-evaluated with them bound.

The preset routes, `GET /api/v1/presets/{name}` and
`POST /api/v1/presets/{name}/install`, validate a definition too, under the
same four keys and the deadline. The definition is sha256-pinned against the
index at `preset_library.url`, so the operator vouches for it, but the limits
are operator configuration and apply wherever validation happens, and the
install route needs the same permission as `POST /api/v1/products`, which
everyone holds while auth is off.

Unlike `enforce_limits`, these limits are always on: they are
resource-exhaustion guards rather than OpenJD quantitative limits, and no
setting turns them off. An out-of-range value, including `0`, fails startup;
it is never clamped.

Each key has a counterpart on the worker
([Worker configuration → `expr`](worker-configuration.md#expr--expr-expression-limits))
that meters the same thing at run time. Four caveats apply to all four keys.

A fifth key,
[`openjd.expr_submission_deadline`](#openjdexpr_submission_deadline), is a
wall-clock backstop. It decides only whether this server keeps working on a
request (503), never whether a template is valid (422). It has no worker
counterpart and the four caveats below do not apply to it; see its own
section.

#### 1. The cumulative operation ceiling is the product of two keys

Nothing in sqi counts operations across a whole template. The template-wide
budget counts positions (one per format string or `let:` binding walked), and
each position is separately capped at `openjd.expr_operation_limit`
operations. The cumulative ceiling an operator sets is therefore the product:

```
openjd.expr_template_positions  x  openjd.expr_operation_limit
          10,000                x          10,000              = 10^8 (default)
```

Raising either key multiplies that ceiling; raising both multiplies it twice.
At the two maxima (100,000 x 100,000) it is 10¹⁰, 100x the default. Nothing
measures the sum, so nothing reports that a template came close to it.

`openjd.expr_memory_limit` is not a third multiplier. It decides how large a
value one evaluation may hold, not what an operation costs, and raising it to
the maximum leaves the cost of an operation-heavy template unchanged. Raise it
when a template needs to build a large value.

#### 2. None of these limits bounds wall-clock time

Operations do not all cost the same time. Specification section 1.3.10 rule 3
(`third_party/openjd-specifications/wiki/2026-02-Expression-Language.md`)
prices a string operation at the value's length divided by 256, so byte-heavy
work is charged very little. Each of these expressions fits inside one
position's default 10,000-operation budget:

| Expression | Operations charged | CPU | per operation |
|---|---|---|---|
| `("x" * 900000).upper()` | 7,034 | ~6 ms | 0.9 µs |
| `("x" * 900000).title()` | 7,034 | ~58 ms | 8.2 µs |
| `re_findall("x", "x" * 900000)` | 3,519 | ~51 ms | 14.5 µs |
| `max([len(re_findall("x", "x" * 900000)) for i in range(2)])` | 7,048 | ~103 ms | 14.6 µs |

The cost per operation varies by about 17x, so no operation budget bounds
time. A template of 10,000 positions of the last row (about 650 KB, inside the
4 MiB request limit) costs about 17 minutes of server CPU with every default
budget respected; at the two maxima the same construction costs about 40
hours. Treat these as lower bounds on the worst case: a construction with a
higher cost per operation would cost more.

Raising these limits therefore lengthens the worst request the server can be
asked to serve, roughly in proportion. The time is bounded by a separate key,
[`openjd.expr_submission_deadline`](#openjdexpr_submission_deadline), which
stops one submission's expression evaluation after a fixed wall-clock
allowance (5s by default) and answers `503`. That bound holds whatever the
operation pricing permits, but it does not make the four limits safe to raise
freely: a request refused at the deadline still spent that time, and
concurrent requests each get their own allowance. If `POST /api/v1/jobs` is
reachable without authentication, size request timeouts and front-end
concurrency with that in mind.

Raising these four keys without also raising the deadline turns legitimate
acceptances into 503s. For a generated multi-layer VFX shot-render job (12
`let:` bindings per step, comprehensions over real frame ranges,
`RANGE_EXPR`/`LIST[STRING]`/`PATH` parameters) submitted through the real
submission path:

| Limits | Shape | Wall clock | Verdict |
|---|---|---|---|
| defaults | 100 steps x 1,000 frames | 0.83 s | accepted |
| defaults | 100 steps x 2,000 frames | 1.11 s | `422`, retained bytes |
| legal maxima | 100 steps x 10,000 frames | 7.5 s | `503`, deadline |

At the defaults the deterministic budgets bind before the clock, and the
budget that binds is retained bytes. At the maxima that inverts: a template
the counters would accept is stopped by the 5s clock. If you raise these four,
raise [`openjd.expr_submission_deadline`](#openjdexpr_submission_deadline)
with them.

#### 3. The byte dimension is cumulative allocation, not peak live retention

`openjd.expr_template_retained_bytes` sums what every `let:` block in the
template retains, in evaluation order, across the whole walk. It does not
model when Go's garbage collector reclaims an earlier block's values. A
template that never holds more than 2.7 MB live at once is still charged
10.8 MB if it retains that much across four sequential `let:` blocks, and is
rejected as if it held all of it at once.

So sizing a host's RAM against this number over-provisions, and sizing this
number against an observed RSS rejects templates that would never have used
that memory.

#### 4. Every worker must be at least as generous as this server

The five worker keys meter the same values on the worker host at run time,
after the job has been accepted. A worker tighter than this server could be
handed work this server accepted and fail it there:

| This server accepts templates under… | …so every worker needs at least |
|---|---|
| `openjd.expr_operation_limit` | `expr.operation_limit` |
| `openjd.expr_memory_limit` | `expr.memory_limit` |
| `openjd.expr_template_positions` | `expr.assignment_positions` |
| `openjd.expr_template_retained_bytes` | `expr.assignment_retained_bytes` |
| `openjd.expr_template_retained_bytes` | `expr.let_retained_bytes` |

Every worker advertises all five values when it registers, and the scheduler
does not dispatch EXPR work to a worker whose values are short, so the job is
withheld instead of failing task by task on the host. What an operator sees:

- a `WARN` diagnostic when the short worker registers, naming the worker and
  every short dimension with both numbers and both config keys. The server
  logs it, so it appears under Admin → Server log and not on that worker's
  detail page, which shows only what the worker itself published;
- `unschedulable_reason` on any `ready` task no capable worker exists for,
  carrying the same text, plus a WebSocket event, but only while the
  unschedulable sweep is on. With
  [`scheduler.unschedulable_grace`](#schedulerunschedulable_grace) set to `0`,
  the sweep is disabled and such a task waits `ready` with nothing written on
  it; the registration `WARN`, which fires once and may predate the job, is
  then the only signal.

A short worker keeps running all other work; only EXPR work is withheld from
it. Raise the workers first, then the server.

The last row pairs one key with two: `expr.let_retained_bytes` bounds a single
symbol table, a scope this server does not meter separately, so it is
compared against the template-wide budget that bounds it from above. The
comparison is conservative: a worker whose `expr.let_retained_bytes` is below
`openjd.expr_template_retained_bytes` gets no EXPR work, even when its value
is ample for the templates you submit. Lower the server key or raise the
worker key; either way the mismatch is visible, and no task fails after
acceptance.

None of the five is a guarantee. They are necessary conditions: at run time
the worker binds concrete values this server only had placeholders for, so an
accepted job can still exhaust a worker that satisfies every row above.

> The gate recognizes an EXPR job from the extension list recorded on the job
> row at submission, so for jobs submitted by v0.3.0 or later the test is
> exact: a job flagged with this reason declares `EXPR`.
>
> Job rows written by v0.2.0 or earlier record no list. For those the gate
> falls back to scanning the raw template for the bytes `EXPR` and the bytes
> `extensions`. A template that declares some other extension and also
> mentions the string elsewhere (a comment, or an environment variable such
> as `HOUDINI_EXPR_CACHE`) matches, and the job is withheld from every short
> worker with a reason naming EXPR limits it does not use. The fix is the same
> either way: raise the workers or lower the server. The scan errs toward
> withholding work, except for a deliberately obfuscated declaration (an
> escaped `EXPR` in YAML or JSON), which it misses.

---

### `openjd.expr_operation_limit`

| | |
|---|---|
| **Type** | `int` |
| **Default** | `10000` |
| **Range** | `1000` – `100000` |
| **Env var** | `SQI_OPENJD_EXPR_OPERATION_LIMIT` |

Operations (specification section 1.3.10) that one expression evaluation may
spend. An expression exceeding it is rejected with an operation-limit error
naming the configured number, and the template is refused. The default is far
below the specification's recommended 10 million because sqi checks
client-supplied templates synchronously on the request path.

Multiplies with `expr_template_positions`; see caveat 1 above.

Lowering it shrinks what shipped presets can do. The
`ffmpeg-segment-transcode-expr` preset's join-list expression costs about
24.15 operations per slice, so its documented 400-slice ceiling assumes the
default of 10,000; at the floor of 1,000 the ceiling is about 41 slices, and
nothing else warns about it.

```yaml
openjd:
  expr_operation_limit: 10000
```

### `openjd.expr_memory_limit`

| | |
|---|---|
| **Type** | `int` |
| **Default** | `1000000` |
| **Range** | `4096` – `10000000` |
| **Env var** | `SQI_OPENJD_EXPR_MEMORY_LIMIT` |

Live bytes (specification section 1.3.9) that one expression evaluation may
hold. Exceeding it fails that evaluation with a memory-limit error naming the
configured number. The default is far below the specification's recommended
100 million, for the same reason as the operation limit.

This decides how large a string or list a single expression can build, which
is why caveat 2's examples use strings of about 900 KB.

A single `let:` block can hold 50 times this number. The template-wide
retained-bytes counter is charged once per block, after the block finishes,
so one block can transiently hold up to `50 x openjd.expr_memory_limit`
before anything sees it: 50 MB at this default and 500 MB at the 10000000
maximum, per concurrent request, on a path that is unauthenticated by
default. Size the server's RAM against that product. (50 is the
specification's per-block binding cap, Template Schemas §3.6.) The same
ceiling is described under
[`openjd.expr_template_retained_bytes`](#openjdexpr_template_retained_bytes).

It does not multiply wall-clock time; see caveat 1.

```yaml
openjd:
  expr_memory_limit: 1000000
```

### `openjd.expr_template_positions`

| | |
|---|---|
| **Type** | `int` |
| **Default** | `10000` |
| **Range** | `256` – `100000` |
| **Env var** | `SQI_OPENJD_EXPR_TEMPLATE_POSITIONS` |

Expression positions one template may contain, summed across a whole
validation pass: one charge per format string checked and one per `let:`
binding evaluated. Exceeding it rejects the template with a template-wide
budget error.

The default allows about 70 positions for a generous step across the 100-step
cap (about 7,040), with headroom. A template of about 21 positions per step (a
command, 15 args, 3 environment variables, 2 embedded files, no `let:`) uses
about 2,100.

Template upload, submit-time re-checking and the parameter-space resolver
each walk under their own allowance.

Raising this key alone is the likeliest way to strand work (caveat 4). Raise
every worker's `expr.assignment_positions` to at least this value first.

```yaml
openjd:
  expr_template_positions: 10000
```

### `openjd.expr_template_retained_bytes`

| | |
|---|---|
| **Type** | `int` |
| **Default** | `10000000` |
| **Range** | `65536` – `100000000` |
| **Env var** | `SQI_OPENJD_EXPR_TEMPLATE_RETAINED_BYTES` |

Bytes a template's `let:` bindings may cumulatively retain across one walk.
`let:` is the only construct in the walk that keeps a value (every other
position renders a result and discards it), so it is the only thing that
charges this counter. It counts cumulative allocation; see caveat 3.

> **Raise every worker before raising this key.** It is compared against two
> worker keys (caveat 4): `expr.assignment_retained_bytes`, whose default of
> `20000000` leaves 2× headroom, and `expr.let_retained_bytes`, whose default
> of `10000000` equals this default. Any increase puts every
> default-configured worker below this server and withholds EXPR work from
> all of them. Raise `expr.let_retained_bytes` on every worker first, to at
> least the value you intend to set here. Lowering this key is always safe.

Even with both worker keys satisfied, this bound does not cover everything the
worker meters. It charges only what a `let:` block adds, while the worker
measures the whole symbol table the bindings land in, including a job's own
large `STRING`/`PATH` parameters. A worker sized exactly at this value can
still fail an accepted job whose parameters are large. See
[`expr.let_retained_bytes`](worker-configuration.md#exprlet_retained_bytes).

This key does not bound a single block. The charge lands once per `let:`
block, after the block finishes evaluating, so one block can transiently
retain up to 50 x `openjd.expr_memory_limit` (50 MB at the defaults) before
this counter sees it. (50 is the specification's per-block binding cap,
Template Schemas §3.6.) That per-block ceiling moves with `expr_memory_limit`.

```yaml
openjd:
  expr_template_retained_bytes: 10000000
```

### `openjd.expr_submission_deadline`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `5s` |
| **Range** | `1s` – `60s` |
| **Env var** | `SQI_OPENJD_EXPR_SUBMISSION_DEADLINE` |

How long this server keeps evaluating one submission's expressions before
giving up on it.

This is not a fifth limit and does not decide whether a template is valid.
The four limits above are deterministic: a template that breaches one
breaches it on every machine, so the submitter gets
`422 Unprocessable Entity` and retrying cannot help. This one is wall clock,
so the same body could be accepted on an idle host and refused on a loaded
one. A breach therefore reports that this server gave up, with
`503 Service Unavailable`, and a retry may succeed. Persistent 503s mean
either an expensive template or a deadline set too low.

Six routes can answer `503` for this reason: `POST /api/v1/jobs` and
`POST /api/v1/products/{name}/jobs` (which otherwise answer `422` for an
invalid template), `POST /api/v1/products` and `PUT /api/v1/products/{name}`
(which otherwise answer `400`), and the two preset routes
`GET /api/v1/presets/{name}` and `POST /api/v1/presets/{name}/install` (which
otherwise answer `422`, and also answer `503`, for an unrelated reason, when
no preset library is configured). On every route the 4xx is a verdict on the
template and the 503 is not.

It exists because none of the four limits bounds time (caveat 2): a template
within every default budget can cost about 17 minutes of server CPU without
this deadline. It is the only bound that does not depend on that estimate.

The floor is `1s` because below it a legitimate large template (a body near
the 4 MiB request cap, on a busy host) could be refused. The ceiling is `60s`
because a ceiling anywhere near the 17-minute worst case would bound nothing.

> **The headroom against real work is about 6x.** The largest template in
> caveat 2's VFX example that the defaults accept (100 steps over 1,000
> frames, 5,013 positions, 1.66M operations) takes 0.83 s against the 5s
> default; on a host 2–3x slower it would take 1.7–2.5 s. Keep that in mind
> before lowering this key.
>
> At the defaults, realistic `let:`-using templates hit the retained-bytes
> budget at about 1.1 s, before this deadline. A template that does cheap,
> byte-heavy work across many positions and discards each result never
> approaches that budget, so this deadline is the only bound on it. At the
> legal maximum limits the deadline binds for legitimate templates too, so
> raising the four limits without raising this key converts legitimate
> acceptances into 503s (caveat 2).

It has no worker counterpart: the worker evaluates jobs this server has
already accepted, not anonymous requests, so caveat 4 does not apply.

```yaml
openjd:
  expr_submission_deadline: 5s
```

> **A malformed duration fails at startup, in the file and the environment
> alike.** `5 sec`, `5000` or `5 s` is not a Go duration, so
> `expr_submission_deadline: 5 sec` fails the load naming its line, and
> `SQI_OPENJD_EXPR_SUBMISSION_DEADLINE=5 sec` naming the variable. This holds
> for every duration key in the server's configuration. v0.3.0 and earlier
> skipped an unparseable value in the file and kept the default, so a
> configuration file those versions accepted can fail to load after an
> upgrade.

---

## `diagnostics` — In-UI diagnostic log buffer

### `diagnostics.buffer_size`

| | |
|---|---|
| **Type** | `int` |
| **Default** | `1000` |
| **Env var** | `SQI_DIAGNOSTICS_BUFFER_SIZE` |

Maximum diagnostic records retained per component (`server` plus each
connected worker) in `sqi-server`'s in-memory ring buffer. This value is also
the on/off switch:

- `0`: diagnostics are disabled. No buffer is allocated, the server does not
  subscribe to `worker.diag.>`, and the REST diagnostics endpoint returns 503.
- positive: the per-component capacity. When a component's buffer is full
  the oldest records are evicted. The buffer is in-memory only and is cleared on
  server restart.

Negative values are rejected. The buffer feeds the web UI (Admin → Server log
and each worker's detail page).

```yaml
diagnostics:
  buffer_size: 2000 # or 0 to disable
```

> Workers have their own `diagnostics.enabled` toggle, since they publish
> rather than buffer; see [Worker configuration](#worker-configuration) below.

See [`docs/observability.md`](observability.md) for the full diagnostics guide.

---

## `preset_library` — Remote preset catalog

### `preset_library.url`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `"https://uberware.github.io/sqi-presets/index.json"` |
| **Env var** | `SQI_PRESET_LIBRARY_URL` |

URL of the preset library's JSON index. The default points to the official community
preset library hosted on GitHub Pages.

Set to an empty string `""` to disable the feature. When disabled, the Preset
Library browse page shows a "not configured" empty state and all `/api/v1/presets`
endpoints return 503.

```yaml
preset_library:
  url: "https://uberware.github.io/sqi-presets/index.json"
```

To use a private or self-hosted library, provide any accessible HTTP or HTTPS URL
that serves the index JSON. See [`docs/preset-library.md`](preset-library.md) for the
index format and full integration guide.

---

## `ha` — High availability

Opt-in. With `ha.enabled: false` (the default) nothing under `ha` is read or
validated and the server behaves as a single-server farm. When enabled, any
number of `sqi-server` processes share one PostgreSQL database and one
external NATS account; exactly one is the **leader** and does the farm's work,
and the rest are warm standbys. Use one configuration file for every server in
the group.

### `ha.enabled`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `false` |
| **Env var** | `SQI_HA_ENABLED` |

Lets this server join an HA group. It requires
[`store.driver: postgres`](#storedriver) and
[`nats.mode: external`](#natsmode); either missing fails config validation with
an error on `ha.enabled` naming the key to change, and the server does not
start. Both conflicts are reported together when both apply.

```yaml
ha:
  enabled: true
store:
  driver: postgres
nats:
  mode: external
```

---

### `ha.lease_ttl`

| | |
|---|---|
| **Type** | duration |
| **Default** | `15s` |
| **Env var** | `SQI_HA_LEASE_TTL` |

How long the leader's lease lasts without being renewed, and therefore how long
a standby waits to take over after a crash. The renew period (`lease_ttl / 3`)
and the point at which a leader that cannot renew stops itself (`lease_ttl x
0.8`) are derived from it rather than configured.

Must be between `6s` and `60s`; a value outside that range fails config
validation. It is checked only when `ha.enabled` is `true`. Shorter values fail
over sooner but tolerate less database latency before a healthy leader loses
its lease; longer values do the opposite.

```yaml
ha:
  lease_ttl: 15s
```

---

### `ha.instance_name`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `""` (the host name) |
| **Env var** | `SQI_HA_INSTANCE_NAME` |

The name this server is known by in logs, `/readyz` and the list of servers.
When empty it is the host name, or `sqi-server` if the host name cannot be
read. Give each server in a group a distinct name when several share a host
name, as containers started from one image can.

```yaml
ha:
  instance_name: sched-a
```

---

## `auth` — Authentication (opt-in)

### `auth.enabled`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `false` |
| **Env var** | `SQI_AUTH_ENABLED` |
| **CLI flag** | `--auth-enabled` |

The switch for sqi's authentication gate. With the default `false`, the
server is open on a trusted local network and every request is served as an
anonymous superuser.

Setting it to `true` requires every REST request and the WebSocket upgrade to
carry valid credentials (a session or an API key) and enforces role-based
authorization: every mutating route and several read routes are gated by a
role→permission policy. See [`docs/auth.md`](auth.md).

```yaml
auth:
  enabled: false
```

---

### `auth.validate_job_owner`

| | |
|---|---|
| **Type** | `bool` |
| **Default** | `true` |
| **Env var** | `SQI_AUTH_VALIDATE_JOB_OWNER` |
| **CLI flag** | `--auth-validate-job-owner` |

Rejects a job submission whose `owner` (set via a `jobs.submit_as` override)
names no known user, with `400`. This keeps `Job.Owner` reliable for
owner-scoped visibility: the `user` role's job listings are filtered by owner,
so a mistyped owner would produce a job invisible to the person who owns it
and missed by an admin filtering on them. Disable it when owners come from a
directory that has not yet provisioned local records.

```yaml
auth:
  validate_job_owner: true
```

---

### `auth.session.ttl`

| | |
|---|---|
| **Type** | `duration` |
| **Default** | `168h` (7 days) |
| **Env var** | `SQI_AUTH_SESSION_TTL` |

Absolute lifetime of a session created by `POST /api/v1/auth/login`, from
creation. There is no sliding or idle renewal: using a session does not
extend it. Must be `> 0` when `auth.enabled` is `true`; not validated when
auth is disabled.

```yaml
auth:
  session:
    ttl: "168h"
```

---

### `auth.session.cookie_name`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `"sqi_session"` |
| **Env var** | `SQI_AUTH_SESSION_COOKIE_NAME` |

Name of the session cookie set by login and cleared by logout. Change this
only if `sqi_session` collides with another cookie on the same domain (e.g.
sqi served under a shared reverse-proxy host alongside another app).

Must not be empty when `auth.enabled` is true, or the server fails to start.
The CSRF middleware reads the cookie by name, and an empty name would let
every mutating request take the "no session cookie" exempt path, disabling
CSRF protection.

```yaml
auth:
  session:
    cookie_name: "sqi_session"
```

---

### `auth.session.cookie_secure`

| | |
|---|---|
| **Type** | `string` |
| **Default** | `"auto"` |
| **Accepted values** | `auto`, `true`, `false` |
| **Env var** | `SQI_AUTH_SESSION_COOKIE_SECURE` |

Controls the session cookie's `Secure` attribute. `"auto"` sets `Secure` when
the request arrived over TLS or carries `X-Forwarded-Proto: https`, which
covers a TLS-terminating proxy. `"false"` forces `Secure` off, for a
plain-HTTP LAN deployment; `"true"` forces it on. Any other value fails config
validation when `auth.enabled` is `true`.

```yaml
auth:
  session:
    cookie_secure: "auto"
```

---

### `auth.bootstrap.username` / `auth.bootstrap.password`

| | |
|---|---|
| **Type** | `string` / `string` |
| **Default** | `""` / `""` |
| **Env var** | `SQI_AUTH_BOOTSTRAP_USERNAME` / `SQI_AUTH_BOOTSTRAP_PASSWORD` |

Seed credentials for the first admin account, applied once at startup only
when `auth.enabled` is `true` and the `users` table is empty; the account
is created with `role: "admin"`. Once any user exists, these values have no
effect and never overwrite an existing password. Leaving both empty is valid:
an auth-enabled server with no users logs a `WARN` and boots without an admin,
until an operator sets both and restarts. Setting only one of the two is a
startup validation error, which catches a mistyped env var name.

`auth.bootstrap.password` is redacted (`<redacted>`) in
`sqi-server config print` and any other re-marshaled dump of the config; the
loaded value is used normally.

```yaml
auth:
  bootstrap:
    username: "admin"
    password: "change-me-after-first-login"
```

See [`docs/auth.md`](auth.md) for the full authentication model, the local
account model, and the role/permission matrix enforced on every route.

---

### `auth.ldap.*`

Directory (LDAP / Active Directory) authentication. Every key below sits
under `auth.ldap`. No `auth.ldap` key has a CLI flag; set them in the config
file or the environment. `role_map` is file-only, because a list of
group→role pairs has no sensible environment encoding.

The whole block is inert while `auth.ldap.enabled` is false: the server never
builds a verifier and never contacts a directory, whatever else is set here.
Validation of the keys below only runs when `auth.ldap.enabled` is true.

Setting `auth.ldap.enabled: true` while `auth.enabled` is false is a
configuration error, and the server refuses to start:

```
auth.ldap.enabled: requires auth.enabled=true; LDAP without the auth gate authenticates nobody
```

Enable both together, or neither.

| Key | Type | Default | Env var |
|---|---|---|---|
| `auth.ldap.enabled` | bool | `false` | `SQI_AUTH_LDAP_ENABLED` |
| `auth.ldap.url` | string | `""` | `SQI_AUTH_LDAP_URL` |
| `auth.ldap.start_tls` | bool | `false` | `SQI_AUTH_LDAP_START_TLS` |
| `auth.ldap.tls_skip_verify` | bool | `false` | `SQI_AUTH_LDAP_TLS_SKIP_VERIFY` |
| `auth.ldap.ca_file` | string | `""` | `SQI_AUTH_LDAP_CA_FILE` |
| `auth.ldap.timeout` | duration | `10s` | `SQI_AUTH_LDAP_TIMEOUT` |
| `auth.ldap.bind_dn` | string | `""` | `SQI_AUTH_LDAP_BIND_DN` |
| `auth.ldap.bind_password` | string | `""` | `SQI_AUTH_LDAP_BIND_PASSWORD` |
| `auth.ldap.base_dn` | string | `""` | `SQI_AUTH_LDAP_BASE_DN` |
| `auth.ldap.user_filter` | string | `(sAMAccountName=%s)` | `SQI_AUTH_LDAP_USER_FILTER` |
| `auth.ldap.nested_groups` | bool | `false` | `SQI_AUTH_LDAP_NESTED_GROUPS` |
| `auth.ldap.user_dn_template` | string | `""` | `SQI_AUTH_LDAP_USER_DN_TEMPLATE` |
| `auth.ldap.username_attr` | string | `sAMAccountName` | `SQI_AUTH_LDAP_USERNAME_ATTR` |
| `auth.ldap.display_name_attr` | string | `displayName` | `SQI_AUTH_LDAP_DISPLAY_NAME_ATTR` |
| `auth.ldap.unique_id_attr` | string | *(none — required)* | `SQI_AUTH_LDAP_UNIQUE_ID_ATTR` |
| `auth.ldap.role_source` | string | `directory` | `SQI_AUTH_LDAP_ROLE_SOURCE` |
| `auth.ldap.role_map` | list | `[]` | *(file only — no env form)* |
| `auth.ldap.default_role` | string | `read-only` | `SQI_AUTH_LDAP_DEFAULT_ROLE` |

**Transport.** `url` must be `ldap://…` or `ldaps://…` and is required when
enabled. `start_tls` upgrades a plain `ldap://` connection and is rejected
against `ldaps://`, which is already TLS. `ca_file` is a PEM bundle used to
verify the directory's certificate; an unreadable file aborts boot.
`tls_skip_verify` disables certificate verification, which lets a
man-in-the-middle read every password sent over the connection; it is logged
as a `WARN` at boot and should never be set outside a lab. `timeout` must be
`> 0`; it bounds the TCP connect and each request leg, and is the only bound
on a hung directory (see
[A hung directory](auth.md#a-hung-directory)).

**Bind mode.** The two modes are mutually exclusive, and one is required:

- **Search-then-bind:** set `base_dn` (required) and usually `bind_dn` +
  `bind_password`. `user_filter` must contain `%s`, the placeholder for the
  escaped username. Leaving `bind_dn` empty selects anonymous search, which
  is valid; setting `bind_password` without `bind_dn` is a validation error,
  because the password would be ignored.
- **Template bind:** set `user_dn_template`, which must contain `%s` (e.g.
  `uid=%s,ou=people,dc=example,dc=com`). Combining it with
  `bind_dn`/`base_dn` is a validation error.

`nested_groups` requires search mode. Template bind reads the flat
`memberOf` attribute, so the combination is rejected at boot.

**Roles.** `role_source` must be `directory` or `local` (see
[`role_source`](auth.md#role_source--who-owns-a-users-role) for what each
means and why one value drives both the login re-sync and the API's 409).
`role_map` is ordered and the first match wins. Each entry needs a non-empty
`group` and a `role` that is one of `admin`, `operator`, `user`, `read-only`;
an unknown role fails validation. `default_role` accepts those same four
values or empty, where empty means reject any login that matched no group.

`bind_password` is redacted (`<redacted>`) in `sqi-server config print`, like
`auth.bootstrap.password`. Prefer setting it through
`SQI_AUTH_LDAP_BIND_PASSWORD`, so it never lands in a config file on disk.

```yaml
auth:
  enabled: true
  ldap:
    enabled: true
    url: "ldaps://dc01.example.com:636"
    timeout: "10s"
    bind_dn: "CN=sqi-svc,OU=Service Accounts,DC=example,DC=com"
    # bind_password via SQI_AUTH_LDAP_BIND_PASSWORD
    base_dn: "DC=example,DC=com"
    user_filter: "(sAMAccountName=%s)"
    username_attr: "sAMAccountName"
    display_name_attr: "displayName"
    unique_id_attr: "objectGUID"   # "entryUUID" on OpenLDAP; no default, always required
    nested_groups: true
    role_source: "directory"
    role_map:
      - group: "CN=Farm Admins,OU=Groups,DC=example,DC=com"
        role: admin
      - group: "CN=Farm Operators,OU=Groups,DC=example,DC=com"
        role: operator
    default_role: "read-only"
```

**`unique_id_attr` is required and has no default**, because no single value
is correct on both Active Directory (`objectGUID`) and RFC 4530 servers
(`entryUUID`), and guessing on a server exposing both could pick the wrong
one. A config with `auth.ldap.enabled: true` and no `unique_id_attr` fails
validation and the server does not boot. LDAP accounts provisioned before
this key existed carry an empty identifier and must be recreated; see
[Upgrading from an earlier sqi](auth.md#upgrading-from-an-earlier-sqi).

See [`docs/auth.md`](auth.md#ldap--active-directory) for the model behind
these keys: why LDAP attaches at login rather than per request, how
just-in-time provisioning works, why accounts match on a stable identifier
rather than a username, why a local admin account is required in `directory`
mode, and the revocation-lag and timing caveats.

---

### `auth.oidc.*`

OAuth2/OIDC single sign-on. Every key below sits under `auth.oidc`. No
`auth.oidc` key has a CLI flag; set them in the config file or the
environment. `role_map` is file-only, for the same reason as
`auth.ldap.role_map`.

The whole block is inert while `auth.oidc.enabled` is false: the server never
builds the SSO route, whatever else is set here. Validation of the keys below
only runs when `auth.oidc.enabled` is true.

Setting `auth.oidc.enabled: true` while `auth.enabled` is false is a
configuration error, and the server refuses to start:

```
auth.oidc.enabled: requires auth.enabled=true; SSO without the auth gate authenticates nobody
```

Enable both together, or neither. `issuer` discovery happens on first use
rather than at boot, so a briefly unreachable provider does not stop the
server from starting.

The block configures a single identity provider.

| Key | Type | Default | Env var |
|---|---|---|---|
| `auth.oidc.enabled` | bool | `false` | `SQI_AUTH_OIDC_ENABLED` |
| `auth.oidc.issuer` | string | `""` | `SQI_AUTH_OIDC_ISSUER` |
| `auth.oidc.client_id` | string | `""` | `SQI_AUTH_OIDC_CLIENT_ID` |
| `auth.oidc.client_secret` | string | `""` | `SQI_AUTH_OIDC_CLIENT_SECRET` |
| `auth.oidc.redirect_url` | string | `""` | `SQI_AUTH_OIDC_REDIRECT_URL` |
| `auth.oidc.scopes` | []string | `[openid, profile, email]` | `SQI_AUTH_OIDC_SCOPES` (comma-separated) |
| `auth.oidc.username_claim` | string | `preferred_username` | `SQI_AUTH_OIDC_USERNAME_CLAIM` |
| `auth.oidc.display_name_claim` | string | `name` | `SQI_AUTH_OIDC_DISPLAY_NAME_CLAIM` |
| `auth.oidc.groups_claim` | string | `groups` | `SQI_AUTH_OIDC_GROUPS_CLAIM` |
| `auth.oidc.role_source` | string | `directory` | `SQI_AUTH_OIDC_ROLE_SOURCE` |
| `auth.oidc.role_map` | list | `[]` | *(file only — no env form)* |
| `auth.oidc.default_role` | string | `read-only` | `SQI_AUTH_OIDC_DEFAULT_ROLE` |
| `auth.oidc.reauth_mode` | string | `after_logout` | `SQI_AUTH_OIDC_REAUTH_MODE` |
| `auth.oidc.logout_mode` | string | `local` | `SQI_AUTH_OIDC_LOGOUT_MODE` |
| `auth.oidc.post_logout_redirect_url` | string | `""` | `SQI_AUTH_OIDC_POST_LOGOUT_REDIRECT_URL` |
| `auth.oidc.button_label` | string | `Sign in with SSO` | `SQI_AUTH_OIDC_BUTTON_LABEL` |

**Endpoint.** `issuer`, `client_id`, `client_secret`, and `redirect_url` are
all required when enabled. `issuer` and
`redirect_url` must each be an absolute URL. `redirect_url` must resolve to
this server's `/api/v1/auth/oidc/callback`; `post_logout_redirect_url` is
only read (and only required) when `logout_mode` is `"provider"`.

**Claims.** `scopes` must be non-empty and include `"openid"`. `"groups"` is
not a standard OIDC scope: whether group membership needs a scope, a
provider-side mapper, or both varies by provider. `username_claim`,
`display_name_claim` and `groups_claim` must not be empty; each has a
default, so an empty value can only come from clearing it explicitly.
`groups_claim` names the claim that feeds `role_map`.

**Roles.** `role_source` must be `directory` or `local`. It mirrors
`auth.ldap.role_source` but is set separately, so an operator can trust one
provider's groups and not the other's. `role_map` is ordered and the first
match wins. Each entry needs a non-empty `group` and a `role` that is one of
`admin`, `operator`, `user`, `read-only`; an unknown role fails validation.
`default_role` accepts those same four values or empty, where empty means
reject any login that matched no group.

**Reauth and logout.** `reauth_mode` is one of `after_logout` (re-prompt only
on the login following an explicit logout), `always` (re-prompt every
login), or `never` (silent re-login always permitted). `logout_mode` is
`local` (end only the sqi session) or `provider` (also end the session at the
identity provider, which signs the user out of every tool that trusts it, and
is therefore not the default).

`client_secret` is redacted (`<redacted>`) in `sqi-server config print`, like
`auth.ldap.bind_password`. Prefer setting it through
`SQI_AUTH_OIDC_CLIENT_SECRET`, so it never lands in a config file on disk.

```yaml
auth:
  enabled: true
  oidc:
    enabled: true
    issuer: "https://login.microsoftonline.com/<tenant>/v2.0"
    client_id: "sqi"
    # client_secret via SQI_AUTH_OIDC_CLIENT_SECRET
    redirect_url: "https://sqi.example.com/api/v1/auth/oidc/callback"
    scopes: ["openid", "profile", "email"]
    username_claim: "preferred_username"
    display_name_claim: "name"
    groups_claim: "groups"
    role_source: "directory"
    role_map:
      - group: "sqi-farm-admins"
        role: admin
      - group: "sqi-farm-operators"
        role: operator
    default_role: "read-only"
    reauth_mode: "after_logout"
    logout_mode: "local"
```

See [`docs/auth.md`](auth.md#oidc--sso) for the model behind these keys: the
login flow and its defenses, why accounts match on the `sub` claim, how
`reauth_mode` and `logout_mode` differ, and the limits (revocation lag, roles
applying at next login only, what `logout_mode: provider` does on Keycloak,
and which providers CI covers).

---

## Queue identity: `run_as_user` / `run_as_group` (task isolation)

`run_as_user` and `run_as_group` are not `sqi-server` config-file keys, and
there is no server-wide or farm-wide default for either. They are set per
queue, through the queue REST resource only (`POST /api/v1/queues`,
`PUT /api/v1/queues/{id}`); the web UI and the Python SDK have no field for
them. They are documented here because, like the retry overrides above, they
are queue-level settings that change worker behavior.

| Field | Type | Default | Effect |
|---|---|---|---|
| `run_as_user` | `string \| null` | `null` | OS username tasks (and OpenJD environment `onEnter`/`onExit` actions) in this queue execute as. `null`, the default, means no isolation: tasks run as the worker daemon's own account. |
| `run_as_group` | `string \| null` | `null` | OS group for the same tasks. `null` means the target user's primary group. |

Both fields are excluded from the Farm → server-default cascade that the
retry-policy fields above use, because a farm-wide default would apply an OS
identity to a queue whose owner never configured one. A queue's isolation
setting is either explicit or absent; it is never inherited.

**Permission.** Setting either field, including sending an explicit `null` to
clear a previously-set value, requires the `isolation.manage` permission,
held only by the `admin` role; `infra.manage` (which `operator` holds, and
which gates every other queue field) is not sufficient. Omitting both keys
from a `PUT` body preserves whatever is stored and requires no permission.
This exception to `PUT`'s full-replace semantics lets an `operator` without
`isolation.manage` edit a queue's priority or concurrency limit without
clearing an `admin`'s isolation configuration. See
[Task isolation](auth.md#task-isolation) in the auth guide for the reasoning,
including why enabling isolation raises the worker daemon's privilege even
though it lowers each task's.

**Validation.** `run_as_group` requires `run_as_user` to be set too (in the
same request, or already stored and preserved by an omitted key). The
scheduler applies isolation only when `run_as_user` is set, so a group with no
user would be ignored. Both `POST` and `PUT` reject that combination with
`400 Bad Request`.

The scheduler places only the username in the task assignment sent to the
worker over NATS, never a credential, and the worker resolves it to a real OS
credential itself. This matters because both worker↔server transport
protections are opt-in and off by default: authentication via `nats.auth`
(see [Known gaps](auth.md#known-gaps)) and encryption via TLS on the broker (see
[`docs/tls.md`](tls.md)). In a default deployment the assignment channel is
neither authenticated nor encrypted, so nothing secret may travel on it.

This mechanism runs on both POSIX (Linux/macOS) and Windows (the worker must
run as a LocalSystem service, or hold `SeAssignPrimaryTokenPrivilege`, to
assume another account's identity); see
[`docs/worker-configuration.md`](worker-configuration.md#isolation--run-as-user-task-execution)
for the full worker-side `isolation` config block, the environment allowlist,
and the per-platform requirements.

### Important: Worker upgrade required

`run_as_user` is enforced only by workers that support task isolation. A
worker built without it (v0.2.0 and earlier) would ignore the `isolation`
field in its task assignments and run job code as the worker daemon's own OS
user, reporting success.

The server prevents that: it discards registrations, heartbeats and status
reports from any worker whose wire-protocol version differs from its own, and
such a worker is offered no work at all. Every worker without isolation
support speaks an older protocol version, so it never receives an
assignment. The scheduler does not otherwise check for isolation support, so
upgrade every worker to the server's release and do not rely on mixed
versions.

A worker that supports isolation but is misconfigured fails closed: it
refuses to start, or fails the individual task with an actionable error
message.

---

## Worker configuration

Worker configuration applies to `sqi-worker` instances, not the server. Workers
load configuration from the same layered sources as the server (defaults → file →
environment → flags), except that the config file is named `sqi-worker.yaml`
and environment variables use the `SQI_WORKER_` prefix, apart from
`SQI_DIAGNOSTICS_ENABLED` and `SQI_STAGING_DEFAULTS`.

The full worker key reference (NATS connection, identity, metrics, discovery,
log streaming, and more) is in
[`docs/worker-configuration.md`](worker-configuration.md), and a commented
example is at
[`config/sqi-worker.example.yaml`](https://github.com/uberware/sqi/blob/main/config/sqi-worker.example.yaml).
The keys below are summarized for operators configuring a worker alongside
the server: `staging` controls local path staging, `diagnostics` is the
counterpart of the server-side diagnostics setting, `expr` must match the
server's EXPR limits, and `capabilities` configures software auto-detection.

### Staging (`stage_locally` path delivery)

Used by workers that run jobs declaring the `stage_locally` delivery of
`SQI_PATH_TRANSLATION`.

| Key | Type | Description |
|---|---|---|
| `staging.scratch_dir` | string | Base directory for per-attempt staged copies. Defaults to `<os.TempDir()>/sqi-staging` when unset. |
| `staging.sync_command` | string | Command template invoked per path, with `{src}`, `{dest}`, and optional `{object_type}` placeholders (e.g. `rsync -a {src} {dest}`). The same template serves copy-in and copy-out. Left unset (or set to `builtin`), sqi copies the bytes itself instead of shelling out. |
| `staging.defaults` | bool | Default `true`. When true, an otherwise-unconfigured worker still runs `stage_locally` jobs via the TEMP scratch dir and built-in copy above (one-time WARN logged). Set `false` to make an unconfigured worker fail `stage_locally` jobs immediately instead. |

```yaml
staging:
  scratch_dir: "/tmp/sqi-staging"
  sync_command: "rsync -a {src} {dest}"
  defaults: true
```

Full detail, including the limits of the built-in copy and the
`SQI_STAGING_DEFAULTS` environment variable, is in
[Worker configuration → `staging`](worker-configuration.md#staging--local-path-staging-stage_locally-delivery).

### Diagnostics (`diagnostics.enabled`)

Controls whether the worker mirrors its own structured (`slog`) output to
`sqi-server` over core NATS, where it appears in the web UI alongside server
logs. This is the worker agent's operational log, separate from task process
output, which is always streamed. It is the worker's counterpart to the
server's `diagnostics.buffer_size`: workers publish and the server buffers.

| Key | Type | Default | Env var | Description |
|---|---|---|---|---|
| `diagnostics.enabled` | bool | `true` | `SQI_DIAGNOSTICS_ENABLED` | When `true`, the worker's diagnostic-log records are published to `sqi-server` in addition to the local log (stderr, or `log.file` when set). Set `false` to keep them local only. |

Note the env var is `SQI_DIAGNOSTICS_ENABLED` (not `SQI_WORKER_…`), matching the
server-side diagnostics naming.

```yaml
diagnostics:
  enabled: true
```

See [`docs/observability.md`](observability.md) for the full diagnostics guide.

### EXPR expression limits (`expr.*`)

The worker's five `expr.*` keys bound what one assignment may spend
evaluating EXPR expressions on the worker host at run time. Each must be at
least as generous as its `openjd.expr_*` counterpart on this server, or the
scheduler withholds EXPR work from that worker. See
[EXPR expression limits, caveat 4](#4-every-worker-must-be-at-least-as-generous-as-this-server)
above for the pairings and what an operator sees, and
[Worker configuration → `expr`](worker-configuration.md#expr--expr-expression-limits)
for the keys themselves.

### Capability auto-detection (`capabilities.detect` / `capabilities.disable`)

Controls the worker's software capability auto-detection. Built-in detectors
for Maya, Nuke, Houdini, Blender, Cinema 4D, 3ds Max, After Effects, FFmpeg,
SGO Mistika (Boutique/Ultima, VR and Workflows), the standalone renderers
(Arnold `kick`, V-Ray, `husk`, Redshift, RenderMan, 3Delight and Mantra) and
`oiiotool` run automatically at startup and advertise a tag (e.g. `maya`, plus `maya_2025`) with value `"true"`, with no
per-worker configuration. That alone satisfies the reference DCC presets'
`key=true` requirement. See
[`docs/products.md`](products.md) and
[`docs/dcc-submitters.md`](dcc-submitters.md) for how these auto-detected
tags relate to the reference DCC products/presets.

| Key | Type | Default | Env var | Description |
|---|---|---|---|---|
| `capabilities.detect` | `[]Detector` | `[]` | — (config file only) | Custom detectors for in-house tools, same schema as the built-ins. |
| `capabilities.disable` | `[]string` | `[]` | `SQI_WORKER_CAPABILITIES_DISABLE` (comma-separated, appended) | Turn off a built-in detector by tag name, e.g. `[blender]`. |

```yaml
capabilities:
  detect:
    - tag: mytool
      checks:
        - exe: mytool
  disable: [blender]
```

See [`docs/worker-capabilities.md`](worker-capabilities.md#capability-auto-detection-built-in-dcc-detectors)
for the full auto-detection guide (how it runs, the tag/version model, the
`sqi-worker capabilities` command) and
[`docs/worker-capabilities.md`](worker-capabilities.md#writing-custom-detectors)
for the detector schema reference.

---

## Quick reference table

| Key | Type | Default | Env var | CLI flag |
|---|---|---|---|---|
| `http.addr` | string | `0.0.0.0:8080` | `SQI_HTTP_ADDR` | `--http-addr` |
| `http.enable_pprof` | bool | `false` | `SQI_HTTP_ENABLE_PPROF` | — |
| `http.rate_limit.enabled` | bool | `true` | `SQI_HTTP_RATE_LIMIT_ENABLED` | — |
| `http.cors_origins` | []string | `[]` (= `*`) | `SQI_HTTP_CORS_ORIGINS` | `--http-cors-origins` |
| `http.tls.enabled` | bool | `false` | `SQI_HTTP_TLS_ENABLED` | — |
| `http.tls.cert_file` | string | `""` | `SQI_HTTP_TLS_CERT_FILE` | — |
| `http.tls.key_file` | string | `""` | `SQI_HTTP_TLS_KEY_FILE` | — |
| `nats.mode` | string | `embedded` | `SQI_NATS_MODE` | — |
| `nats.auth.enabled` | bool | `false` | `SQI_NATS_AUTH_ENABLED` | — |
| `nats.auth.user_ttl` | duration | `1h` | `SQI_NATS_AUTH_USER_TTL` | — |
| `nats.auth.join_token_ttl` | duration | `1h` | `SQI_NATS_AUTH_JOIN_TOKEN_TTL` | — |
| `nats.auth.join_token_single_use` | bool | `true` | `SQI_NATS_AUTH_JOIN_TOKEN_SINGLE_USE` | — |
| `nats.auth.enrollment_endpoint_enabled` | bool | `true` | `SQI_NATS_AUTH_ENROLLMENT_ENDPOINT_ENABLED` | — |
| `nats.embedded.addr` | string | `0.0.0.0:4222` | `SQI_NATS_EMBEDDED_ADDR` | — |
| `nats.embedded.advertise_url` | string | `""` | `SQI_NATS_EMBEDDED_ADVERTISE_URL` | — |
| `nats.embedded.data_dir` | string | `data/nats` | `SQI_NATS_EMBEDDED_DATA_DIR` | — |
| `nats.embedded.max_store_mb` | int | `1024` | `SQI_NATS_EMBEDDED_MAX_STORE_MB` | — |
| `nats.embedded.tls.enabled` | bool | `false` | `SQI_NATS_EMBEDDED_TLS_ENABLED` | — |
| `nats.embedded.tls.cert_file` | string | `""` | `SQI_NATS_EMBEDDED_TLS_CERT_FILE` | — |
| `nats.embedded.tls.key_file` | string | `""` | `SQI_NATS_EMBEDDED_TLS_KEY_FILE` | — |
| `nats.embedded.tls.client_ca_file` | string | `""` | `SQI_NATS_EMBEDDED_TLS_CLIENT_CA_FILE` | — |
| `nats.external.urls` | []string | `[]` | `SQI_NATS_EXTERNAL_URLS` | — |
| `nats.external.advertise_urls` | []string | `[]` | `SQI_NATS_EXTERNAL_ADVERTISE_URLS` | — |
| `nats.external.account` | string | `SQI` | `SQI_NATS_EXTERNAL_ACCOUNT` | — |
| `nats.external.nkey_seed_file` | string | `""` | `SQI_NATS_EXTERNAL_NKEY_SEED_FILE` | — |
| `nats.external.creds_file` | string | `""` | `SQI_NATS_EXTERNAL_CREDS_FILE` | — |
| `nats.external.stream_replicas` | int | `1` | `SQI_NATS_EXTERNAL_STREAM_REPLICAS` | — |
| `nats.external.connect_timeout` | duration | `30s` | `SQI_NATS_EXTERNAL_CONNECT_TIMEOUT` | — |
| `nats.external.tls.ca_file` | string | `""` | `SQI_NATS_EXTERNAL_TLS_CA_FILE` | — |
| `nats.external.tls.cert_file` | string | `""` | `SQI_NATS_EXTERNAL_TLS_CERT_FILE` | — |
| `nats.external.tls.key_file` | string | `""` | `SQI_NATS_EXTERNAL_TLS_KEY_FILE` | — |
| `nats.external.tls.server_name` | string | `""` | `SQI_NATS_EXTERNAL_TLS_SERVER_NAME` | — |
| `nats.external.callout.nkey_seed_file` | string | `""` | `SQI_NATS_EXTERNAL_CALLOUT_NKEY_SEED_FILE` | — |
| `nats.external.callout.issuer_seed_file` | string | `""` | `SQI_NATS_EXTERNAL_CALLOUT_ISSUER_SEED_FILE` | — |
| `nats.external.callout.system_creds_file` | string | `""` | `SQI_NATS_EXTERNAL_CALLOUT_SYSTEM_CREDS_FILE` | — |
| `store.driver` | string | `sqlite` | `SQI_STORE_DRIVER` | — |
| `store.sqlite_path` | string | `sqi.db` | `SQI_STORE_SQLITE_PATH` | — |
| `store.checkpoint_interval` | duration | `5m` | `SQI_STORE_CHECKPOINT_INTERVAL` | — |
| `store.postgres.dsn` | string | `""` | `SQI_STORE_POSTGRES_DSN` | — |
| `store.postgres.max_conns` | int | `16` | `SQI_STORE_POSTGRES_MAX_CONNS` | — |
| `log.level` | string | `info` | `SQI_LOG_LEVEL` | `--log-level` |
| `log.format` | string | `json` | `SQI_LOG_FORMAT` | `--log-format` |
| `log.file` | string | `""` | `SQI_LOG_FILE` | — |
| `log.max_size_mb` | int | `100` | `SQI_LOG_MAX_SIZE_MB` | — |
| `log.max_backups` | int | `5` | `SQI_LOG_MAX_BACKUPS` | — |
| `scheduler.heartbeat_timeout` | duration | `30s` | `SQI_SCHEDULER_HEARTBEAT_TIMEOUT` | — |
| `scheduler.tick_interval` | duration | `500ms` | `SQI_SCHEDULER_TICK_INTERVAL` | — |
| `scheduler.max_tasks_per_worker` | int | `1` | `SQI_SCHEDULER_MAX_TASKS_PER_WORKER` | — |
| `scheduler.offline_worker_retention` | duration | `24h` | `SQI_SCHEDULER_OFFLINE_WORKER_RETENTION` | — |
| `scheduler.job_retention` | duration | `168h` | `SQI_SCHEDULER_JOB_RETENTION` | — |
| `scheduler.job_retention_include_failed` | bool | `false` | `SQI_SCHEDULER_JOB_RETENTION_INCLUDE_FAILED` | — |
| `scheduler.unschedulable_grace` | duration | `30s` | `SQI_SCHEDULER_UNSCHEDULABLE_GRACE` | — |
| `scheduler.reconcile_grace` | duration | `2m` (or `heartbeat_timeout` if longer, when unset) | `SQI_SCHEDULER_RECONCILE_GRACE` | — |
| `scheduler.default_max_attempts` | int | `3` | `SQI_SCHEDULER_DEFAULT_MAX_ATTEMPTS` | — |
| `scheduler.retry_delay` | duration | `30s` | `SQI_SCHEDULER_RETRY_DELAY` | — |
| `scheduler.default_failure_limit` | int | `0` | `SQI_SCHEDULER_DEFAULT_FAILURE_LIMIT` | — |
| `discovery.enabled` | bool | `true` | `SQI_DISCOVERY_ENABLED` | — |
| `discovery.instance_name` | string | `sqi-server` | `SQI_DISCOVERY_INSTANCE_NAME` | — |
| `openjd.enforce_limits` | bool | `true` | `SQI_OPENJD_ENFORCE_LIMITS` | `--openjd-enforce-limits` |
| `openjd.expr_operation_limit` | int | `10000` | `SQI_OPENJD_EXPR_OPERATION_LIMIT` | — |
| `openjd.expr_memory_limit` | int | `1000000` | `SQI_OPENJD_EXPR_MEMORY_LIMIT` | — |
| `openjd.expr_template_positions` | int | `10000` | `SQI_OPENJD_EXPR_TEMPLATE_POSITIONS` | — |
| `openjd.expr_template_retained_bytes` | int | `10000000` | `SQI_OPENJD_EXPR_TEMPLATE_RETAINED_BYTES` | — |
| `openjd.expr_submission_deadline` | duration | `5s` | `SQI_OPENJD_EXPR_SUBMISSION_DEADLINE` | — |
| `diagnostics.buffer_size` | int | `1000` | `SQI_DIAGNOSTICS_BUFFER_SIZE` | — |
| `preset_library.url` | string | `https://uberware.github.io/sqi-presets/index.json` | `SQI_PRESET_LIBRARY_URL` | — |
| `ha.enabled` | bool | `false` | `SQI_HA_ENABLED` | — |
| `ha.lease_ttl` | duration | `15s` | `SQI_HA_LEASE_TTL` | — |
| `ha.instance_name` | string | `""` (host name) | `SQI_HA_INSTANCE_NAME` | — |
| `auth.enabled` | bool | `false` | `SQI_AUTH_ENABLED` | `--auth-enabled` |
| `auth.validate_job_owner` | bool | `true` | `SQI_AUTH_VALIDATE_JOB_OWNER` | `--auth-validate-job-owner` |
| `auth.session.ttl` | duration | `168h` | `SQI_AUTH_SESSION_TTL` | — |
| `auth.session.cookie_name` | string | `sqi_session` | `SQI_AUTH_SESSION_COOKIE_NAME` | — |
| `auth.session.cookie_secure` | string | `auto` | `SQI_AUTH_SESSION_COOKIE_SECURE` | — |
| `auth.bootstrap.username` | string | `""` | `SQI_AUTH_BOOTSTRAP_USERNAME` | — |
| `auth.bootstrap.password` | string | `""` | `SQI_AUTH_BOOTSTRAP_PASSWORD` | — |
| `auth.ldap.enabled` | bool | `false` | `SQI_AUTH_LDAP_ENABLED` | — |
| `auth.ldap.url` | string | `""` | `SQI_AUTH_LDAP_URL` | — |
| `auth.ldap.start_tls` | bool | `false` | `SQI_AUTH_LDAP_START_TLS` | — |
| `auth.ldap.tls_skip_verify` | bool | `false` | `SQI_AUTH_LDAP_TLS_SKIP_VERIFY` | — |
| `auth.ldap.ca_file` | string | `""` | `SQI_AUTH_LDAP_CA_FILE` | — |
| `auth.ldap.timeout` | duration | `10s` | `SQI_AUTH_LDAP_TIMEOUT` | — |
| `auth.ldap.bind_dn` | string | `""` | `SQI_AUTH_LDAP_BIND_DN` | — |
| `auth.ldap.bind_password` | string | `""` | `SQI_AUTH_LDAP_BIND_PASSWORD` | — |
| `auth.ldap.base_dn` | string | `""` | `SQI_AUTH_LDAP_BASE_DN` | — |
| `auth.ldap.user_filter` | string | `(sAMAccountName=%s)` | `SQI_AUTH_LDAP_USER_FILTER` | — |
| `auth.ldap.nested_groups` | bool | `false` | `SQI_AUTH_LDAP_NESTED_GROUPS` | — |
| `auth.ldap.user_dn_template` | string | `""` | `SQI_AUTH_LDAP_USER_DN_TEMPLATE` | — |
| `auth.ldap.username_attr` | string | `sAMAccountName` | `SQI_AUTH_LDAP_USERNAME_ATTR` | — |
| `auth.ldap.display_name_attr` | string | `displayName` | `SQI_AUTH_LDAP_DISPLAY_NAME_ATTR` | — |
| `auth.ldap.unique_id_attr` | string | *(none — required)* | `SQI_AUTH_LDAP_UNIQUE_ID_ATTR` | — |
| `auth.ldap.role_source` | string | `directory` | `SQI_AUTH_LDAP_ROLE_SOURCE` | — |
| `auth.ldap.role_map` | list | `[]` | — | — |
| `auth.ldap.default_role` | string | `read-only` | `SQI_AUTH_LDAP_DEFAULT_ROLE` | — |
| `auth.oidc.enabled` | bool | `false` | `SQI_AUTH_OIDC_ENABLED` | — |
| `auth.oidc.issuer` | string | `""` | `SQI_AUTH_OIDC_ISSUER` | — |
| `auth.oidc.client_id` | string | `""` | `SQI_AUTH_OIDC_CLIENT_ID` | — |
| `auth.oidc.client_secret` | string | `""` | `SQI_AUTH_OIDC_CLIENT_SECRET` | — |
| `auth.oidc.redirect_url` | string | `""` | `SQI_AUTH_OIDC_REDIRECT_URL` | — |
| `auth.oidc.scopes` | []string | `[openid, profile, email]` | `SQI_AUTH_OIDC_SCOPES` | — |
| `auth.oidc.username_claim` | string | `preferred_username` | `SQI_AUTH_OIDC_USERNAME_CLAIM` | — |
| `auth.oidc.display_name_claim` | string | `name` | `SQI_AUTH_OIDC_DISPLAY_NAME_CLAIM` | — |
| `auth.oidc.groups_claim` | string | `groups` | `SQI_AUTH_OIDC_GROUPS_CLAIM` | — |
| `auth.oidc.role_source` | string | `directory` | `SQI_AUTH_OIDC_ROLE_SOURCE` | — |
| `auth.oidc.role_map` | list | `[]` | — | — |
| `auth.oidc.default_role` | string | `read-only` | `SQI_AUTH_OIDC_DEFAULT_ROLE` | — |
| `auth.oidc.reauth_mode` | string | `after_logout` | `SQI_AUTH_OIDC_REAUTH_MODE` | — |
| `auth.oidc.logout_mode` | string | `local` | `SQI_AUTH_OIDC_LOGOUT_MODE` | — |
| `auth.oidc.post_logout_redirect_url` | string | `""` | `SQI_AUTH_OIDC_POST_LOGOUT_REDIRECT_URL` | — |
| `auth.oidc.button_label` | string | `Sign in with SSO` | `SQI_AUTH_OIDC_BUTTON_LABEL` | — |

---

## Minimal production example

```yaml
# /etc/sqi/sqi-server.yaml

http:
  addr: "0.0.0.0:8080"

nats:
  embedded:
    data_dir: "/var/lib/sqi/nats"
    max_store_mb: 4096

store:
  sqlite_path: "/var/lib/sqi/sqi.db"

log:
  level: "info"
  format: "json"

scheduler:
  heartbeat_timeout: "60s"

discovery:
  enabled: false       # disable multicast in a server environment
  instance_name: "sqi-prod"
```

---

## See also

- [`config/sqi-server.example.yaml`](https://github.com/uberware/sqi/blob/main/config/sqi-server.example.yaml) —
  Commented example covering the HTTP, NATS, store, log, scheduler, discovery,
  OpenJD, diagnostics and preset-library sections. The `http.cors_origins` key
  and the whole `auth` block are documented in this reference only.
- [`docs/architecture.md`](architecture.md) — Component layout and how configuration values are consumed.
- [`docs/operations.md`](operations.md) — Install, upgrade, rollback, backup, restore, moving to PostgreSQL, and log rotation.
- [`docs/observability.md`](observability.md) — In-UI diagnostics, REST/WS log API, and external log wiring.
