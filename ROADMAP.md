# `sqi` Roadmap and Technical Architecture

> **Status:** Pre-release. Active development beginning mid-2026. Details may change with requests, feedback, and discoveries as development progresses

This document provides technical detail on `sqi`'s architecture, core concepts, and development roadmap. For the vision and feature overview, see [README.md](README.md).

---

## Technology Stack

| Layer | Technology | Rationale |
|---|---|---|
| Server / scheduler / worker core | Go | Single static binary, trivial cross-platform builds, excellent networking and concurrency |
| Web UI | TypeScript + React or Svelte | Standard modern web stack, works in any browser |
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
├── Worker agent       (pull-based, polls for assigned tasks)
├── Task executor      (spawns and monitors processes)
├── Path resolver      (translates storage location names to paths)
└── Status reporter    (streams logs and status to server)

sqi-client (Python library)
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

Configuration cascades: farm defaults → queue overrides. This avoids repeating settings across every job submission.

**Scheduling considers:** job priority, task dependencies, queue and farm policy (concurrency limits, scheduling mode), compute location affinity, worker capability tags (OS, GPU, installed software), and usage pool availability.

---

## Job Model and OpenJD

`sqi` adopts the [Open Job Description](https://github.com/OpenJobDescription/openjd-specifications) (OpenJD) format as its native job execution format.

**Benefits:**
- Studios authoring jobs for other OpenJD-compatible systems can submit to `sqi` unchanged
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
type: filesystem
roots:
  default: /mnt/nas/shows
  windows_workers: Z:\shows
  cloud_aws_us_east: s3://studio-bucket/shows
```

### S3-Compatible Storage

Native support for S3-compatible object storage: AWS S3, Backblaze B2, Cloudflare R2, MinIO, and others.

### Path Translation Modes

- **OpenJD** (preferred): Standard path mapping file written into each Session. Applications that support OpenJD natively consume it directly.
- **Resolved**: All paths resolved to concrete paths before command construction. Universal for applications with no path mapping support.
- **Command arg**: Path pairs passed as explicit arguments (e.g., Maya workspace remapping).
- **Environment**: Path mappings via environment variables.
- **Staged**: Pre-job staging to worker-local storage for cloud workers without direct access to source storage.

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
- Work assignment messages (server → worker)
- Task status and log streaming (worker → server)
- Real-time UI updates (server → web clients via WebSocket)
- Heartbeats and worker registration

NATS can run embedded within `sqi-server` (simple mode) or as a separate cluster (production mode).

---

## Development Roadmap

### Phase 1: Core (v0.1 — alpha)

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

### Phase 2: Products and Presets (v0.2)

- Product/preset definition system (YAML/JSON)
- Preset library integration (browse and install from community repo)
- Web UI product form editor
- Additional path translation modes
- S3-compatible storage support
- DCC submitter framework (Maya, Houdini) — built on the Python client
- Compute location configuration and job affinity
- Product limits (concurrency caps tied to a product/preset, building on Phase 1 usage pool tracking)

### Phase 3: Auth and Multi-User (v0.3)

- Local account auth (username/password, API keys)
- LDAP/AD integration
- Role-based access control (admin, operator, user, read-only)
- Owner/submitter distinction in job model
- OAuth2/OIDC support
- User limits (per-user concurrent task caps)
- Custom limits (end-user-configurable limit dimensions, e.g. show or client code)

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
- User settings (theme, list refresh interval, per-user preferences)

### Phase 6: v1.0 Release

- Community preset library populated
- Full documentation
- Migration guides
- Dual licensing finalized (AGPL + commercial)

---

## Contributing

Code contributions aligned with the current development phase are most likely to be accepted quickly. See [CONTRIBUTING.md](CONTRIBUTING.md) for detailed guidelines on code, presets, documentation, and design discussion.
