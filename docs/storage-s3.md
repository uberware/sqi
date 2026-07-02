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
sqi passes three placeholders: `{src}` and `{dest}` for the source and destination
paths, and `{object_type}` (expands to `FILE` or `DIRECTORY`). For an `s3://`
storage root the concrete path is a literal `s3://bucket/key` URI — the sync tool
receives, for example, `aws s3 cp s3://bucket/shows/scene.hip /scratch/attempt/0/scene.hip`.
The sync tool therefore **must** accept `s3://` URI arguments natively (or be wrapped
in a script that rewrites them — see the rclone and mc sections below).

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

Set `staging.sync_command` in `sqi-worker.yaml`. Three placeholders are available:

| Placeholder | Expands to |
|---|---|
| `{src}` | Concrete source path |
| `{dest}` | Concrete destination path |
| `{object_type}` | `FILE` or `DIRECTORY` (from the OpenJD PATH parameter's `objectType`) |

**Important:** for an `s3://` storage root the concrete path is a literal
`s3://bucket/key` URI. Only tools that accept `s3://` URI syntax natively work
without a wrapper. `aws s3 cp` and `s5cmd` both do; rclone and mc do not — see
their sections below.

### AWS S3

```yaml
sync_command: "aws s3 cp {src} {dest}"
```

Credentials from the standard AWS credential chain (`~/.aws`, instance profile,
task role, etc.). For non-AWS providers see the MinIO, R2, and B2 sections
(same `aws` CLI, different `--endpoint-url`).

### s5cmd (fast alternative)

[s5cmd](https://github.com/peak/s5cmd) is a high-performance S3 client that
accepts `s3://bucket/key` natively:

```yaml
sync_command: "s5cmd cp {src} {dest}"
```

For non-AWS providers add `--endpoint-url`:

```yaml
sync_command: "s5cmd --endpoint-url http://minio.internal:9000 cp {src} {dest}"
```

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

Use the S3-compatible endpoint with the AWS CLI (substitute the correct region
endpoint for your B2 bucket):

```yaml
sync_command: "aws --endpoint-url https://s3.us-west-004.backblazeb2.com s3 cp {src} {dest}"
```

B2 Application Key ID and Application Key go in `~/.aws/credentials` (or the
standard `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` environment variables).

### rclone (requires a wrapper)

rclone uses `remote:bucket/path` syntax — it does **not** accept `s3://` URIs
directly. `rclone copy {src} {dest}` does not work as a `sync_command` when the
storage root is `s3://`.

If you prefer rclone, write a small wrapper script that rewrites the
`s3://bucket/key` path sqi passes into rclone's `remote:bucket/key` form, then
point `sync_command` at that script:

```sh
#!/usr/bin/env bash
# /usr/local/bin/sqi-rclone-sync
# Rewrite s3://bucket/key → RCLONE_REMOTE:bucket/key for rclone.
# Set RCLONE_REMOTE in the worker environment to the configured remote name.
: "${RCLONE_REMOTE:?set RCLONE_REMOTE to your rclone remote name}"
rewrite() {
  [[ "$1" == s3://* ]] && echo "${RCLONE_REMOTE}:${1#s3://}" || echo "$1"
}
exec rclone copy "$(rewrite "$1")" "$(rewrite "$2")"
```

```yaml
# chmod +x /usr/local/bin/sqi-rclone-sync first
sync_command: "/usr/local/bin/sqi-rclone-sync {src} {dest}"
```

### MinIO Client (`mc`) (requires a wrapper)

mc uses `alias/bucket/object` syntax — it does **not** accept `s3://` URIs
directly. `mc cp {src} {dest}` does not work as a `sync_command` when the
storage root is `s3://`.

If you prefer mc, use the same wrapper pattern, rewriting
`s3://bucket/key` → `MC_ALIAS/bucket/key`:

```sh
#!/usr/bin/env bash
# /usr/local/bin/sqi-mc-sync
# Rewrite s3://bucket/key → MC_ALIAS/bucket/key for mc.
# Set MC_ALIAS in the worker environment to the configured alias name.
: "${MC_ALIAS:?set MC_ALIAS to your mc alias name}"
rewrite() {
  [[ "$1" == s3://* ]] && echo "${MC_ALIAS}/${1#s3://}" || echo "$1"
}
exec mc cp "$(rewrite "$1")" "$(rewrite "$2")"
```

```yaml
# chmod +x /usr/local/bin/sqi-mc-sync first; configure alias first:
# mc alias set myminio http://minio.internal:9000 KEY SECRET
sync_command: "/usr/local/bin/sqi-mc-sync {src} {dest}"
```

For most MinIO deployments the `aws --endpoint-url` recipe above is simpler and
requires no wrapper.

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
