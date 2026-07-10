# sqi-worker Capability Tags Reference

Capability tags are key-value pairs reported to `sqi-server` at worker
registration time. Operators use them to write worker affinity rules in job
submissions, routing tasks to workers that have the required software or
hardware.

Tags appear in two forms:

- **List-style tags** — presence only; e.g., a manual `gpu` entry (the value
  is the empty string). Used for software labels and feature flags. (The
  built-in DCC detectors below are the exception: they emit `key=true` pairs,
  not empty-value presence tags — see [Capability
  auto-detection](#capability-auto-detection-built-in-dcc-detectors).)
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
| Linux | `/proc/driver/nvidia/gpus/` — NVIDIA kernel driver sysfs tree. Currently only NVIDIA GPUs are detected. |
| macOS | Not detected yet (requires CGo + IOKit). |
| Windows | Not detected yet (requires WMI or Direct3D). |

When no GPU is detected all GPU fields are zero/empty and the worker is not
reported as GPU-capable. Use the manual tag `gpu` to mark a worker as
GPU-capable on macOS or Windows.

---

## Capability auto-detection (built-in DCC detectors)

In addition to the hardware/OS tags above, `sqi-worker` runs a second,
declarative detection engine (`internal/worker/capabilities`) that looks for
installed creative applications — Maya, Nuke, Houdini, Blender — and
advertises a tag with value `"true"` automatically, with no per-worker
configuration. This makes the software actually installed on a worker visible
without hand-editing its config, and it's enough on its own to satisfy the
`anyOf: ["true"]` gate the six shipped reference presets declare — a standard
install matches those presets with zero configuration. Run
`sqi-worker capabilities` any time to see what was found. See
[`docs/dcc-submitters.md`](dcc-submitters.md#reference-presets) for how these
auto-detected tags relate to the reference presets.

### How it runs

Detection runs once, at process startup, before the worker's first
registration (`BuildWorkerCapabilities` in
[`cmd/sqi-worker/start.go`](https://github.com/uberware/sqi/blob/main/cmd/sqi-worker/start.go)):

1. Every **built-in detector** not named in `capabilities.disable` is
   evaluated.
2. Every **custom detector** under `capabilities.detect` is evaluated (these
   are validated at load time; a malformed custom detector — missing `tag`,
   zero checks, more than one check primitive set, an invalid `os` gate, or a
   non-compiling regex — fails worker startup with a clear error rather than
   silently skipping).
3. Manual `worker.capability_tags` are merged last and always win on a key
   collision (see [Manual capability tags](#manual-capability-tags) below).

Detection is declarative only — checks read the filesystem, `PATH`, an
environment variable, or (Windows) the registry; nothing is executed. It runs
once at startup, not on a timer, so installing or removing an application on a
running worker is only picked up on the next restart.

### Built-in detectors

Four built-ins ship embedded in the worker binary, one YAML file per
application under
[`internal/worker/capabilities/builtins/`](https://github.com/uberware/sqi/tree/main/internal/worker/capabilities/builtins):

| Tag | Checks | Version source |
|---|---|---|
| `maya` | Linux `path_glob /usr/autodesk/maya*/bin/maya`; macOS `path_glob /Applications/Autodesk/maya*/Maya.app`; Windows `registry HKLM\SOFTWARE\Autodesk\Maya` | `maya(?P<v>[0-9]+)` against the matched path |
| `houdini` | `exe hython` (any OS, if on `PATH`); Linux `path_glob /opt/hfs*/bin/houdini`; macOS `path_glob /Applications/Houdini/Houdini*/Frameworks/Houdini.framework`; Windows `registry HKLM\SOFTWARE\Side Effects Software\Houdini` | `(?:hfs\|Houdini)(?P<v>[0-9]+\.[0-9]+)` against the matched path |
| `nuke` | Linux `path_glob /usr/local/Nuke*/Nuke*`; macOS `path_glob /Applications/Nuke*/Nuke*.app`; Windows `registry HKLM\SOFTWARE\The Foundry\Nuke` | `Nuke([0-9]+\.[0-9]+)` against the matched path |
| `blender` | `exe blender` (any OS, if on `PATH`); macOS `path_glob /Applications/Blender.app`; Windows `registry HKLM\SOFTWARE\BlenderFoundation\Blender` | none (presence only — no `-<version>` tags) |

A `path_glob`/`exe` check's matched signal is a filesystem path, so version
extraction works there. A `registry` check's signal is the registry key
string itself (not a value read from the registry), so on Windows these
built-ins currently detect *presence* only — no `-<version>` variant — unless
a path-based check also matches on that host.

Every software tag a shipped reference preset requires
(`presets/dcc/*.yaml`'s `attr.worker.tag.<name>`) must be emitted by some
built-in detector — enforced by
`TestBuiltinDetectors_CoverPresets` in
[`internal/worker/capabilities/builtins_test.go`](https://github.com/uberware/sqi/blob/main/internal/worker/capabilities/builtins_test.go),
so a new default preset cannot ship without a matching detector (see
[Adding a new auto-detected tag](development.md#adding-a-new-capability-tag-to-auto-detection)
in the development guide).

### Tag and version model

A detector emits tags into the same `Tags` map as the hardware tags above:

- The bare `<tag>` (e.g. `maya`) is emitted once, with value `"true"`, the
  moment *any* of its checks matches — this matches the same `key=true`
  convention as a manual tag like `maya=true`, so `attr.worker.tag.<tag>`
  `anyOf: ["true"]` requirements are satisfied directly.
- `<tag>-<version>` (e.g. `maya-2025`) is emitted per **distinct** version
  string captured by the detector's `version.from` regex, also with value
  `"true"`.
- **Multi-version is supported.** A `path_glob` check can match several
  installs at once (e.g. `/opt/hfs20.0/…` and `/opt/hfs20.5/…` both present),
  so a single detector run can emit `houdini`, `houdini-20.0`, **and**
  `houdini-20.5` together — every distinct version found, not just one.

### Precedence: built-in → custom → manual

- Built-in detectors run before custom detectors; if a custom detector emits
  a tag also emitted by an (enabled) built-in, the built-in's evaluation runs
  first and the tag is already present, so the custom detector doesn't
  overwrite it. Since both detectors emit the same `"true"` value this has no
  practical effect on the tag itself, only on which detector is credited as
  the source in the `sqi-worker capabilities` output below.
- To fully replace a built-in's checks for a tag (e.g. because it misfires on
  a nonstandard host layout), also add the tag to `capabilities.disable` so
  only your custom detector's checks run.
- Manual `worker.capability_tags` are merged last via `MergeManualTags` and
  always overwrite a same-named key from any detector, built-in or custom —
  this is the escape hatch to suppress a misdetection (`maya=` clears it to
  empty) or to force the tag on a worker where the built-in checks don't fire
  (`maya=true` on a nonstandard install path).

### The `sqi-worker capabilities` command

Prints every tag the worker would currently advertise — auto-detected
(hardware and software) plus manual — with its source, without connecting to
a server:

```sh
sqi-worker capabilities
```

```text
TAG        VALUE   SOURCE
gpu        true    manual
houdini    true    builtin:houdini
os         linux   auto
os_version 22.04   auto
```

The `VALUE` column is the tag's advertised value (bare tags resolve to `true`;
hardware tags like `os`/`os_version` carry their detected string). The `SOURCE`
column is one of `auto` (the hardware probe's own `Tags` entries
— just `os` and, when detected, `os_version`), `builtin:<tag>`, `custom`, or
`manual`. This is the first thing to run
when a worker isn't picking up jobs you'd expect it to — it answers "why
isn't my worker getting Maya jobs?" without needing to connect to a server or
inspect logs. `sqi-worker start --dry-run` includes the same detected
capabilities as part of its larger config-and-capabilities summary.

---

## Writing custom detectors

Custom detectors are configured under `capabilities.detect` in the worker
config file (see
[`docs/worker-configuration.md`](worker-configuration.md#capabilities-software-auto-detection)
and
[`config/sqi-worker.example.yaml`](https://github.com/uberware/sqi/blob/main/config/sqi-worker.example.yaml)).
They use the exact same schema as the built-ins
([`internal/worker/capabilities/detector.go`](https://github.com/uberware/sqi/blob/main/internal/worker/capabilities/detector.go)),
so any built-in YAML under
[`internal/worker/capabilities/builtins/`](https://github.com/uberware/sqi/tree/main/internal/worker/capabilities/builtins)
doubles as a worked example.

```yaml
capabilities:
  detect:
    - tag: mytool                # required; the base tag key
      checks:                    # required; at least one; ANY match => present
        - exe: mytool            # found via a PATH lookup
        - path_glob: "/opt/mytool*/bin/mytool"   # >=1 filesystem match
        - env: HFS                # env var present, even if empty (bare form)
        - env: { name: HOUDINI_ROOT, matches: "hfs[0-9.]+" }  # env var + value regex
        - registry: 'HKLM\SOFTWARE\MyCompany\MyTool'  # Windows only
          os: windows             # optional per-check OS gate
      version:                    # optional
        from: "mytool(?P<v>[0-9.]+)"   # regex applied to each matched signal
  disable: [blender]              # turn off a misfiring built-in by tag name
```

### Detector schema

| Field | Required | Description |
|---|---|---|
| `tag` | yes | The base tag key this detector emits (case-insensitive at match time, but keep it lowercase by convention). |
| `checks` | yes, ≥ 1 | A list of probes; the detector matches if **any** check matches. |
| `checks[].exe` | one of these four per check | Resolved via a `PATH` lookup (like the shell `which`, using `exec.LookPath`). The signal is the resolved path. |
| `checks[].path_glob` | | A `filepath.Glob` pattern; every match is a separate signal, which is how multi-version detection works. |
| `checks[].env` | | An environment variable check. Bare form (`env: HFS`) requires the variable to be present in the environment (an empty value still counts as present). Mapping form (`env: {name: ..., matches: ...}`) additionally requires the value to match a regex. |
| `checks[].registry` | | A Windows registry key path; checked for existence only (no value read). Evaluates to false on Linux/macOS unless combined with an `os: windows` gate (harmless either way, since the underlying check is a no-op off Windows). |
| `checks[].os` | no | Restricts the check to one platform: `linux`, `darwin`, or `windows`. Omit to run the check on all platforms. |
| `version.from` | no | A regex applied to **each matched signal** of every check. The version is the named group `v` (`(?P<v>...)`) if present, otherwise the first capturing group. A signal that doesn't match contributes only the bare tag, no version tag. |

Exactly one of `exe` / `path_glob` / `env` / `registry` must be set per
check — a check with zero or more than one is rejected at load time, along
with a missing `tag`, zero `checks`, an invalid `os` value, or a `version.from`
/ `env.matches` regex that fails to compile. Built-ins go through the same
`Validate()` call at load time and are covered by tests that assert they
parse and validate cleanly, so a broken built-in YAML is caught in CI; custom
detectors are validated when the worker config is loaded at startup, and a
validation failure stops the worker from starting with a descriptive error
rather than silently dropping the detector.

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

There is currently no explicit "remove this field" mechanism for hardware
fields. Hardware-field overrides are planned for a future configuration option.

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

To gate a step on a worker tagged `maya=true` — auto-detected on any worker
with a standard Maya install (see [Capability
auto-detection](#capability-auto-detection-built-in-dcc-detectors) above), or
set manually via `capability_tags: [maya=true]` (see [Setting manual
tags](#setting-manual-tags) above) for a nonstandard install:

```yaml
steps:
  - name: Render
    hostRequirements:
      attributes:
        - name: attr.worker.tag.maya
          anyOf: ["true"]
```

This is the real syntax the six reference presets under `presets/dcc/` use
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
