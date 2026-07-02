# Storage locations

A **storage location** is a name for "the place your data lives." Jobs reference
it by name; each worker fills in its own real path at run time.

## Why this exists (start here if you have one machine)

On a single computer, a path like `D:\projects\shot01` just works — every program
sees the same drive. A render farm is different: it may have Linux machines,
Windows machines, and maybe cloud workers, and **the same data sits at a different
path on each** (`/mnt/nas/shot01`, `Z:\shot01`, `s3://studio/shot01`).

Instead of hard-coding one of those paths into your job (which then only runs in
one place), you give that storage a **name** and list where it lives on each kind
of worker. Your job refers to the name; sqi substitutes the right real path for
whichever worker picks up the task.

### Simplest possible example

Create one location:

- **Name:** `projects`
- **Roots:** `default → /mnt/studio/projects`
- **Type:** `filesystem` (derived from the root — response-only, not supplied on create)

In your job, reference a file inside it:

```
loc://projects/shot01/scene.hip
```

On every worker, sqi rewrites that to `/mnt/studio/projects/shot01/scene.hip`.
Add a second root later (e.g. `windows_workers → P:\projects`) and the same job
starts working on Windows workers too — no job changes.

> You do **not** need any storage locations to use sqi. If your jobs use plain
> absolute paths that are valid on every worker, skip this feature entirely.
> Storage locations are opt-in, and there is no default location.

## Reference

### The `loc://` URI

```
loc://<location-name>/<relative-path>
```

Use it anywhere in an OpenJD template: command, arguments, environment variable
values, task parameters, and embedded-file data.

### Roots and resolution

`roots` maps a **compute-location name** (or the special key `default`) to a
concrete root path. At dispatch sqi resolves a reference for the worker that will
run the task by:

1. using the root for the worker's compute location if one is defined; else
2. falling back to the `default` root; else
3. failing the task.

`default` is therefore the universal fallback.

### Translation mechanisms

sqi applies two complementary mechanisms automatically:

- **Resolved substitution** — `loc://` references are rewritten to concrete paths
  before the command is built, so applications need no path-mapping support.
- **OpenJD `path_mapping.json`** — a standard OpenJD path-mapping file is written
  into the session working directory for applications that consume it natively.

### Submit-time coverage validation

When you submit a job, sqi checks every `loc://` reference:

- The named location must exist.
- A **step** that declares compute-location affinity `X` is accepted if the
  location has a root for `X` **or** a `default` root.
- A **step with no affinity** requires a `default` root (it can run anywhere).
- A reference at **job scope** — in a job environment or a job-parameter
  default — requires a `default` root, because job environments may run on
  workers in any compute location.

Submissions that fail these checks are rejected with a message naming the
location and what is missing.

### Resolution / coverage matrix

| Step affinity | Roots present | Submit | Worker that runs it | Dispatch resolves to |
|---|---|---|---|---|
| none | `default` only | accepted | any (empty / X / Y) | `default` |
| none | compute-specific only (no `default`) | rejected at submit | — | — |
| none | compute-specific + `default` | accepted | X → X-specific; others → `default` | as noted |
| affinity X | X-specific only | accepted | X only | X-specific |
| affinity X | `default` only | accepted | X only | `default` (fallback) |
| affinity X | neither | rejected at submit | — | — |

The scheduler only assigns an affinity-`X` step to a worker whose compute
location is `X`; a no-affinity step can run on any worker, which is why it needs a
`default`.

### Storage types

`type` is a **read-only, derived** field computed from the location's roots:
`filesystem` when all roots are filesystem paths (or there are no roots), `s3`
when all roots are `s3://` URIs, and `mixed` when roots span both schemes.
Supplying `type` on create or update returns HTTP 400 — set the roots and sqi
infers the type automatically.

A `mixed` location is the normal shape for a farm that uses on-prem NAS for
some workers and `s3://` roots for cloud workers.

### What sqi does not do

sqi does **not** replicate or synchronize data between locations. Ensure data is
present in the target storage before jobs run.

S3 I/O — copying files to and from object storage — is handled by the
operator's sync tool (e.g. `aws s3 cp`, `rclone`, `mc`) through the B4
[`stage_locally`](openjd-extensions/path-translation.md) delivery. sqi invokes
the operator-configured `staging.sync_command`; it stores no S3 credentials or
endpoint addresses.

See [`docs/storage-s3.md`](storage-s3.md) for S3 setup, per-provider recipes,
and the mounted vs staged decision guide.

See also: [`openjd-submission.md`](openjd-submission.md) and
[`architecture.md`](architecture.md).
