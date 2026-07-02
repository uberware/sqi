# S3-compatible storage

sqi is a **thin layer** with respect to S3. It stores no credentials, embeds no
S3 client library, and moves no bytes itself. Object-store data reaches a worker
through the same two paths already used for on-premises storage: **mounted** or
**staged**.

## Mount vs stage: choose one per root

### Mounted access

Use a FUSE mount tool (mountpoint-s3, goofys, s3fs, rclone mount, …) to expose
the bucket as a regular filesystem path on each worker. Register that path as a
plain filesystem root:

```yaml
name: shows
roots:
  cloud_linux: /mnt/s3-shows   # mountpoint-s3 or goofys mount
  onprem:      /nas/shows
```

sqi sees an ordinary path. No `stage_locally`, no sync command, nothing extra.

### Staged access (B4 `stage_locally`)

Use an `s3://` root and the `SQI_PATH_TRANSLATION` extension with `stage_locally`.
Before each task the worker invokes the operator-configured `staging.sync_command`
to copy inputs to worker-local scratch; after the task it copies outputs back.
sqi supplies `{src}` and `{dest}` placeholders; the sync tool does the transfer.

```yaml
name: shows
roots:
  cloud: s3://studio-bucket/shows
  onprem: /nas/shows
```

```yaml
# sqi-worker config
staging:
  scratch_dir: /scratch/sqi
  sync_command: "aws s3 cp {src} {dest}"
```

```yaml
# Job template
extensions: [ SQI_PATH_TRANSLATION ]
SQI_PATH_TRANSLATION:
  deliveries:
    - stage_locally
    - swap_in_place
```

### Which to choose

| Scenario | Recommended approach |
|---|---|
| Workers have persistent mounts (on-prem NAS, always-on cloud) | Mounted |
| Ephemeral cloud workers (no mount daemon, burst fleet) | Staged |
| Mixed fleet (on-prem + cloud) | Mixed: filesystem root for on-prem, `s3://` root for cloud workers, with `stage_locally` |

---

## sqi stores no credentials or endpoint

Endpoint addresses, AWS credentials, rclone remote configuration, and mc
aliases all live **in the operator's per-worker environment** — not in sqi.
Every sync tool reads them through its own standard mechanisms:

| Tool | Where config lives |
|---|---|
| AWS CLI | `~/.aws/credentials`, `~/.aws/config`, or env vars (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_DEFAULT_REGION`, `AWS_ENDPOINT_URL`) |
| rclone | `~/.config/rclone/rclone.conf` (named remotes) |
| MinIO Client (`mc`) | `~/.mc/config.json` (named aliases) |

sqi has no visibility into these settings and makes no attempt to validate them.

---

## Per-provider `sync_command` recipes

Set `staging.sync_command` in `sqi-worker.yaml`. Use `{src}` and `{dest}` as
placeholders for the source and destination paths.

### AWS S3

```yaml
sync_command: "aws s3 cp {src} {dest}"
```

Credentials from the standard AWS credential chain (`~/.aws`, instance profile,
task role, etc.).

### MinIO (via `--endpoint-url`)

```yaml
sync_command: "aws --endpoint-url http://minio.internal:9000 s3 cp {src} {dest}"
```

Or set `AWS_ENDPOINT_URL` in the worker environment and omit the flag.

### Cloudflare R2

```yaml
sync_command: "aws --endpoint-url https://<account_id>.r2.cloudflarestorage.com s3 cp {src} {dest}"
```

R2 credentials go in `~/.aws/credentials` (or environment variables) using the
R2 API token as access/secret key.

### Backblaze B2

```yaml
# rclone — configure a B2 remote named "b2" first
sync_command: "rclone copy {src} {dest}"
```

Or use the S3-compatible endpoint with the AWS CLI:

```yaml
sync_command: "aws --endpoint-url https://s3.us-west-004.backblazeb2.com s3 cp {src} {dest}"
```

### rclone (generic)

rclone works for any provider. Configure a named remote in `rclone.conf`, then:

```yaml
sync_command: "rclone copy {src} {dest}"
```

rclone translates `s3://bucket/key` paths to the appropriate remote
automatically when the remote is aliased to the bucket.

### MinIO Client (`mc`)

```yaml
# Configure an alias first: mc alias set myminio http://minio.internal:9000 KEY SECRET
sync_command: "mc cp {src} {dest}"
```

---

## `s3://` path without staging fails at run time

If a job resolves a `loc://` reference to an `s3://` path but does **not**
enable `stage_locally`, the worker fails the task **pre-execution** (before the
process is launched) with this message:

```
resolved path "s3://..." is an object-store URI but stage_locally is not enabled
for this job; enable the SQI_PATH_TRANSLATION stage_locally delivery or use a
filesystem/mounted root
```

There is no submit-time warning — the failure appears in the task's error
details. Fix it by either:

1. Adding `stage_locally` to the job's `SQI_PATH_TRANSLATION` deliveries, or
2. Replacing the `s3://` root with a filesystem/mounted path for that compute
   location.

---

## Storage location `type` is derived

sqi derives the `type` field from the roots; you cannot set it directly
(supplying `type` on create or update returns HTTP 400). The three possible
values:

| `type` | Meaning |
|---|---|
| `filesystem` | All roots are filesystem paths (or there are no roots) |
| `s3` | All roots are `s3://` URIs |
| `mixed` | Roots span both schemes |

A `mixed` location is the normal shape for a farm that uses on-prem NAS for
some workers and S3 for cloud workers. sqi keys actual path-joining behavior off
each individual root's scheme, not off the derived `type`.

Each `s3://` root is validated as a well-formed `s3://bucket[/prefix]` URI on
create and update. A value that starts with `s3:` but is missing the double
slash (e.g. `s3:mybucket`) is rejected immediately.

---

## Cloud workers without direct storage access

Cloud workers that have no network path to on-prem NAS and no FUSE mount should
use `stage_locally`. The workflow is:

1. Upload source assets to S3 before submitting the job (your pipeline's
   responsibility — sqi does not replicate data).
2. Register an `s3://` root for the cloud compute location.
3. Enable `stage_locally` in the job's `SQI_PATH_TRANSLATION` block.
4. Configure each cloud worker with `staging.scratch_dir` and
   `staging.sync_command`.

Outputs are copied back after each task automatically by the same sync command
(direction reversed: `dest` ← `src`).

See [`docs/openjd-extensions/path-translation.md`](openjd-extensions/path-translation.md)
for the full `stage_locally` reference.
