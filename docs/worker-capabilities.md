# sqi-worker Capability Tags Reference

Capability tags are key-value pairs reported to `sqi-server` at worker
registration time. Operators use them to write worker affinity rules in job
submissions, routing tasks to workers that have the required software or
hardware.

Tags appear in two forms:

- **List-style tags** — presence only; e.g., `maya-2025` (the value is the
  empty string). Used for software labels and feature flags.
- **Key=value tags** — e.g., `os=linux`, `os_version=22.04`. The value adds
  context for range-based matching rules.

---

## Auto-detected tags

The following tags are populated automatically at startup via the
`internal/worker/capabilities` package. They reflect the state of the host
machine at the moment the worker process launches.

### `os`

The operating system identifier as reported by `runtime.GOOS`:

| Value | Platform |
|---|---|
| `linux` | Linux |
| `darwin` | macOS |
| `windows` | Windows |

**Detection:** Always present; derived from the compiled Go runtime constant
`runtime.GOOS`.

**Example:** `os=linux`

---

### `os_version`

Human-readable OS release string. Omitted when detection fails.

| Platform | Source | Example value |
|---|---|---|
| Linux | `/etc/os-release` — `VERSION_ID` field (falls back to `VERSION`) | `22.04` |
| macOS | `kern.osproductversion` sysctl | `14.5` |
| Windows | `RtlGetVersion` from `ntdll.dll` | `10.0.22621` |

**Detection:** Best-effort; omitted if the OS release file is unreadable or
the sysctl call fails.

**Example:** `os_version=22.04`

---

### `cpu_count`

Number of logical (hardware-thread) CPUs available to the worker process.

**Detection:** `runtime.NumCPU()` on all platforms. Reflects the number of
CPUs visible to the process; on containerized workers this is the cgroup CPU
quota rather than the full host core count.

**Example:** Used as a numeric value in heartbeat messages and registration.
Not stored as a string tag but reported in registration metadata.

---

### `ram_mb`

Total installed physical RAM in mebibytes (MiB).

| Platform | Source |
|---|---|
| Linux | `/proc/meminfo` — `MemTotal` field (converted from kB) |
| macOS | `hw.memsize` sysctl (8-byte little-endian uint64, converted to MiB) |
| Windows | `GlobalMemoryStatusEx` from `kernel32.dll` |

**Detection:** Best-effort; reported as `0` if detection fails.

---

### GPU fields

GPU hardware is reported in registration metadata (not as string tags) and
covers:

| Field | Description |
|---|---|
| `gpu.vendor` | GPU vendor string, e.g. `"NVIDIA"` |
| `gpu.model` | GPU model name from the driver, e.g. `"RTX 4090"` |
| `gpu.vram_mb` | Total VRAM of the primary GPU in MiB |
| `gpu.count` | Number of GPUs detected |

| Platform | Source |
|---|---|
| Linux | `/proc/driver/nvidia/gpus/` — NVIDIA kernel driver sysfs tree. Only NVIDIA GPUs are detected in Phase 1. |
| macOS | Not detected in Phase 1 (requires CGo + IOKit; deferred to a future phase). |
| Windows | Not detected in Phase 1 (requires WMI or Direct3D; deferred to a future phase). |

When no GPU is detected all GPU fields are zero/empty and the worker is not
reported as GPU-capable. Use the manual tag `gpu` to mark a worker as
GPU-capable on macOS or Windows.

---

## Manual capability tags

Manual tags are set in configuration (or via the `SQI_WORKER_CAPABILITY_TAGS`
environment variable) and merged with auto-detected capabilities at
registration time. Manual tags always overwrite any auto-detected tag with
the same key.

### Setting manual tags

**Via config file:**

```yaml
worker:
  capability_tags:
    - maya-2025
    - arnold-7
    - gpu
    - highram
    - maya=true   # key=value: sets Tags["maya"] = "true"
```

**Via environment variable (comma-separated):**

```sh
SQI_WORKER_CAPABILITY_TAGS=maya-2025,arnold-7,gpu,maya=true sqi-worker start
```

**Verify before connecting:**

```sh
sqi-worker start --dry-run
```

The dry-run output includes a `tags:` section listing all tags (auto-detected
and manual) that would be sent in the registration message.

---

## Common manual tag conventions

These are not enforced by the software — they are conventions that make
affinity rules in job submissions readable and consistent.

