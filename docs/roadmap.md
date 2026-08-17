# `sqi` Roadmap and Technical Architecture

> **Status:** v0.2.0 (Phase 2) released. Phase 3 (auth and multi-user) — including task isolation (run-as-user) — is complete and merged on `main`, unreleased ahead of the v0.3 release. Details may change with requests, feedback, and discoveries as development progresses.

This document provides technical detail on `sqi`'s architecture, core concepts, and development roadmap. For the vision and feature overview, see [README.md](index.md).

---

## Technology Stack

| Layer | Technology | Rationale |
|---|---|---|
| Server / scheduler / worker core | Go | Single static binary, trivial cross-platform builds, excellent networking and concurrency |
| Web UI | TypeScript + React | Standard modern web stack, works in any browser |
| Python client API | Python 3 | Scripting and pipeline integration; used by DCC submitter tools |
| DCC submitter widgets | Python + Qt (PySide6) | Cross-platform integration with DCC application environments |
| Embedded state store (simple mode) | SQLite | Zero configuration, single file, sufficient for small deployments |
| External state store (production mode) | PostgreSQL | Reliable, suitable for HA deployments |
| Message transport | NATS JetStream | Lightweight, embeddable, supports pull-based worker patterns |
| Container image | Docker (Alpine-based) | For worker deployment on cloud instances |

---

## Core Architecture

### Components

```
sqi-server
├── Scheduler          (job queue, task assignment, priority, dependencies)
├── REST + WebSocket API
├── Web UI             (embedded SPA)
├── Usage pool manager
├── Preset registry
└── State store        (SQLite embedded or PostgreSQL external)

sqi-worker
├── Worker agent       (pull-based, requests work via lease request/reply)
├── Task executor      (spawns and monitors processes)
├── Path resolver      (translates storage location names to paths)
└── Status reporter    (streams logs and status to server)

sqi-sdk (Python library)
└── REST API wrapper for scripted submission and pipeline integration

DCC submitters (Python + Qt)
└── Application-specific submission UIs
```

### Worker Model: Pull, Not Push

Workers pull work from the scheduler rather than receiving pushed assignments.

**Benefits:**
- Workers can be added and removed at any time without scheduler changes
- Cloud workers that auto-scale work naturally
- Simpler lifecycle management
- Better resilience to network disruptions

On a local network, workers locate the server automatically via mDNS broadcast — no address configuration required.

### Scheduler Design

The scheduler is intentionally stateless with respect to the running process — all durable state lives in the database. This enables:

- Restart and replacement without losing farm state
- Replication for high availability via leader election
- Clean separation of concerns

**Organizational hierarchy:**

- **Farm**: Top-level grouping representing the operation (or one facility in a larger org)
- **Queue**: Container for jobs within a farm; provides policy and routing boundaries
- **Job**: Work submitted to a queue
- **Step**: Component of a job with optional dependencies on other steps
- **Task**: Atomic unit of work (one process on one worker)

Configuration cascades: farm defaults → queue overrides, with retry policy (max attempts, retry delay, failure limit) resolved over four tiers — server default → farm → queue → job override. This avoids repeating settings across every job submission.

**Scheduling considers:** job priority, task dependencies, queue and farm policy (concurrency limits, scheduling mode), compute location affinity, worker capability tags (OS, GPU, installed software), and usage pool availability.
 - *Design:* ready tasks remain `ready` until a worker sends a core-NATS
    request to `work.lease.<queue>`. The server computes free cores
    (`CPUCount − Σ committed`), selects a priority-ordered batch that fits,
    atomically transitions the batch `ready → assigned` (stamping `assigned_at`
    only now), and replies. The `SQI_WORK` JetStream stream, `work.assign.<queue>`
    subject, and server dispatch loop are removed.
 - *CPU capacity:* `amount.worker.vcpu` `min` is a consumable per-task
    reservation; omitting it reserves the whole machine (one task per worker).
    The server tracks committed cores in the database; the ledger rebuilds
    instantly on restart.
 - *Deferred:* worker drain/headroom signal, head-of-line reservation for
    large tasks, `amount.worker.vcpu.max`, memory/GPU dimensions.

