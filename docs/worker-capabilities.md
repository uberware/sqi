# sqi-worker Capability Tags Reference

Capability tags are key-value pairs reported to `sqi-server` at worker
registration time. Operators use them to write worker affinity rules in job
submissions, routing tasks to workers that have the required software or
hardware.

Tags appear in two forms:

- **List-style tags**: presence only, such as a manual `gpu` entry, whose value
  is the empty string. Used for software labels and feature flags. The
  built-in detectors below instead emit `key=true` pairs (see [Capability
  auto-detection](#capability-auto-detection-built-in-dcc-detectors)).
- **Key=value tags**: such as `os=linux` or `os_version=22.04`, where the value
  carries information a matching rule can test.

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

**Detection:** `runtime.NumCPU()` on all platforms: the logical CPUs the
process may run on. On Linux that is the process's CPU affinity set, so a
container pinned with `--cpuset-cpus` reports the pinned CPUs, while a CPU
quota such as `--cpus` does not lower the count.

Reported as a number in registration metadata, not as a string tag.

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

In addition to the hardware and OS tags above, `sqi-worker` runs a declarative
detection engine (`internal/worker/capabilities`) that looks for installed
applications (Maya, Nuke, Houdini, Blender, Cinema 4D, 3ds Max, After Effects,
Mistika Boutique/Ultima, VR and Workflows, ffmpeg, and the standalone
renderers below) and advertises a tag with value `"true"` for each one found,
with no per-worker configuration. That satisfies the `anyOf: ["true"]` gate the
shipped reference presets declare, so a worker with a standard install matches
those presets as is. Run `sqi-worker capabilities` to see what was found. See
[`docs/dcc-submitters.md`](dcc-submitters.md#reference-presets) for how these
tags relate to the reference presets.

### How it runs

Detection runs once, at process startup, before the worker's first
registration (`capabilities.BuildWorkerCapabilities`, called from
[`cmd/sqi-worker/start.go`](https://github.com/uberware/sqi/blob/main/cmd/sqi-worker/start.go)):

1. Every **built-in detector** not named in `capabilities.disable` is
   evaluated.
2. Every **custom detector** under `capabilities.detect` is evaluated. A
   malformed custom detector (missing `tag`, no checks, more than one check
   primitive in a check, an invalid `os` gate, or a regex that does not
   compile) stops the worker from starting with an error.
3. Manual `worker.capability_tags` are merged last and win on a key collision
   (see [Manual capability tags](#manual-capability-tags) below).

Checks only read the filesystem, `PATH`, an environment variable or (on
Windows) the registry; nothing is executed. Because detection runs only at
startup, installing or removing an application takes effect when the worker
restarts.

### Built-in detectors

Nineteen built-ins ship embedded in the worker binary, one YAML file per
application under
[`internal/worker/capabilities/builtins/`](https://github.com/uberware/sqi/tree/main/internal/worker/capabilities/builtins):

| Tag | Checks | Version source |
|---|---|---|
| `maya` | Linux `path_glob /usr/autodesk/maya*/bin/maya`; macOS `path_glob /Applications/Autodesk/maya*/Maya.app`; Windows `registry HKLM\SOFTWARE\Autodesk\Maya` | `maya(?P<v>[0-9]+)` against the matched path |
| `houdini` | `exe hython` (any OS, if on `PATH`); Linux `path_glob /opt/hfs*/bin/houdini`; macOS `path_glob /Applications/Houdini/Houdini*/Frameworks/Houdini.framework`; Windows `registry HKLM\SOFTWARE\Side Effects Software\Houdini` | `(?:hfs\|Houdini ?)(?P<v>[0-9]+\.[0-9]+)` against the matched path |
| `nuke` | Linux `path_glob /usr/local/Nuke*/Nuke*`; macOS `path_glob /Applications/Nuke*/Nuke*.app`; Windows `registry HKLM\SOFTWARE\The Foundry\Nuke` | `Nuke([0-9]+\.[0-9]+)` against the matched path |
| `blender` | `exe blender` (any OS, if on `PATH`); macOS `path_glob /Applications/Blender.app`; Windows `registry HKLM\SOFTWARE\BlenderFoundation\Blender` | none (presence only, no `-<version>` tags) |
| `cinema4d` | Linux `path_glob /opt/maxon/cinema4dr*/bin/Commandline`; macOS `path_glob /Applications/Maxon Cinema 4D */Commandline.app`; Windows `path_glob C:\Program Files\Maxon Cinema 4D *\Commandline.exe` | `Maxon Cinema 4D (?P<v>[0-9]{4})` against the matched path (a Linux match carries no year, so no version) |
| `3dsmax` | `exe 3dsmaxcmd` (any OS, if on `PATH`); Windows `registry HKLM\SOFTWARE\Autodesk\3dsMax`; Windows `path_glob C:\Program Files\Autodesk\3ds Max *\3dsmaxcmd.exe` | `3ds Max (?P<v>[0-9]{4})` against the matched path |
| `aftereffects` | `exe aerender` (any OS, if on `PATH`); macOS `path_glob /Applications/Adobe After Effects */aerender`; Windows `path_glob C:\Program Files\Adobe\Adobe After Effects *\Support Files\aerender.exe` | `After Effects (?P<v>[0-9]{4})` against the matched path |
| `ffmpeg` | `exe ffmpeg` (any OS, if on `PATH`); no install glob or registry probe | none (presence only) |
| `mistika` | Linux `path_glob /home/mistika/SGO Apps/Mistika Ultima*/bin/mistika`; macOS `path_glob /Applications/SGO Apps/Mistika Boutique.app*/Contents/MacOS/mistika`; Windows `path_glob C:\Program Files\SGO Apps\Mistika Boutique*\bin\mistika.exe` | `Mistika (?:Ultima\|Boutique) ?(?P<v>[0-9]+\.[0-9]+)` against the matched path |
| `mistikavr` | Linux `path_glob /home/mistika/SGO Apps/Mistika VR*/bin/vr`; macOS `path_glob /Applications/SGO Apps/Mistika VR.app*/Contents/MacOS/vr`; Windows `path_glob C:\Program Files\SGO Apps\Mistika VR*\bin\vr.exe` | `Mistika VR ?(?P<v>[0-9]+\.[0-9]+)` against the matched path |
| `mistikaworkflows` | Linux `path_glob /home/mistika/SGO Apps/Mistika Workflows*/bin/workflows`; macOS `path_glob /Applications/SGO Apps/Mistika Workflows.app*/Contents/MacOS/workflows`; Windows `path_glob C:\Program Files\SGO Apps\Mistika Workflows*\bin\workflows.exe` | `Mistika Workflows ?(?P<v>[0-9]+\.[0-9]+)` against the matched path |
| `arnold` | `exe kick` (any OS); Linux `path_glob /usr/autodesk/arnold/maya*/bin/kick`; Windows `path_glob C:\Program Files\Autodesk\Arnold\maya*\bin\kick.exe` (the copy MtoA installs) | none (presence only; the paths carry the Maya version, not Arnold's) |
| `vray` | `exe vray` (any OS); Linux `path_glob /usr/Chaos/V-Ray/Standalone_for_*/bin/vray`; Windows `path_glob C:\Program Files\Chaos\V-Ray\Standalone for x64\bin\vray.exe` | none |
| `husk` | `exe husk` (any OS); Linux `path_glob /opt/hfs*/bin/husk`; Windows `path_glob C:\Program Files\Side Effects Software\Houdini *\bin\husk.exe` | `(?:hfs\|Houdini ?)(?P<v>[0-9]+\.[0-9]+)` against the matched path |
| `mantra` | `exe mantra` (any OS); Linux `path_glob /opt/hfs*/bin/mantra`; Windows `path_glob C:\Program Files\Side Effects Software\Houdini *\bin\mantra.exe` | `(?:hfs\|Houdini ?)(?P<v>[0-9]+\.[0-9]+)` against the matched path |
| `redshift` | `exe redshiftCmdLine` (any OS); Linux `path_glob /usr/redshift/bin/redshiftCmdLine`; macOS `path_glob /Applications/Maxon Redshift */bin/redshiftCmdLine`; Windows `path_glob C:\Program Files\Maxon Redshift *\bin\redshiftCmdLine.exe` | `Maxon Redshift (?P<v>[0-9]{4})` against the matched path |
| `renderman` | `exe prman` (any OS); `env RMANTREE`; `path_glob RenderManProServer-*/bin/prman` under `/opt/pixar` (Linux), `/Applications/Pixar` (macOS) and `C:\Program Files\Pixar` (Windows) | `RenderManProServer-(?P<v>[0-9]+\.[0-9]+)` against the matched path or the `RMANTREE` value |
| `3delight` | `exe renderdl` (any OS); `env DELIGHT`; macOS `path_glob /Applications/3Delight/bin/renderdl`; Windows `path_glob C:\Program Files\3Delight\bin\renderdl.exe` | none |
| `oiiotool` | `exe oiiotool` (any OS, if on `PATH`) | none (presence only) |

The three Mistika detectors carry **no `exe` check**: their binaries are
`mistika`, `vr` and `workflows`, and a bare `PATH` probe for the last two would
tag workers that have no Mistika at all. They detect by install location only,
but the presets invoke the bare command, so the binary must also be on the
worker's `PATH` for the job to run.

The `cinema4d` detector also carries **no `exe` check**. Its preset invokes the
bare command `Commandline`, a name generic enough that a `PATH` probe could tag
a worker with no Cinema 4D installed, which would then be handed render work it
cannot do. It matches the default Maxon install folders only; tag a Cinema 4D
installed elsewhere by hand with `cinema4d=true` under
`worker.capability_tags`. The `3dsmax` and `aftereffects` detectors do probe
`PATH` for `3dsmaxcmd` and `aerender`, names specific to the product. For all
three, a match on an install location (or, for `3dsmax`, the registry) tags the
worker even when the command is not on `PATH`, but the presets invoke the bare
command, so it must be on the worker's `PATH` for the job to run. A `3dsmax`
registry match carries no year, so it tags the worker without a version.

Each standalone-renderer detector checks `PATH` for the command its preset
runs, plus the install locations listed in the table. `husk` and `mantra` have
their own tags, separate from `houdini`, because a worker can have `hython`
without either renderer on `PATH`. The `redshift` tag marks the Redshift
**install**, not a usable GPU; GPU detection covers only NVIDIA on Linux (see
[GPU fields](#gpu-fields)). On a host with Redshift but no usable GPU, list
`redshift` under `capabilities.disable`.

A `path_glob` or `exe` check's matched signal is a filesystem path, so version
extraction works there. A `registry` check's signal is the registry key string
itself, not a value read from the registry, so a registry match on Windows
(`maya`, `houdini`, `nuke`, `blender`, `3dsmax`) yields the bare tag without a
`-<version>` variant unless a path-based check also matches on that host.

Every software tag a shipped reference preset requires
(`presets/sqi/*.yaml`'s `attr.worker.tag.<name>`) must be emitted by a
built-in detector. `TestBuiltinDetectors_CoverPresets` in
[`internal/worker/capabilities/builtins_test.go`](https://github.com/uberware/sqi/blob/main/internal/worker/capabilities/builtins_test.go)
enforces this, so a new default preset cannot ship without one (see
[Adding a new auto-detected tag](development.md#adding-a-new-capability-tag-to-auto-detection)
in the development guide).

### Tag and version model

A detector emits tags into the same `Tags` map as the hardware tags above:

- The bare `<tag>` (e.g. `maya`) is emitted once, with value `"true"`, when
  any of its checks matches. That is the same `key=true` form as a manual tag
  like `maya=true`, so it satisfies an `attr.worker.tag.<tag>`
  `anyOf: ["true"]` requirement directly.
- `<tag>-<version>` (e.g. `maya-2025`) is emitted once per distinct version
  string captured by the detector's `version.from` regex, also with value
  `"true"`.
- A `path_glob` check can match several installs at once (e.g.
  `/opt/hfs20.0/…` and `/opt/hfs20.5/…`), so one detector can emit `houdini`,
  `houdini-20.0` and `houdini-20.5` together.

### Precedence: built-in → custom → manual

- Built-in detectors run before custom detectors, and a detector never
  overwrites a tag already present. When a custom detector emits a tag an
  enabled built-in also emits, the value is `"true"` either way; only the
  source credited in the `sqi-worker capabilities` output differs.
- To replace a built-in's checks for a tag (e.g. because it misfires on a
  nonstandard host layout), also add the tag to `capabilities.disable` so only
  your custom detector's checks run.
- Manual `worker.capability_tags` are merged last via `MergeManualTags` and
  overwrite a same-named key from any detector. Use them to suppress a
  misdetection (`maya=` clears the value) or to set a tag the checks miss
  (`maya=true` on a nonstandard install path).

### The `sqi-worker capabilities` command

Prints every tag the worker would advertise, auto-detected (hardware and
software) and manual, with its source, without connecting to a server:

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

The `VALUE` column is the tag's advertised value: `true` for a detector tag,
the detected string for `os` and `os_version`, and whatever a manual entry set
(a bare manual entry such as `gpu` has an empty value; the example above was
set as `gpu=true`). The `SOURCE` column is one of `auto` (the hardware probe's
`os` and, when detected, `os_version`), `builtin:<tag>`, `custom`, or `manual`.
Run this first when a worker is not picking up jobs you expect it to.
`sqi-worker start --dry-run` includes the same tags in its configuration and
capabilities summary.

---

## Writing custom detectors

Custom detectors are configured under `capabilities.detect` in the worker
config file (see
[`docs/worker-configuration.md`](worker-configuration.md#capabilities--software-auto-detection)
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
| `checks[].registry` | | A Windows registry key path under `HKLM` or `HKCU`, checked for existence only (no value is read). Never matches on Linux or macOS; pairing it with `os: windows` makes that explicit. |
| `checks[].os` | no | Restricts the check to one platform: `linux`, `darwin`, or `windows`. Omit to run the check on all platforms. |
| `version.from` | no | A regex applied to **each matched signal** of every check. The version is the named group `v` (`(?P<v>...)`) if present, otherwise the first capturing group. A signal that doesn't match contributes only the bare tag, no version tag. |

Exactly one of `exe` / `path_glob` / `env` / `registry` must be set per check.
A check with none or more than one is rejected at load time, as are a missing
`tag`, an empty `checks` list, an invalid `os` value, and a `version.from` or
`env.matches` regex that fails to compile. Built-ins go through the same
`Validate()` call, and tests assert that each one parses and validates.
Custom detectors are validated when the worker starts, and a validation
failure stops the worker with an error naming the detector.

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

sqi does not enforce these; they are conventions that keep affinity rules in
job submissions readable and consistent.

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

**Auto-detected string tags** (`os` and `os_version`) are stored in `Tags`
and can be overridden by a manual tag with exactly the same key:

```yaml
worker:
  capability_tags:
    - os=freebsd  # replaces Tags["os"] with "freebsd"
    - os_version  # presence-only: clears the auto-detected value to ""
```

To override `os_version` with a specific value, use `os_version=<value>`; to
just clear it, add the bare `os_version` entry (which sets
`Tags["os_version"] = ""`).

**Hardware values** (`RAMMb`, `CPUCount` and the GPU fields) are reported as
typed fields in the registration message, not as entries in the `Tags` map,
and cannot be overridden or removed through `capability_tags`. To route work
around a wrong value, add a manual tag such as `no-gpu` and write affinity
rules against that tag instead of the detected field.

---

## Using capability tags in job submissions

Affinity rules in OpenJD job definitions live under a step's
`hostRequirements.attributes`, each entry a `name` + an `anyOf` (or `allOf`)
list of acceptable string values. A custom worker tag is addressed as
`attr.worker.tag.<key>`, matched case-insensitively against
`Capabilities.Tags[<key>]` (see [`internal/scheduler/matcher.go`](https://github.com/uberware/sqi/blob/main/internal/scheduler/matcher.go)).
Any attribute name outside the well-known set (`attr.worker.os.family`,
`attr.worker.os.version`, `attr.worker.cpu.arch`,
`attr.worker.computelocation`) and the `attr.worker.tag.<key>` namespace
always resolves to `""` and can never match.

**`attr.worker.os.family` and `attr.worker.cpu.arch` take OpenJD's tokens, not
Go's.** The validator accepts only `linux`, `windows`, `macos` for the first
and `x86_64`, `arm64` for the second, and rejects anything else. Two of those
differ from what the worker reports about itself:

| Requirement value | Worker reports | Shown in the API |
| --- | --- | --- |
| `macos` | `darwin` | `darwin` |
| `x86_64` | `amd64` | `amd64` |

The scheduler translates when it matches, so a Mac satisfies `macos` and an
Intel host satisfies `x86_64`. A template that writes `darwin` or `amd64` is
rejected. The web UI and `sqi-worker capabilities` also show the OS as
`darwin`, and neither shows the architecture. To gate on the Go spelling of
the OS, use the `attr.worker.tag.os` tag instead.

A worker that reports no architecture (one built before architecture
reporting was added) matches no `attr.worker.cpu.arch` requirement, so
architecture-gated work is never sent to a host of unknown architecture.
Upgrading and restarting the worker makes it report one.

To gate a step on a worker tagged `maya=true`, whether auto-detected from a
standard Maya install (see [Capability
auto-detection](#capability-auto-detection-built-in-dcc-detectors) above) or
set manually with `capability_tags: [maya=true]` for a nonstandard install
(see [Setting manual tags](#setting-manual-tags) above):

```yaml
steps:
  - name: Render
    hostRequirements:
      attributes:
        - name: attr.worker.tag.maya
          anyOf: ["true"]
```

The seventeen DCC reference presets under `presets/sqi/` (see
[`docs/dcc-submitters.md`](dcc-submitters.md#reference-presets)) and the eight
[standalone renderer presets](preset-library.md#standalone-renderer-reference-presets)
use this syntax, as do the `Transcoding` presets, which gate on
`attr.worker.tag.ffmpeg` (see [`docs/preset-library.md`](preset-library.md#transcoding-reference-presets)).

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