| Tag | Meaning |
|---|---|
| `gpu` | Host has a usable GPU (use on macOS/Windows where auto-detection is limited) |
| `highram` | Host has significantly more RAM than typical (operator-defined threshold) |
| `maya-<version>` | Maya is installed at the specified version, e.g. `maya-2025` |
| `houdini-<version>` | Houdini is installed, e.g. `houdini-20.5` |
| `blender-<version>` | Blender is installed, e.g. `blender-4.2` |
| `nuke-<version>` | Nuke is installed, e.g. `nuke-15.0` |
| `arnold-<version>` | Arnold renderer, e.g. `arnold-7` |
| `karma-renderer` | Karma (Houdini's renderer) is available |
| `deadline-slave` | Legacy Deadline Slave running alongside sqi (for mixed farms) |
| `nas-<location>` | Host has direct access to a specific NAS, e.g. `nas-studio-a` |
| `licensed` | Host has a floating license checked out |

---

## Overriding and suppressing auto-detected tags

Manual tags are merged into the `Tags` map (a `map[string]string` keyed by
tag name). A manual tag overwrites an auto-detected entry only when the map
key is identical.

Each `capability_tags` entry is parsed by `MergeManualTags`:

- A bare entry with no `=` is stored as a presence-only key with an empty
  value, e.g. `maya-2025` → `Tags["maya-2025"] = ""`.
- A `key=value` entry is split on the **first** `=`, e.g. `maya=true` →
  `Tags["maya"] = "true"`. A value may itself contain `=` (only the first
  one splits the key from the value), and a trailing `=` with nothing after
  it (`maya=`) stores an empty value, same as the presence-only form.
- An entry with an empty key (starting with `=`, e.g. `=true`) is skipped —
  an empty tag key can never satisfy an `attr.worker.tag.<key>` requirement.

**Auto-detected string tags** — `os` and `os_version` — are stored in `Tags`
and can be overridden by adding a manual tag with the same exact key:

```yaml
worker:
  capability_tags:
    - os=freebsd  # replaces Tags["os"] with "freebsd"
    - os_version  # presence-only: clears the auto-detected value to ""
```

To override `os_version` with a specific value, use `os_version=<value>`; to
just clear it, add the bare `os_version` entry (which sets
`Tags["os_version"] = ""`).

**Hardware values** — `RAMMb`, `CPUCount`, and GPU fields — are reported as
typed struct fields in the registration message, **not** as entries in the
`Tags` map. They cannot be overridden through `capability_tags`. If you need
to suppress or correct these values, the current option is to set
`SQI_WORKER_CAPABILITY_TAGS` to a flag readable by the server's job affinity
rules, such as `no-gpu`, to annotate the worker without relying on the
auto-detected field.

There is no explicit "remove this field" mechanism for hardware fields in
Phase 1. Hardware-field overrides are planned for a future configuration
option.

---

## Using capability tags in job submissions

Affinity rules in OpenJD job definitions live under a step's
`hostRequirements.attributes`, each entry a `name` + an `anyOf` (or `allOf`)
list of acceptable string values. A custom worker tag is addressed as
`attr.worker.tag.<key>`, matched case-insensitively against
`Capabilities.Tags[<key>]` (see [`internal/scheduler/matcher.go`](https://github.com/uberware/sqi/blob/main/internal/scheduler/matcher.go)).
Any attribute name outside the well-known set (`attr.worker.os.family`,
`attr.worker.os.version`, `attr.worker.computelocation`) and the
`attr.worker.tag.<key>` namespace always resolves to `""` and can never
match.

To gate a step on a worker tagged `maya=true` (set via `capability_tags:
[maya=true]`, see [Setting manual tags](#setting-manual-tags) above):

```yaml
steps:
  - name: Render
    hostRequirements:
      attributes:
        - name: attr.worker.tag.maya
          anyOf: ["true"]
```

This is the real syntax the four reference presets under `presets/dcc/` use
— see [`docs/dcc-submitters.md`](dcc-submitters.md#reference-presets).

The server's scheduler matches the required attribute against registered
workers' `Tags` map and routes each task only to workers that satisfy every
`hostRequirements` attribute. Workers missing the tag entirely, or whose
`Tags[<key>]` doesn't appear in `anyOf`, are not eligible for that task.

---

## Adding a new auto-detected tag

To add a new hardware or OS-level detection to the sqi-worker itself, see
the development guide:
[`docs/development.md` — Adding a new capability tag](development.md#adding-a-new-capability-tag-to-auto-detection).

---

## See also

- [`docs/worker-configuration.md`](worker-configuration.md) — `worker.capability_tags` option.
- [`docs/development.md`](development.md) — How to add a new auto-detected capability tag to the source.
- [`internal/worker/capabilities/`](https://github.com/uberware/sqi/tree/main/internal/worker/capabilities) — Source for auto-detection logic.