---

## Job Model and OpenJD

`sqi` adopts the [Open Job Description](https://github.com/OpenJobDescription/openjd-specifications) (OpenJD) format as its native job execution format.

**Benefits:**
- Studios authoring jobs for other OpenJD-compatible systems can submit to `sqi` unchanged, provided the template does not opt into an extension `sqi` has not implemented — those are rejected by design rather than accepted and misinterpreted (the official `EXPR` expression-language extension **is** implemented and supported) — `sqi` accepts every valid base-spec template in the official conformance suite, though it is still more permissive than the spec about rejecting *invalid* ones (tracked in `test/conformance/baseline.txt`, measured in [`docs/openjd-conformance.md`](openjd-conformance.md))
- Standardized path mapping, parameter spaces, and execution semantics
- Clear separation between job description and job authoring (the product system)

### Products and Presets

A **product** describes a class of work in terms of user-friendly parameters and how they map to commands. A **preset** is a ready-to-use product definition for a specific tool, installable from the community library.

**Built-in default products:**
- **Script**: Run an arbitrary shell command
- **Python**: Run a Python script with a specified interpreter
- **Container**: Run a Docker image

### Job Structure

- **Job**: Named work with owner, submitter, priority, project tag, and one or more steps
- **Step**: Component with dependency graph and a set of tasks
- **Task**: Atomic unit of work — one process on one worker
- **Session**: Ephemeral runtime environment in which tasks run, with setup/teardown hooks

Sessions enable efficiency: expensive setup (launching a container, downloading assets) is paid once per Session rather than once per task. A failed or canceled task terminates its Session — environments are exited cleanly and the working directory is removed — which keeps worker state predictable.

---

## Storage and Path Translation

### Named Storage Locations

Jobs reference storage by logical name rather than concrete paths. Workers resolve names to their local environment at execution time.

Example:

```yaml
name: nas_shows
# type is derived from roots ("mixed" here: filesystem + s3://) and is response-only
roots:
  default: /mnt/nas/shows
  windows_workers: Z:\shows
  cloud_aws_us_east: s3://studio-bucket/shows
```

### S3-Compatible Storage

sqi is a thin layer with respect to S3: it validates `s3://bucket[/prefix]`
roots, derives a storage location's `type` from its roots
(`filesystem`/`s3`/`mixed`), and translates/stages paths at run time. It embeds
no S3 client, stores no credentials or endpoint addresses, and moves no bytes
itself.

S3-backed data reaches a worker via two paths: (1) **mounted** — a FUSE tool
(mountpoint-s3, goofys, rclone mount) exposes the bucket as a plain filesystem
path, already supported with no extra configuration; or (2) **staged** — B4
`stage_locally` invokes the operator's `staging.sync_command` (e.g.
`aws s3 cp {src} {dest}`, `rclone copy {src} {dest}`, `mc cp {src} {dest}`) to
copy inputs to worker-local scratch before each task and outputs back after.
Credentials, endpoint URLs, and remote aliases live entirely in the operator's
per-worker tool configuration — sqi stores none of it.

Supported providers: AWS S3, Backblaze B2, Cloudflare R2, MinIO, and any other
S3-compatible store reachable by the operator's chosen sync tool.

### Path Translation Modes

Path translation rides the `SQI_PATH_TRANSLATION` extension and offers five
delivery mechanisms (deliveries execute in fixed order and are mutually
compatible — a product can declare all five):

- **`translation_file`** (preferred): Native OpenJD `pathmapping-1.0` file
  written into each Session, served via `{{Session.PathMappingRulesFile}}`.
  Applications that support OpenJD path mapping natively consume it directly.
- **`swap_in_place`**: String substitution of path parameters in the template.
  Universal for applications with no path-mapping support. sqi convenience,
  not in the OpenJD spec.
- **`command_flags`**: Individual `src`/`dest` pairs appended as command-line
  flags (e.g., Maya workspace remapping).
