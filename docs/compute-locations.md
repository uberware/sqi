<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
# Compute locations

A **compute location** is a name for "where workers run." Workers declare
their location at startup; steps can restrict tasks to a specific location via
an OpenJD host requirement.

## Why this exists

In a multi-site or multi-environment render farm, workers may run in different
physical locations, cloud regions, or isolated networks — a workstation on
premises, a cloud burst pool, and a shared render room are three distinct
compute locations. Naming these environments lets you:

- Restrict steps to workers in a specific location
  (`attr.worker.computelocation`).
- Map storage paths correctly per location — storage-location roots are keyed
  by compute-location name (see [Storage locations](storage-locations.md)).
- See at a glance which locations have active workers (`worker_count`).

> You do **not** need to configure compute locations to use sqi. A worker
> with no `compute_location` set is eligible for any step that declares no
> location requirement. Compute locations are opt-in.

## How names are managed

### Auto-registration

When a worker starts and reports a non-empty `compute_location`, sqi
automatically creates a registry entry for that name if one does not already
exist. You do not need to pre-create a location before deploying workers.
The auto-registration is:

- **Idempotent** — if the entry already exists, it is left exactly as-is,
  including any description you have curated.
- **Best-effort** — a failure to auto-register (for example, a transient
  database error or a create that races another registration) is logged and
  silently ignored; it never blocks or fails the worker registration itself.

This behaviour is implemented in `ensureComputeLocation` in
`internal/scheduler/scheduler.go`, called on every successful worker
registration.

### Curating entries

Use the REST API (`/api/v1/compute-locations`) or the web UI
(Admin → Compute Locations) to:

- **Create** a location with a description before deploying workers.
- **Update** the name or description of an existing location.
- **Delete** a location that is no longer needed.

Name rules: no whitespace, forward slashes, or quote characters. Names are
stored exactly as given and matched case-sensitively by the scheduler and the
`worker_count` filter — `Studio-A` and `studio-a` are two distinct locations.
Use consistent casing across worker configuration and step host requirements.

### Non-blocking deletes and reappearance

Deleting a compute location removes the catalog entry only. It does not
affect running workers, their in-flight tasks, or their heartbeat cycles. If
an online worker whose `compute_location` matches the deleted name sends
another registration message (which happens on every worker startup and
reconnect), the entry reappears automatically.

Use the `worker_count` field returned by `GET /api/v1/compute-locations` and
`GET /api/v1/compute-locations/{id}` to check whether a location is still in
use. `worker_count` is the number of currently **online** workers that report
that location name. Deleting a location with `worker_count > 0` is
non-destructive but the entry is likely to reappear within seconds.

## Relationship to storage locations

The compute-location name is the same string that keys a storage location's
`Roots` map. When a task runs on a worker whose `compute_location` is
`onprem_linux`, the server resolves `loc://` URI references using the
`onprem_linux` root in each storage location (falling back to `default` when
that key is absent).

Both the compute-location registry and storage-location `Roots` keys use the
same name space. Creating a compute location does not automatically create a
storage-location root; you manage those separately.

See [`docs/storage-locations.md`](storage-locations.md) for root resolution
rules and the full coverage matrix.

## Step affinity and scheduling

A step can be restricted to workers in a specific compute location via the
standard OpenJD host requirement:

```yaml
hostRequirements:
  attributes:
    - name: "attr.worker.computelocation"
      anyOf: ["onprem_linux"]
```

sqi promotes the value (when exactly one is declared in anyOf or allOf) to
the step's `compute_location` field. The scheduler matcher (`internal/scheduler/matcher.go`)
then checks whether the worker's `compute_location` matches the step's value
before assigning the task. A step with no `attr.worker.computelocation`
requirement can run on a worker in any compute location (including workers with
an empty location).

**There is no job-level compute-location affinity** — affinity is per-step
only.

### The registry is a catalog only

The compute-location registry does **not** gate scheduling. A worker whose
`compute_location` value does not appear in the registry is still eligible for
tasks that match that value; a registry entry that has no matching worker
simply has `worker_count: 0`. What matters to the scheduler is the string a
worker self-reports at registration and the string a step declares in its host
requirement — the registry has no influence on either.

The scheduler matcher was not changed by the A2 feature that introduced the
registry. The matcher keys on the raw `step.ComputeLocation` string exactly
as it did in Phase 1.

## REST endpoints

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/v1/compute-locations` | List all locations (includes `worker_count`) |
| `POST` | `/api/v1/compute-locations` | Create a location |
| `GET` | `/api/v1/compute-locations/{id}` | Get a location by ID (includes `worker_count`) |
| `PUT` | `/api/v1/compute-locations/{id}` | Update name/description |
| `DELETE` | `/api/v1/compute-locations/{id}` | Delete a location (non-blocking) |

See [`docs/api.md`](api.md) for request/response shapes and the OpenAPI
specification at `internal/api/openapi.yaml`.

See also: [`docs/storage-locations.md`](storage-locations.md) and
[`docs/worker-configuration.md`](worker-configuration.md).
