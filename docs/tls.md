# TLS

sqi can terminate TLS in-process on every listener it opens: `sqi-server`'s REST
API, WebSocket gateway and embedded web UI on one listener, its embedded NATS
broker on another, and the worker's own metrics and health endpoint on a third.
Workers can optionally be required to present a client certificate. With an
external NATS cluster (`nats.mode: external`), the cluster's own TLS covers the
worker transport, and `sqi-server` is one more TLS client of it (see
[External broker](#external-broker)).

**TLS is off by default.** A `sqi-server` started with no TLS configuration
opens one plaintext HTTP listener and one plaintext broker. Everything below
is opt-in.

Running behind a TLS-terminating reverse proxy is equally supported and is what
many deployments will do anyway — see [Reverse proxy](#reverse-proxy).

## What is encrypted, and what is not

| Surface | Covered by | Notes |
|---|---|---|
| REST API, WebSocket, web UI | `http.tls` | One listener; it upgrades in place |
| Worker ↔ broker (assignments, status, logs) | `nats.embedded.tls`, or the external cluster's own TLS | Closes the cleartext gap that broker authentication alone does not |
| `sqi-server` ↔ external broker | `nats.external.tls` and `tls://` URLs | Only with `nats.mode: external`; the embedded broker's own connections are in-process |
| Worker enrollment (`POST /api/v1/workers/enroll`) | `http.tls` server-side, `nats.server_tls_ca_file` worker-side | Runs over REST before the worker holds any credential |
| Outbound LDAP / OIDC | Their own settings (`auth.ldap.*`, `auth.oidc.*`) | Unchanged by anything here |
| Worker metrics / health (`metrics.addr`) | `metrics.tls` (worker) | Loopback by default, where plaintext is fine; turn TLS on with any address reachable from elsewhere, since this carries metrics, health and — with `enable_pprof` — full runtime profiles |

Two things TLS does **not** do:

- **It is not authentication.** Broker TLS encrypts the worker transport; it does
  not decide *which* worker is connecting. That remains the per-worker nkey
  credential — see [`docs/auth.md`](auth.md). The two are independent and a
  production farm wants both.
- **It does not protect data at rest.** The SQLite database, JetStream's
  file-backed streams and worker session directories are unaffected.

## Configuration

### Server

```yaml
http:
  tls:
    enabled: false
    cert_file: ""     # PEM certificate: leaf first, then any intermediates
    key_file: ""      # PEM private key matching cert_file

nats:
  embedded:
    tls:
      enabled: false
      cert_file: ""
      key_file: ""
      client_ca_file: ""  # set = require and verify worker client certificates
```

Environment: `SQI_HTTP_TLS_ENABLED`, `SQI_HTTP_TLS_CERT_FILE`,
`SQI_HTTP_TLS_KEY_FILE`, `SQI_NATS_EMBEDDED_TLS_ENABLED`,
`SQI_NATS_EMBEDDED_TLS_CERT_FILE`, `SQI_NATS_EMBEDDED_TLS_KEY_FILE`,
`SQI_NATS_EMBEDDED_TLS_CLIENT_CA_FILE`.

The two blocks are deliberately independent. An operator may legitimately serve
the API with a publicly-issued certificate and the broker with a private farm CA,
or turn on one without the other.

There is no minimum-version or cipher-suite setting. Go's server default is
already TLS 1.2 minimum, and a farm has no use for lowering it.

### External broker

With `nats.mode: external`, sqi does not terminate the worker transport: the
cluster's listeners do, configured in the cluster's own nats-server config.
`sqi-server` connects to it as a client:

```yaml
nats:
  external:
    urls: ["tls://nats-a:4222", "tls://nats-b:4222", "tls://nats-c:4222"]
    tls:
      ca_file: ""       # CA that verifies the cluster's certificate; empty = system roots
      cert_file: ""     # client certificate, for a cluster that requires mTLS
      key_file: ""
      server_name: ""   # name to verify the certificate against, if not the URL's host
```

Environment: `SQI_NATS_EXTERNAL_TLS_CA_FILE`, `SQI_NATS_EXTERNAL_TLS_CERT_FILE`,
`SQI_NATS_EXTERNAL_TLS_KEY_FILE`, `SQI_NATS_EXTERNAL_TLS_SERVER_NAME`.

The scheme of `nats.external.urls` decides: `tls://` URLs use TLS, and every
URL must use the same scheme. The `tls` keys are refused beside `nats://`
URLs, since they would make the result depend on what the cluster offers.
Workers use `tls://` URLs too, with the cluster's CA in `nats.tls_ca_file`.
`sqi-server tls init --host <cluster-name>` can issue the cluster's certificate
from the farm CA, as the [reference cluster](operations.md#the-reference-compose-cluster)
does. TLS between the cluster's nodes is the cluster's concern.

### Worker

```yaml
nats:
  url: "nats://sqi-server.example:4222"
  tls_enabled: "auto"                      # "auto" | "true" | "false"
  tls_ca_file: ""                          # CA that verifies the BROKER certificate
  tls_cert_file: ""                        # client certificate, for mTLS
  tls_key_file: ""
  server_url: "https://sqi-server.example:8080"
  server_tls_ca_file: ""                   # CA that verifies the API certificate
  server_tls_insecure_skip_verify: false

metrics:
  addr: "127.0.0.1:9091"
  tls:                                     # the worker's own metrics listener
    enabled: false
    cert_file: ""
    key_file: ""
```

Environment: `SQI_WORKER_NATS_TLS_ENABLED`, `SQI_WORKER_NATS_TLS_CA_FILE`,
`SQI_WORKER_NATS_TLS_CERT_FILE`, `SQI_WORKER_NATS_TLS_KEY_FILE`,
`SQI_WORKER_NATS_SERVER_TLS_CA_FILE`,
`SQI_WORKER_NATS_SERVER_TLS_INSECURE_SKIP_VERIFY`,
`SQI_WORKER_METRICS_TLS_ENABLED`, `SQI_WORKER_METRICS_TLS_CERT_FILE`,
`SQI_WORKER_METRICS_TLS_KEY_FILE`.

`metrics.tls` is independent of everything else here: it protects the worker's
own listener, not its connection to the server. A worker needs a certificate of
its own for it — issue one with `sqi-server tls issue --host <worker-host>`, or
use whatever your monitoring stack already trusts.

`tls_ca_file` and `server_tls_ca_file` are separate keys on purpose: they verify
two different certificates, on two different ports, which can legitimately come
from different issuers. On a farm generated by `sqi-server tls init` they are
the same `ca.crt`.

**`tls_enabled` is a three-valued string, not a bool** — the same shape as the
server's `auth.session.cookie_secure`:

| Value | Meaning |
|---|---|
| `"auto"` (default) | Use TLS when there is a reason to: any `tls_*` field is set, **or** the discovered server advertises a TLS-required broker |
| `"true"` | Always use TLS, even with no CA configured (system roots) — for a broker presenting a publicly-trusted certificate, which nothing else would signal |
| `"false"` | Never use TLS, even with a CA configured — an explicit override for an operator who knows the broker is plaintext |

A bool cannot express this: the interesting states are *infer* and *force off*,
and a bool collapses both onto its zero value. The usual boolean spellings
(`1`/`yes`/`on`, `0`/`no`/`off`) are accepted as synonyms in both the file and
the environment; anything else is rejected at startup rather than silently
falling back to `auto`.

### Discovery fills in "auto"

A worker that finds its server over mDNS uses the broker URL the server
advertises in its `nats_url` TXT record, and the URL's scheme says whether the
broker requires TLS: `tls://` when `nats.embedded.tls.enabled` is on, or
whatever scheme the advertised external URL has. Under `"auto"` a `tls://` URL
is a reason to use TLS, so a discovered TLS farm connects over TLS with nothing
configured on the worker at all. Two consequences:

- With no `tls_ca_file`, the broker certificate is verified against the **system
  roots**, so a farm CA fails with a certificate error naming the issuer. The
  worker warns about exactly this at startup. Copy `ca.crt` over and set
  `tls_ca_file`.
- Under `"false"` a `tls://` URL still connects over TLS, since the NATS
  client requires it for that scheme, but with none of the worker's TLS
  settings: no CA file, no client certificate, no `insecure_skip_verify`, so
  the certificate is verified against the system roots. The worker warns
  about this, because the certificate error that follows does not mention
  the setting responsible.

An explicit `nats.url` discovers nothing, so none of this applies: an operator
who configures the URL by hand configures the transport by hand.

### CA semantics

Uniform in every component: **an empty CA setting means the system roots; a CA
setting means that CA only.** Pointing a worker at a farm CA *pins* the acceptable
issuer rather than adding to the public set. A deployment using publicly-issued
certificates leaves the CA settings empty.

### Validation

Certificate problems are refused **at load**, with a message naming the config key.
Nothing about a certificate is allowed to fail for the first time at connect time.

| Condition | Result |
|---|---|
| `enabled: true` with an empty `cert_file` or `key_file` | error at startup |
| File missing, unreadable, or key/certificate mismatch | error at startup |
| Certificate already expired | error at startup |
| Certificate expires within 30 days (including the `nats.external.tls` client certificate) | WARN at startup |
| Certificate files set while `enabled: false` | WARN — "serving plaintext" |
| `nats.embedded.tls.client_ca_file` set while `nats.embedded.tls.enabled: false` | error at startup |
| `nats.external.tls.*` set beside `nats://` URLs, or only one of `cert_file` / `key_file` | error at startup |

An expired certificate is an error rather than a warning because a server that
boots with one is a server whose entire farm fails to connect — strictly worse
than refusing to boot with an actionable message.

## Generating certificates

```bash
sqi-server tls init --out ./certs --host sqi-server.example
```

Writes a farm CA (`ca.crt`, `ca.key` — ECDSA P-256, 10 years) and a server
certificate (`server.crt`, `server.key` — 2 years) covering the hosts given by
`--host`, plus `localhost`, `127.0.0.1` and `::1` in every case. Private keys are
written `0600`; certificates `0644`. On success it prints the exact configuration
keys to set on each side.

Without `--host`, the certificate covers this machine's hostname and its mDNS
name, the hostname in the `.local` domain (`render-srv` and `render-srv.local`).
The mDNS name is the host of the broker URL that [discovery](configuration.md#discoveryenabled)
derives when `nats.embedded.advertise_url` is empty, so a worker that finds the
server by mDNS can verify it.

`--host` is repeatable. Loopback is always included even when `--host` names
specific hosts: a certificate naming only the LAN host cannot be verified from the
machine itself, which breaks local health probes and any client reaching the server
by the name it actually runs under.

For the optional mTLS path, issue per-worker client certificates:

```bash
sqi-server tls init --out ./certs --host sqi-server.example --client render-01 --client render-02
```

The command **refuses to overwrite an existing `ca.key`**. Replacing a farm CA
invalidates every certificate ever issued from it, so that has to be a deliberate
act: move the old directory aside first.

That refusal means `tls init` cannot be re-run to add a worker later — use
`tls issue`.

### Adding a worker, or rotating the server certificate

```bash
# add a worker to a farm that already exists
sqi-server tls issue --out ./certs --client render-07

# rotate the server certificate (replaces server.crt, so --force)
sqi-server tls issue --out ./certs --host sqi-server.example --force
```

`tls issue` signs from the CA already in `--out` and never touches it, so every
certificate already deployed keeps working. Distribute the new
`client-<id>.crt`/`.key` to that worker and set `nats.tls_cert_file` /
`nats.tls_key_file`; no other worker needs to change.

It **refuses to replace an existing file without `--force`**. A client key that
is already deployed belongs to a running worker, and replacing it takes that
worker offline at its next restart with nothing to indicate why.

A rotated server certificate is only picked up on restart — certificates are
read once, at startup.

The command exists because SANs and extended key usage are what hand-rolled
`openssl` invocations most often get wrong, and getting them wrong fails at
worker-connect time — far from the mistake — rather than at load. Any CA works;
this one makes the common case correct by default.

## Worker client certificates (mTLS)

Setting `nats.embedded.tls.client_ca_file` makes the embedded broker require a
client certificate signed by that CA from every connecting worker. An external
cluster that requires client certificates does so in its own configuration,
and `sqi-server` then needs `nats.external.tls.cert_file` and `key_file` too.

This **layers on** the nkey credential rather than replacing it. The certificate
gates *who may open a connection*; the nkey decides *which worker* they are, and
remains what drives subject permissions, per-worker inbox prefixes and revocation.
A valid farm certificate on its own authenticates nobody.

sqi does not bind a client certificate to a worker ID: any enrolled nkey may be
presented over any valid farm client certificate. If you need certificate-to-worker
binding, that is not available today.

The server's own connection to its own broker is exempt from the client-certificate
requirement. It runs in-process over a pipe rather than the network, and requiring
a certificate of the process that already holds the broker's private key would be
meaningless. Network peers are never exempt.

## Enabling TLS on a running farm

**This is a coordinated restart, not a rolling one.** When TLS is enabled the
listener upgrades in place — there is no plaintext port left — and a worker
configured for TLS cannot talk to a plaintext broker. There is no window in which
both old and new workers can connect.

The supported order:

1. `sqi-server tls init --out ./certs --host <server-name>`
2. Copy `ca.crt` to every worker.
3. Stage each worker's `tls_ca_file` and `server_tls_ca_file` in its config, but
   do not restart the workers yet.
4. Set `http.tls` and `nats.embedded.tls` on the server, then restart the
   server and all workers together.

## Certificate rotation

Rotation requires a **server restart**. Certificates are read once, at startup,
and validated before the listener opens; there is no hot-reload. Replacing the
files under a running server takes effect only at its next restart.

Renew before expiry: the server refuses to start with an expired certificate, and
warns for 30 days beforehand.

## Reverse proxy

Terminating TLS at nginx, Caddy, Traefik or a cloud load balancer is fully
supported and needs no TLS configuration in sqi. Two things to get right:

- **Set `X-Forwarded-Proto: https`.** Session cookies default to
  `auth.session.cookie_secure: "auto"`, which resolves `Secure` from the request's
  TLS state or that header. Without it, cookies issued behind a proxy are not
  marked `Secure`. The CSRF origin check reads the same signal.
- **The link from the proxy to sqi is still plaintext** unless you also enable
  `http.tls` and point the proxy at HTTPS. On a single host over loopback that is
  usually fine; across a network it is not.

A reverse proxy in front of the **API** does nothing for the **broker**. Workers
connect to the NATS port directly, so TLS on the broker is the only thing that
encrypts assignments, status and log chunks.

## Verifying

```bash
curl --cacert ./certs/ca.crt https://sqi-server.example:8080/readyz
```

On **Windows**, add `--ssl-revoke-best-effort`:

```powershell
# curl.exe, not curl: Windows PowerShell aliases `curl` to Invoke-WebRequest.
curl.exe --cacert .\certs\ca.crt --ssl-revoke-best-effort https://sqi-server.example:8080/readyz
```

Windows curl builds link Schannel, which insists on checking certificate
revocation. The CA `tls init` generates publishes no CRL or OCSP endpoint, so
the request otherwise fails with `schannel: the revocation status is unknown`
(exit 60) against a chain that is perfectly valid. The flag relaxes only the
revocation check — an untrusted root is still rejected, so the command keeps
proving the server presents a certificate signed by your CA. This affects curl
only; sqi's own components verify with Go's TLS stack and need no such flag.

The startup log states the transport:

```
level=INFO msg="http: listening" addr=0.0.0.0:8080 tls=true url=https://localhost:8080
```

A worker that fails to connect over TLS reports the reason rather than retrying
silently: a certificate signed by an unknown CA fails verification with that named
as the cause, and a worker with no TLS configuration attempting a TLS-required
broker fails with `secure connection not available`.

The server advertises `tls=1` in its mDNS TXT records when the API is
TLS-terminated, and a `tls://` `nats_url` when the broker is. A discovering
worker acts on the `nats_url`; see
[Discovery fills in "auto"](#discovery-fills-in-auto).

`scripts/smoke.sh` runs its whole flow in three modes — plain, broker-auth and TLS
— so the TLS path is exercised end to end on every CI run.

## See also

- [`docs/auth.md`](auth.md) — the authentication model, including broker
  authentication and worker enrollment
- [`docs/configuration.md`](configuration.md) — every configuration key
- [`docs/architecture.md`](architecture.md) — NATS subjects and data flow