- **`environment`**: Path mappings delivered via an environment variable.
- **`stage_locally`**: Job-level PATH parameters staged to worker-local
  scratch before the run and copied back after, for cloud workers without
  direct access to source storage. Works with no worker configuration — an
  unconfigured worker falls back to a TEMP scratch directory and sqi's own
  built-in copy — but a farm spanning multiple compute locations needs an
  explicit `staging.scratch_dir` and `staging.sync_command`
  (`rsync`/`aws-cli`/etc.) for real remote transfer.

`swap_in_place` and `translation_file` are the default when no
`SQI_PATH_TRANSLATION` extension is declared. Full reference:
[`products.md`](products.md#path-translation) and
[`openjd-extensions/path-translation.md`](openjd-extensions/path-translation.md).

### What `sqi` does not do

`sqi` does not manage storage replication or synchronization between locations. Studios are responsible for ensuring data is present in cloud storage before jobs run. `sqi` provides the path translation and staging hooks to integrate with whatever sync solution the studio uses (rclone, AWS DataSync, rsync, etc.).

---

## Compute Locations and Worker Management

A **compute location** is a named grouping of workers that share the same storage root mappings. Examples: `onprem_linux`, `onprem_windows`, `cloud_aws_us_east`, `cloud_gcp_europe`.

Jobs and individual steps can declare affinity to a compute location. The scheduler ensures those tasks run on workers in the matching location.

**Worker capabilities and tags:** Workers self-report at registration — OS, OS version, installed software, GPU presence, GPU VRAM, available RAM, CPU count. Capability requirements can be declared either implicitly (via product definitions, which bake requirements into jobs) or explicitly (by manually configuring requirements on individual jobs). The scheduler reads these requirements from the job and matches tasks to workers whose capabilities satisfy them.

---

## State and Messaging

The database is the source of truth for all job, task, worker, and configuration state. SQLite is embedded within `sqi-server` (simple mode) or PostgreSQL runs as a separate instance (production mode).

NATS JetStream handles:
- Task status and log streaming (worker → server)
- Heartbeats and worker registration

Work leases use **core NATS** request/reply (not JetStream): the worker requests
work on `work.lease.<queue>` and the server replies with a batch it is
authorized to run (pull-based). Real-time UI updates reach web clients over
WebSocket, fanned out by the server after it ingests the JetStream messages.

NATS can run embedded within `sqi-server` (simple mode) or as a separate cluster (production mode).

---

## Development Roadmap

### Phase 1: Core (v0.1 — alpha) ✅ Released

**Goal:** Working farm, core job model, basic web UI.

- sqi-server: scheduler, REST API, WebSocket, embedded NATS, SQLite state
- sqi-worker: pull-based worker, bare metal process executor
- Farm and queue management: REST CRUD plus web UI to list, create, and edit farms and queues (jobs are submitted to a queue, so at least one farm and queue must exist)
- Basic web UI: dashboard, job list, worker list, log viewer, job submission
- Python client API
- Named storage locations (resolved path translation)
- Usage pool tracking (count-based)
- Simple all-in-one deployment
- Docker image for worker

### Phase 2: Products and Presets (v0.2) ✅ Released

- Product/preset definition system (YAML/JSON) — a thin catalog over OpenJD templates, with embedded Script/Python/Container built-ins
- Preset library integration — static JSON index at a configurable URL (default: official community library on GitHub Pages); browse presets in the Admin hub with per-preset status (not installed / installed / update available); preview the definition and install as a product (`source: installed`) in one click; SHA-256 integrity and update-detection check on install; read-only installed products, uninstallable, with Duplicate-to-custom available on every product
- Web UI product management editor and a product-driven submission form (parameter form generated from the selected product)
- Path translation deliveries (`swap_in_place`, `translation_file`, `command_flags`, `environment`, `stage_locally`) as the `SQI_PATH_TRANSLATION` vendor extension
- S3-compatible storage support (thin layer: derived type, root validation, path staging via operator sync tool)
- DCC submitter framework — in-application submitters for Maya, Houdini, Nuke, and Blender (the `sqi-submitter` Python package), built on the Python client
- Compute location registry and step-level affinity (native OpenJD `attr.worker.computelocation`)
- Chunk bounds (`SQI_CHUNK_BOUNDS` vendor extension) — expose each task chunk's frame start/end to the command line, used by the Maya, Blender and Mistika reference presets
- Auto-retry and failure limits — per-task retry policy (max attempts, retry delay) and a job-level failure ceiling that auto-parks a job, resolved over four tiers (server → farm → queue → job) with per-task attempt history
- Cross-job dependencies — a submission may declare `depends_on` upstream jobs (same farm, across queues); dependents are held `blocked` until every upstream completes, then released (or canceled if an upstream fails)
- Testing job presets — ready-to-run `test-render`/`test-steps` presets (bash and PowerShell) published to the preset library for smoke-testing a farm

### Phase 3: Auth and Multi-User (v0.3) — complete, unreleased

Authentication is **opt-in and off by default** — an unconfigured server behaves exactly as it did
before Phase 3. See [docs/auth.md](auth.md) for the model and setup.

- Local account auth (username/password, API keys)
- LDAP/AD integration
- Role-based access control (admin, operator, user, read-only)
- Owner/submitter distinction in job model
- OAuth2/OIDC support
- **Task isolation (run-as-user)** — queue-scoped: tasks in an isolated queue
  execute as an operator-configured OS account instead of the worker's own,
  closing the privilege-escalation gap the identity plane alone left open (a
  principal holding `jobs.write` could otherwise execute arbitrary code as the
  worker service account). Supported on POSIX and Windows — on Windows the
  worker must run as a LocalSystem service (or hold
  `SeAssignPrimaryTokenPrivilege`) to assume another account's identity. See
  [docs/auth.md](auth.md) and
  [docs/worker-configuration.md](worker-configuration.md) for the model,
  setup, and known gaps.

### Post-Phase-3, pre-v0.3: OpenJD `EXPR` and expanded reference presets — complete, unreleased

- **OpenJD `EXPR` extension** — the official expression-language extension is
  fully implemented and `StatusSupported`: expression core, type system,
  collections, comprehensions, the ~100-function standard library, path
  mapping, template integration (scopes, `let` bindings, bounded evaluation
  with operator-configurable limits), RFC 0007 extended parameter types, and
  the web `*_LIST` widgets. EXPR templates are accepted, submitted, dispatched
  and executed, with expressions resolved on the worker at phase 3. See
  [`docs/openjd-extensions/expr.md`](openjd-extensions/expr.md).
- **ffmpeg reference presets** — transcode, sequence-encode, and
  segment-transcode (bash/PowerShell/EXPR) added to `presets/sqi/`; the
  segment-transcode EXPR variant is the first shipped preset to declare
  `extensions: [EXPR]`.
- **Mistika reference presets** — Boutique, VR, and Workflows render presets
  added to `presets/sqi/`, each using the `SQI_CHUNK_BOUNDS` extension.

### Phase 4: Production Hardening (v0.4 — beta)

- PostgreSQL state store option
- Scheduler HA (leader election, warm standby)
- Distributed NATS cluster
- Worker auto-scaling hooks (AWS, GCP, Azure)
- Installer packages (Linux, macOS, Windows)

### Phase 5: LLM Plugin and Polish (v0.5 — RC)

- Optional LLM plugin (disabled by default)
- Provider-agnostic interface (OpenAI-compatible)
- Error log diagnosis feature
- Worker/job management by natural language
- Web Push notifications

### Phase 6: v1.0 Release

- Community preset library populated
- Full documentation
- Migration guides
- Dual licensing finalized (AGPL + commercial)

---

## Contributing

Code contributions aligned with the current development phase are most likely to be accepted quickly. See [CONTRIBUTING.md](contributing.md) for detailed guidelines on code, presets, documentation, and design discussion.
