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

Twenty-five built-ins ship embedded in the worker binary, one YAML file per
application under
[`internal/worker/capabilities/builtins/`](https://github.com/uberware/sqi/tree/main/internal/worker/capabilities/builtins):

| Tag | Checks | Version source |
|---|---|---|
| `maya` | Linux `path_glob /usr/autodesk/maya*/bin/maya`; macOS `path_glob /Applications/Autodesk/maya*/Maya.app`; Windows `path_glob C:\Program Files\Autodesk\Maya*\bin\maya.exe` and `registry HKLM\SOFTWARE\Autodesk\Maya` | `(?i)maya(?P<v>[0-9]+)` against the matched path (case-insensitive: the Windows folder is `Maya2026`) |
| `houdini` | `exe hython` (any OS, if on `PATH`); Linux `path_glob /opt/hfs*/bin/houdini`; macOS `path_glob /Applications/Houdini/Houdini*/Frameworks/Houdini.framework`; Windows `path_glob C:\Program Files\Side Effects Software\Houdini *\bin\houdini.exe` and `registry HKLM\SOFTWARE\Side Effects Software\Houdini` | `(?:hfs\|Houdini ?)(?P<v>[0-9]+\.[0-9]+)` against the matched path |
| `nuke` | `exe nuke` (any OS, if on `PATH`); Linux `path_glob /usr/local/Nuke*/Nuke*`; macOS `path_glob /Applications/Nuke*/Nuke*.app`; Windows `path_glob C:\Program Files\Nuke*\Nuke[0-9]*.exe` and `registry HKLM\SOFTWARE\The Foundry\Nuke` (older installers; the 17.x MSI writes `HKLM\SOFTWARE\Foundry\Nuke 17.1v2` instead, so it is found by the glob) | `Nuke([0-9]+\.[0-9]+)` against the matched path (none when only `exe nuke` matched) |
| `blender` | `exe blender` (any OS, if on `PATH`); macOS `path_glob /Applications/Blender.app`; Windows `path_glob C:\Program Files\Blender Foundation\Blender *\blender.exe` and `registry HKLM\SOFTWARE\BlenderFoundation\Blender` (older installers; the MSI and winget write neither the key nor a `PATH` entry, so the glob is what finds them) | none (presence only, no `_<version>` tags) |
| `cinema4d` | Linux `path_glob /opt/maxon/cinema4dr*/bin/Commandline`; macOS `path_glob /Applications/Maxon Cinema 4D */Commandline.app`; Windows `path_glob C:\Program Files\Maxon Cinema 4D *\Commandline.exe` | `Maxon Cinema 4D (?P<v>[0-9]{4})` against the matched path (a Linux match carries no year, so no version) |
| `threedsmax` | `exe 3dsmaxcmd` (any OS, if on `PATH`); Windows `registry HKLM\SOFTWARE\Autodesk\3dsMax`; Windows `path_glob C:\Program Files\Autodesk\3ds Max *\3dsmaxcmd.exe` | `3ds Max (?P<v>[0-9]{4})` against the matched path |
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
| `threedelight` | `exe renderdl` (any OS); `env DELIGHT`; macOS `path_glob /Applications/3Delight/bin/renderdl`; Windows `path_glob C:\Program Files\3Delight\bin\renderdl.exe` | none |
| `oiiotool` | `exe oiiotool` (any OS, if on `PATH`) | none (presence only) |
| `katana` | `exe katana` (any OS, if on `PATH`); Linux `path_glob /opt/Foundry/Katana*/katana` and `/opt/foundry/katana*/katana`; Windows `path_glob C:\Program Files\Katana*\bin\katanaBin.exe`. All install paths unverified; Katana does not run on macOS. Also supplies a default `katana` command (Windows: `katanaBin.exe`) | `[Kk]atana([0-9]+\.[0-9]+v[0-9]+)` against the matched path (none when only `exe katana` matched) |
| `silhouette` | `exe sfxcmd` (any OS, if on `PATH`); Windows `path_glob C:\Program Files\BorisFX\Silhouette*\sfxcmd.exe` and `C:\Program Files\SilhouetteFX\Silhouette*\sfxcmd.exe`; macOS `path_glob /Applications/BorisFX/Silhouette*/Silhouette.app/Contents/MacOS/sfxcmd` and the `SilhouetteFX` equivalent; Linux `path_glob /opt/BorisFX/Silhouette*/sfxcmd` and `/opt/SilhouetteFX/Silhouette*/sfxcmd`. The `SilhouetteFX` Windows and macOS folders are Boris FX's documented layout, the `BorisFX` roots and Linux paths are unverified. Also supplies a default `sfxcmd` command, which carries `-nogui` on Linux (see [Detected commands](#detected-commands)) | `Silhouette ?([0-9]{4}(?:\.[0-9]+)?)` against the matched path (none when only `exe sfxcmd` matched) |
| `fusion` | `exe FusionRenderNode` (any OS, if on `PATH`); Windows `path_glob C:\Program Files\Blackmagic Design\Fusion Render Node *\FusionRenderNode.exe`; macOS `path_glob /Applications/Blackmagic Fusion * Render Node/Fusion Render Node.app/Contents/MacOS/Fusion Render Node`; Linux `path_glob /opt/BlackmagicDesign/FusionRenderNode*/FusionRenderNode`. All install paths inferred, none observed. Also supplies a default `FusionRenderNode` command (macOS: the executable is `Fusion Render Node`) | `(?:Fusion Render Node \|FusionRenderNode\|Fusion )(?P<v>[0-9]+(?:\.[0-9]+)?)` against the matched path (none when only `exe FusionRenderNode` matched) |
| `vred` | `exe VREDCore` (any OS, if on `PATH`); Windows `path_glob C:\Program Files\Autodesk\VREDCore-*\bin\WIN64\VREDCore.exe` (Autodesk's documented folder) and `C:\Program Files\Autodesk\VREDPro-*\bin\WIN64\VREDPro.exe`; Linux `path_glob /opt/Autodesk/VREDCore-*/bin/VREDCore` and `/opt/Autodesk/VREDCluster-*/bin/VREDCore` (the installer takes a target folder; both roots are unverified). VRED does not run on macOS. Also supplies a default `VREDCore` command from a VRED Core install, never from VRED Pro | `VRED(?:Core\|Pro\|Cluster)-([0-9]+\.[0-9]+)` against the matched path, VRED's internal version (17.0 is VRED 2025, 18.0 is VRED 2026; none when only `exe VREDCore` matched) |
| `keyshot` | `exe keyshot_headless` (any OS, if on `PATH`); Windows `path_glob C:\Program Files\KeyShot*\bin\keyshot_headless.exe`; macOS `path_glob /Applications/KeyShot*.app/Contents/MacOS/keyshot*`. All install paths inferred, none observed; KeyShot has no Linux desktop build, and its manual documents `keyshot -headless` on macOS and `keyshot_headless` on Windows. Also supplies a default `keyshot_headless` command: `keyshot_headless.exe` on Windows, and on macOS the bundle's `keyshot` program, which carries `-headless` (see [Detected commands](#detected-commands)) | `KeyShot ?([0-9]+(?:\.[0-9]+)?)` against the matched path: `12` for a `KeyShot12` folder, a year for `KeyShot 2024` (none when only `exe keyshot_headless` matched) |
| `modo` | `exe modo_cl` (any OS, if on `PATH`); Windows `path_glob C:\Program Files\Modo*\modo_cl.exe` and `C:\Program Files\Foundry\Modo\*\modo_cl.exe` (the 17.1 and 13.2 documented layouts); macOS `path_glob /Applications/Modo*.app/Contents/MacOS/modo_cl`. All install paths follow Foundry's documentation and none was observed; Linux has no fixed path, so put `modo_cl` on `PATH`. Modo is discontinued. Also supplies a default `modo_cl` command on Windows and macOS | `Modo[ \\/]*([0-9]+\.[0-9]+v[0-9]+)` against the matched path: Modo's own `17.1v1` form, from `Modo17.1v1`, `Modo 17.1v1` or `Foundry\Modo\13.2v1` (none when only `exe modo_cl` matched) |

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
`worker.capability_tags`. The `threedsmax` and `aftereffects` detectors do probe
`PATH` for `3dsmaxcmd` and `aerender`, names specific to the product. For all
three, a match on an install location (or, for `threedsmax`, the registry) tags the
worker even when the command is not on `PATH`, but the presets invoke the bare
command, so it must be on the worker's `PATH` for the job to run. A `threedsmax`
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
(`maya`, `houdini`, `nuke`, `blender`, `threedsmax`) yields the bare tag without a
`_<version>` variant unless a path-based check also matches on that host.

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
- `<tag>_<version>` (e.g. `maya_2025`) is emitted once per distinct version
  string captured by the detector's `version.from` regex, also with value
  `"true"`. Every character of the version that is not a letter or digit becomes
  an underscore, so `17.1` gives `nuke_17_1` and `10.10` gives `mistika_10_10`.
  A tag key becomes the last part of `attr.worker.tag.<key>` in a job's
  `hostRequirements`, and an OpenJD identifier allows only letters, digits and
  underscores, so a hyphen or a dot would make the tag impossible to require from
  a conformant template.
- A `path_glob` check can match several installs at once (e.g.
  `/opt/hfs20.0/…` and `/opt/hfs20.5/…`), so one detector can emit `houdini`,
  `houdini_20_0` and `houdini_20_5` together.

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
| `command.name` | no | The bare command name the presets run (for example `nuke`). Makes the detector supply a default for that command; see [Detected commands](#detected-commands). |
| `command.globs[].path_glob` | with `command` | A `filepath.Glob` pattern matching the **executable itself**, not the install directory (on macOS that is inside the `.app` bundle). `version.from` is applied to each match to choose the newest install. |
| `command.globs[].os` | no | Restricts the glob to `linux`, `darwin` or `windows`. |
| `command.args.<os>` | no | Arguments the application's own invocation requires on that OS (`linux`, `darwin` or `windows`), for example a flag that stops it opening a window on a headless worker. Literal strings, not format strings; an empty string is rejected. Never a license flag. See [Detected commands](#detected-commands). |

Exactly one of `exe` / `path_glob` / `env` / `registry` must be set per check.
A check with none or more than one is rejected at load time, as are a missing
`tag`, an empty `checks` list, an invalid `os` value, and a `version.from` or
`env.matches` regex that fails to compile. Built-ins go through the same
`Validate()` call, and tests assert that each one parses and validates.
Custom detectors are validated when the worker starts, and a validation
failure stops the worker with an error naming the detector.

### Detected commands

A tag only says software is installed. The presets run a **bare command**, and an
install often has none of that name: Nuke is `Nuke17.1.exe` on Windows and
`Nuke17.1` elsewhere, in a directory named for the release (`Nuke17.1v2`), and
its installer adds nothing to `PATH`. The worker starts a job's command directly,
with no shell, so a shell alias does not count either. Without help, a worker
tagged `nuke` would accept Nuke jobs and fail each with "executable file not
found".

A detector with a `command` stanza closes that gap. For each command name, the
worker decides where it resolves, in this order:

1. **The operator's `worker.commands` entry**, if there is one
   ([`worker.commands`](worker-configuration.md#workercommands)).
2. **A command of that name already on `PATH`.** The host or studio set it up, so
   the worker leaves it alone and never shadows it.
3. **The newest install the detector found**, as a default. "Newest" is by
   numeric version (16.0 beats 9.0), and two installs of one version, such as
   `Nuke17.1v1` and `Nuke17.1v2`, are ordered by path so the later release wins.

A detected default carries **no arguments** unless the detector declares
`command.args` for the platform: arguments the application's own invocation
requires, such as a flag that keeps it from opening a window. Those are applied to
the detected default **and** to a command found on `PATH` (the worker keeps the
`PATH` executable and adds the arguments, and the table reports source `PATH`
with them), but never to a `worker.commands` entry, which the operator owns
whole. If a detector has arguments for the platform but finds no install, they
still apply to a command on `PATH`; with neither, nothing is registered. Flags
that depend on a license, such as Nuke's `--nc` for a non-commercial license,
belong to the operator: put them in `worker.commands`. Only the `silhouette` and `keyshot` built-ins declare
arguments: `-nogui` for `sfxcmd` on Linux, where it otherwise needs an X display, and `-headless` for
`keyshot_headless` on macOS, where the detected command is the app's `keyshot` program (the form KeyShot
documents). So a command on `PATH` is left entirely alone except `sfxcmd` on a Linux worker, which
keeps `-nogui`, and `keyshot_headless` on a macOS worker, which keeps `-headless`.

The `nuke`, `blender`, `houdini` (`hython`), `katana`, `fusion`, `silhouette`
(`sfxcmd`), `vred` (`VREDCore`), `keyshot` (`keyshot_headless`) and `modo` (`modo_cl`) built-ins define a command; the others rely on the command being on `PATH`. Blender's has Windows and macOS globs
only: a Linux install is wherever it was unpacked, or a distro package already on
`PATH`. Houdini's has Windows and Linux globs only; on macOS put `hython` on
`PATH` or map it in `worker.commands`. For
Blender the version is read only to pick the newest install, so its tag stays
presence only. Turning a detector off with `capabilities.disable` turns off its
command default too. A custom detector can add one:

```yaml
capabilities:
  detect:
    - tag: mytool
      checks:
        - path_glob: "/opt/mytool*/bin/mytool"
      version:
        from: "mytool(?P<v>[0-9.]+)"
      command:
        name: mytool
        globs:
          - path_glob: "/opt/mytool*/bin/mytool"
            os: linux
```

`sqi-worker capabilities` and `sqi-worker start --dry-run` print a **COMMAND**
table showing what each command name resolves to, with its arguments and where
the decision came from (`config`, `PATH` or `detected`); the worker also logs each
one at startup.

**A `worker.commands` entry also advertises the tag.** Mapping `nuke` says where
Nuke is, so the worker advertises `nuke` (and `nuke_<version>`, when the path
carries one) even if the install is somewhere the detector's globs do not search.
It does so only when the entry's target exists on that host, so a mistyped path
advertises nothing, and never over a tag already present: a
[`worker.capability_tags`](worker-configuration.md#workercapability_tags) entry
such as `nuke=false` still turns the tag off. `sqi-worker capabilities` shows such
a tag with the source `worker.commands`.

The version in `nuke_<version>` is read from the text of the mapped path, as the
detector's version regex reads it from an install path. The worker never runs the
executable to ask it. A path that names a different release than the one it
launches (a `Nuke16.0v1` folder that is really a link to 17.1) therefore
advertises the wrong version. If your paths can mislead, add `nuke=false` and the
tags you want to `capability_tags` yourself.

The `nuke` default's globs follow the layouts in Foundry's documentation
(Linux `/usr/local/Nuke17.1v2/Nuke17.1`, macOS
`/Applications/Nuke17.1v2/Nuke17.1v2.app/Contents/MacOS/Nuke17.1`). The Windows
and Linux paths were checked against real 17.1v2 installs (Linux on Ubuntu 26.04
under WSL2, detecting `nuke_17_1` and defaulting the command to the installed
executable); the macOS one was not. Blender's Windows glob was checked against a
real Blender 5.2 installed by winget, and a job rendered through the default
command with nothing on `PATH`; its macOS path follows Blender's documented
bundle layout and was not checked. Houdini's Windows `hython` glob was checked against a real Houdini 22.0.466
(Apprentice), and a job ran through the default command with nothing on `PATH`;
its Linux glob (`/opt/hfs*/bin/hython`) follows the `$HFS/bin` layout and was not
checked. The Windows glob added for Maya is the installer's default folder and
has **not** been checked on a machine with Maya installed.

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
    - maya_2025
    - arnold_7
    - gpu
    - highram
    - maya=true   # key=value: sets Tags["maya"] = "true"
```

**Via environment variable (comma-separated):**

```sh
SQI_WORKER_CAPABILITY_TAGS=maya_2025,arnold_7,gpu,maya=true sqi-worker start
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
| `maya_<version>` | Maya is installed at the specified version, e.g. `maya_2025` |
| `houdini_<version>` | Houdini is installed, e.g. `houdini_20_5` |
| `blender_<version>` | Blender is installed, e.g. `blender_4_2` |
| `nuke_<version>` | Nuke is installed, e.g. `nuke_15_0` |
| `arnold_<version>` | Arnold renderer, e.g. `arnold_7` |
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
  value, e.g. `maya_2025` → `Tags["maya_2025"] = ""`.
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

A tag you want a template to require should be spelled as an OpenJD identifier:
letters, digits and underscores, not starting with a digit. sqi's validator is
more lenient and accepts other spellings, but a conformant implementation rejects
a template that names one. A manual tag that nothing requires, such as a label for
your own filtering, can be spelled however you like.

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

The twenty-seven DCC reference presets under `presets/sqi/` (see
[`docs/dcc-submitters.md`](dcc-submitters.md#reference-presets)) and the eight
[standalone renderer presets](preset-library.md#standalone-renderer-reference-presets)
use this syntax, as do the `Transcoding` presets, which gate on
`attr.worker.tag.ffmpeg` (see [`docs/preset-library.md`](preset-library.md#transcoding-reference-presets)).

The server's scheduler matches the required attribute against registered
workers' `Tags` map and routes each task only to workers that satisfy every
`hostRequirements` attribute. Workers missing the tag entirely, or whose
`Tags[<key>]` doesn't appear in `anyOf`, are not eligible for that task.

### Routing by license class

Nuke and Houdini both come in tiers that cannot share scene files. Nuke has a
commercial license, Indie and Non-commercial; Houdini has a commercial license,
Indie and Apprentice. Scripts saved in a restricted tier (Nuke's `.nknc`,
Houdini's `.hipnc`) are not opened by a commercial install, and a restricted
install may refuse the others: Non-commercial Nuke refuses a plain `.nk` from the
command line (see [Nuke Non-commercial](dcc-submitters.md#how-the-worker-finds-nuke)).
A farm that mixes tiers needs each job to land on a worker of the right one.

sqi does not do this for you. The reference presets do not require a license
class, no worker derives one, and the submitters do not pre-fill one. A mixed
farm can build it today from a tag convention:

1. **Tag every worker that has the product**, in `worker.capability_tags`, with
   `<product>_license` set to `commercial`, `indie` or `noncommercial`:

   ```yaml
   worker:
     capability_tags:
       - nuke_license=noncommercial
   ```

2. **Require it in a copy of the preset** (see
   [Customizing for your pipeline](dcc-submitters.md#customizing-for-your-pipeline)),
   next to the product's own tag:

   ```yaml
   hostRequirements:
     attributes:
       - name: attr.worker.tag.nuke
         anyOf: ["true"]
       - name: attr.worker.tag.nuke_license
         anyOf: ["noncommercial"]
   ```

Points that decide whether it works:

- **Every worker must carry the tag.** A requirement matches only a tag the worker
  advertises, and `anyOf`/`allOf` cannot express "any" or "not". A worker with no
  `nuke_license` tag never receives a job that requires one. To accept every class,
  leave the requirement out.
- **Spell the key with an underscore.** OpenJD identifiers allow only letters,
  digits and underscores, so `nuke_license` is valid in any implementation.
  sqi's validator also accepts `attr.worker.tag.nuke-license`, but the
  specification does not, and a template written that way is not portable. The
  automatic tags follow the same rule (`17.1` gives `nuke_17_1`), and so do the
  names of the built-in ones: the 3ds Max and 3Delight tags are `threedsmax` and
  `threedelight` because an identifier cannot start with a digit.
- **The worker does not check the claim.** The tag is whatever the operator wrote;
  sqi neither reads the license nor stops a job whose scene file belongs to another
  class.

This was checked on a local farm with a worker tagged `nuke_license=noncommercial`:
a job requiring `noncommercial` ran, and one requiring `commercial` stayed `ready`
and was never sent. The license-class routing was not checked with Houdini, and the Houdini rules
above come from SideFX's documentation and forum, not from a test: SideFX's pages
give two different resolution limits for Apprentice. What was checked, on Houdini
22.0.466 with an Apprentice license, is that `hython` starts headless, loads a
`.hipnc` and renders a Geometry ROP through `houdini-rop-render`. Mantra and Karma
renders, and Apprentice's resolution limit, were not tried.

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
