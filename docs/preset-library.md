<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
# Preset Library

The **preset library** is a catalog of ready-to-use product definitions hosted at a
configurable URL. Each entry is a product definition file in the same format as a
hand-authored custom product (`name`, `title`, `description`, `category`, `version`,
and an inline OpenJD `template:`), which can be fetched and installed on the farm
without writing any YAML.

> **Installed presets run commands on your farm. Only configure a preset library
> you trust.**

---

## How it works

The preset library operates at two levels:

1. **Index**: a single JSON file listing every available preset with metadata and a
   SHA-256 checksum of its definition file. `sqi-server` fetches the index when you
   open the Preset Library page and caches it in memory for five minutes. The
   **Refresh** button forces a fresh fetch.

2. **Definition files**: individual product definition YAML files referenced by the
   index. A definition is always fetched fresh at install time; the server verifies
   its SHA-256 against the index before storing it. A mismatch is rejected with 422.

### Index format

```json
{
  "presets": [
    {
      "name":        "blender-render",
      "title":       "Blender Render",
      "description": "Render a Blender scene with a given frame range.",
      "category":    "Rendering",
      "version":     "1.2.0",
      "definition":  "blender/blender-render.yaml",
      "sha256":      "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
    }
  ]
}
```

`definition` is a path **relative to the index URL**. If the index is at
`https://example.com/presets/index.json`, the definition above is fetched from
`https://example.com/presets/blender/blender-render.yaml`.

`sha256` is the hex-encoded SHA-256 hash of the definition file's raw content. It
serves two purposes: **integrity** (the downloaded bytes match what the index
promised) and **update detection** (if the hash in the index changes, the installed
product is shown as having an update available). It does not authenticate the
author: the trust boundary is the configured index URL.

The index carries `description`, which the preset list page searches, but not
`readme`. A preset's readme arrives with its definition when its detail page is
opened.

---

## Configuration

See [`docs/configuration.md`](configuration.md#preset_library--remote-preset-catalog)
for the full reference. The short version:

| Key | Default | Env var |
|---|---|---|
| `preset_library.url` | `https://uberware.github.io/sqi-presets/index.json` | `SQI_PRESET_LIBRARY_URL` |

Set `preset_library.url` to an empty string `""` to **disable** the feature. When
disabled, the Browse page shows a "not configured" empty state and all preset REST
endpoints return 503.

The default points to the official community preset library hosted on GitHub Pages.
To use a private or self-hosted library, provide any accessible HTTP or HTTPS URL
that serves an index in the format above.

---

## Browse → preview → install

### Browse

Open **Admin → Preset Library** (`/presets`) to see every preset in the index. Each
entry shows the name, title, category, version, and current install status:

| Status | Meaning |
|---|---|
| Not installed | The preset is available in the library but not yet installed. |
| Installed | The installed product's SHA-256 matches the index. |
| Update available | The index SHA-256 has changed since the preset was installed. |

Click **Refresh** to re-fetch the index from the configured URL.

### Preview

Click a preset name to open its detail page (`/presets/:name`). The page shows the
full product metadata and the raw OpenJD template (read-only). An **Install**,
**Update**, or **Reinstall** button appears based on the current status.

### Install

Clicking **Install** (or **Update** / **Reinstall**) triggers the following on the
server:

1. Fetch the definition file from the URL derived from the index entry.
2. Verify the downloaded content's SHA-256 against the index. Reject with 422 if
   they do not match.
3. Parse and validate the definition as a product (same validation pipeline as
   `POST /api/v1/products`).
4. Store the product with `source: installed`.

A first install returns HTTP 201; an update or reinstall returns HTTP 200.

---

## REST endpoints

All endpoints return 503 when `preset_library.url` is empty. A 503 can also
mean that validating the definition exceeded `openjd.expr_submission_deadline`
on this server; that is not a rejection of the preset, so retry. An unreachable
or unparseable **index** is 502.

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/v1/presets` | List all presets with per-preset status. `?refresh=true` forces a re-fetch of the index. 502 if the index cannot be fetched. |
| `GET` | `/api/v1/presets/{name}` | Preview a preset — metadata, template, and status. 404 if not in the index. 502 if the index cannot be fetched. 422 if the definition cannot be fetched, verified or parsed. |
| `POST` | `/api/v1/presets/{name}/install` | Install or update the named preset. 201 on first install, 200 on update or reinstall. 404 if not in the index. 409 if a built-in or custom product already uses that name. 422 on SHA-256 mismatch or validation failure. 502 if the index cannot be fetched. |

To uninstall, use `DELETE /api/v1/products/{name}`.

---

## Installed product lifecycle

Installed products follow different rules from custom products:

- **Read-only.** `PUT /api/v1/products/{name}` returns 403. The template is
  authoritative from the library and is updated only via the preset install flow.
- **Uninstallable.** `DELETE /api/v1/products/{name}` removes the product from
  the catalog. Jobs already submitted from it are unaffected, because each job
  keeps a copy of its template from submission time.
- **Update available.** When the library's SHA-256 for a preset changes, the Browse
  and Preview pages show an **Update available** badge. Updating re-fetches the
  definition, verifies the new hash, and overwrites the stored product. Installed
  products cannot be edited, so an update never overwrites local edits.
- **Duplicate to custom.** Every product, installed ones included, has a
  **Duplicate to custom** action. It creates a locally editable copy with
  `source: custom` that is no longer tied to the library and is not affected by
  future library updates.

---

## Hosting a private preset library

Any HTTP server that can serve static files is sufficient:

1. Publish an `index.json` in the format shown above.
2. Publish the individual definition YAML files at paths relative to the index URL.

Set `preset_library.url` to the full URL of your `index.json`. When you update a
definition file, also update its `sha256` entry in the index so that already-installed
copies show the **Update available** badge.

---

## DCC reference presets

The official library ships fourteen `Rendering`-category presets as targets for
the [`sqi-submitter`](dcc-submitters.md) in-application submitters:
`maya-layer-render`, `maya-scene-render`, `houdini-rop-render`,
`nuke-write-render`, `nuke-script-render`, `blender-batch-render`,
`blender-view-layer-render`, `cinema4d-render`, `cinema4d-take-render`,
`3dsmax-render` and the four After Effects presets. Fifteen more have no
in-application submitter and are submitted from the web UI or the API:
`mistika-boutique-render`, `mistika-vr-render`, `mistika-workflows-render`,
`katana-render`, `silhouette-render`, `silhouette-render-all`, `fusion-render`,
`vred-render`, `vred-viewpoint-render`, `keyshot-render`,
`keyshot-studio-render`, `modo-render`, `modo-pass-group-render`,
`unreal-mrq-job-render` and `unreal-mrq-queue-render`.

Each names its parameters by the [parameter convention
contract](dcc-submitters.md#the-parameter-convention-contract), so submitter
pre-fill works without configuration. All but the two After Effects queue
presets and the two Unreal presets declare `SceneFile` and `Frames`; the queue
presets render whatever the project has queued, so they take only `SceneFile`,
and the Unreal presets render the frame range their Movie Render Queue config
sets, as one task. An output parameter is
declared only where the command takes one: `OutputDir` on the two Maya presets,
`OutputPath` on Blender, Cinema 4D and 3ds Max, `OutputFile` on the two
VRED and the two KeyShot presets, and `OutputDirectory` on the two Unreal presets. Per-host extras include `Renderer`, `RenderLayer`, `RopPath`,
`WriteNode`, `Scene`, `ViewLayer`, `Take`, `RenderNode`, `Session`,
`OutputNode`, `View`, `Studio`, `PassGroup`, `LevelSequence`, `Map`, `MovieRenderConfig`, `QueueAsset` and `RenderQueueItem`.

Each preset gates on a capability tag that `sqi-worker` auto-detects from a
standard install; a manual tag is needed only for a nonstandard install path
(see [capability
auto-detection](worker-capabilities.md#capability-auto-detection-built-in-dcc-detectors)).
See [`docs/dcc-submitters.md`](dcc-submitters.md) for the full reference,
including the chunking behavior and worker requirements per preset.
`TestBuiltinDetectors_CoverPresets` fails if a shipped preset requires a
software tag that no built-in detector emits.

### Katana

| Preset | Renders | Shape | Gate |
|---|---|---|---|
| `katana-render` | one named Render node (`RenderNode`) of a `.katana` project with `katana --batch` | `FramesPerTask` frames per task, default 1 | `katana` |

`katana-render` has no whole-scene variant, because a Render node is the unit of
work, as a ROP is for Houdini. It runs `katana --batch
--katana-file=<scene> --render-node=<node> -t <first>-<last>`, with each of
`--katana-file=` and `--render-node=` one argument holding the `=` and the value,
so a name with a space stays whole. Foundry documents `-t` with ranges and comma
lists and no step form, so a stepped `Frames` range splits into one task per
frame. Every host that renders needs a `katana_r` render licence.

It is the first preset to declare [`SQI_LOG_CHECKS`](openjd-extensions.md): a
task fails when a line of Katana's output matches `ERROR +\|`, the pattern
Deadline's Katana plugin treats as a failed render, because Katana's exit status
on failure is not documented. Foundry also documents a batch render that never
exits when an ImageRead node's file is missing (4.5v5 to 6.0v3); the preset sets
no timeout, and its readme recommends adding one to the step's `onRun` action.
The `katana` detector's Linux and Windows install paths are unverified; Katana
does not run on macOS. See the preset's readme for what has not been verified.

### Fusion

| Preset | Renders | Shape | Gate |
|---|---|---|---|
| `fusion-render` | a whole `.comp` with `FusionRenderNode` | `FramesPerTask` frames per task, default 10 | `fusion` |

`fusion-render` has no per-target variant: Fusion's command line cannot pick a
Saver, so every Saver in the comp writes where the comp says. It runs
`FusionRenderNode <comp> -render -start <first> -end <last> -quiet -verbose
-quit`, one argument per flag and value; a stepped `Frames` range splits into
one task per frame. The Render Node ships inside Fusion Studio and needs a
Studio licence (a dongle, or a Fusion Server on the subnet); the free edition
cannot render from the command line. Linux needs an X display (Xvfb), and a
Fusion GUI open on the machine holds the scripting port and stalls the node.

It declares [`SQI_LOG_CHECKS`](openjd-extensions.md): a task fails when a line of
standard output contains `The fusion renderer reported that the render failed`
or `Failed to load the comp`, Deadline's two strings. **It is unverified whether
they reach standard output with `-verbose`** rather than only a `-log` file
(Deadline drives the node through its own scripting connection), in which case
the check never fires and a failed render is caught only by an exit status that
is itself undocumented. A Fusion 8 report of a successful render exiting
`-1073741819` is recorded in the readme and not handled. The `fusion`
detector's install paths on all three platforms are inferred, not observed.

### Silhouette

| Preset | Renders | Shape | Gate |
|---|---|---|---|
| `silhouette-render` | one output node (`OutputNode`, a label) of one session (`Session`) of a Silhouette project | `FramesPerTask` frames per task, default 10 | `silhouette` |
| `silhouette-render-all` | every output node of the project's active session | `FramesPerTask` frames per task, default 10 | `silhouette` |

`silhouette-render` runs `sfxcmd -session <session> -node <output node> -range
<first>-<last> <project>` and always passes `-node`, because the node `sfxcmd`
picks without it changed between releases. `silhouette-render-all` runs `sfxcmd
-all -range <first>-<last> <project>` and declares neither `Session` nor
`OutputNode`, so a submitter dialog treats it as whole-scene; `-all` needs a
Silhouette whose `sfxcmd -help` lists it, and which release added it is unknown.
`SceneFile` is the `.sfx` bundle, which is a directory (`objectType: DIRECTORY`),
not the `project.sfx` file inside it. `-range` is in session numbering and
`-start`, which renumbers output files, is never passed; a stepped `Frames` range
splits into one task per frame.

**Neither preset declares [`SQI_LOG_CHECKS`](openjd-extensions.md), and a failed
render that exits 0 is reported as a success.** No line `sfxcmd` prints on
success or failure is documented, a Linux report has it exiting 0 with no frames
written, and its exit codes are undocumented; a pattern was not guessed. Check
the output folder, not only the job status. The `silhouette` detector supplies
`sfxcmd` and, on Linux only, `-nogui` (the first detector to declare arguments),
whether `sfxcmd` comes from the install or from `PATH`; the bundled `lib`
folder must be on the worker's `LD_LIBRARY_PATH`. The detector's install folders
(the documented `SilhouetteFX` layout and a `BorisFX` root) are unverified. A
floating licence needs the render portion `genarts_LICENSE`, in lower case.

### VRED

| Preset | Renders | Shape | Gate |
|---|---|---|---|
| `vred-render` | a `.vpb` scene's saved view with VRED Core | `FramesPerTask` frames per task, default 10, at most 1000 | `vred` |
| `vred-viewpoint-render` | one named viewpoint or camera (`View`) of a `.vpb` scene | `FramesPerTask` frames per task, default 10, at most 1000 | `vred` |

The first **host-script** presets: each task runs `VREDCore <scene>
-insecure_python -fast_start -hide_gui -postpython <code>`, where `<code>` is a
constant Python expression that only executes the preset's embedded `render.py`
(its path written with `EXPR`'s `repr_py`). Autodesk's command-line reference
documents `-postpython` as Python commands run after the scene loads, not a
file name. The script reads the scene, the task's frames, the optional
`OutputFile` and (for the viewpoint preset) `View` from an embedded
`params.json` that `EXPR` builds with `repr_json`, so no job value is ever
written into Python source; it calls VRED's Python API v1 render settings
(animation on, clip range off, first and last frame, frame step 1, the view,
the render filename) and `startRenderToFile`. `params.json` is evaluated on the
worker, against its `EXPR` limits, and costs more as the chunk grows, which is
why `FramesPerTask` stops at 1000 (pinned by
`TestPresetVRED_ParamsCostAtMaxChunk`). A blank `OutputFile` keeps the scene's
render filename.

Both declare [`SQI_LOG_CHECKS`](openjd-extensions.md) with the host-script
markers: a task fails unless the output contains `SQI_RENDER: OK`, and fails on
a line containing `SQI_RENDER: FAILED:` or `No valid license found`. VRED can
exit 0 without rendering (after a licence failure, for one), so its exit
status is not relied on. The script writes both markers straight to file
descriptor 1; **whether that reaches the worker from VRED, a windowed program
on Windows, is unverified**, and if it does not, every task fails rather than
passing silently. The checks give their verdict only when VRED exits, and
whether it exits after `terminateVred()` under `-hide_gui` is undocumented; the
presets set no timeout, so their readmes recommend adding one to the step's
`onRun` action. Autodesk marks `startRenderToFile` as VRED Professional only,
although AWS's VRED samples call it under VRED Core. `-insecure_python` turns
off the Python sandbox of VRED 2024 and later. Linux needs an X server (Mesa
under Xvfb on a machine without a GPU). A Windows worker with only VRED Pro
maps `VREDCore` to `VREDPro.exe` in `worker.commands`. VRED's versions are
internal: 17.0 is VRED 2025, 18.0 is VRED 2026. The `vred` detector's Windows
VRED Core folder is Autodesk's documented one; its VRED Pro folder and both
Linux roots are unverified.

### KeyShot

| Preset | Renders | Shape | Gate |
|---|---|---|---|
| `keyshot-render` | a `.bip` scene with KeyShot's headless mode | `FramesPerTask` frames per task, default 10, at most 1000; a chunk may be stepped or gapped | `keyshot` |
| `keyshot-studio-render` | one named studio (`Studio`) of a `.bip` scene | as `keyshot-render` | `keyshot` |

Host-script presets like VRED's, with one difference: KeyShot's headless scripting has no call that renders
a frame range, so each task runs `keyshot_headless -progress [-floating_feature <feature>] <scene> -script
<script>` and the embedded `render.py` loops over the task's frames, calling `lux.setAnimationFrame` and
`lux.renderImage` for each. The chunks are `NONCONTIGUOUS`, so a stepped or gapped range costs no extra
launches, and the presets declare no `SQI_CHUNK_BOUNDS`. `-floating_feature` appears only when **License
Feature** is set (`keyshot2`, `keyshot_enterprise` or `keyshot_network_rendering`). **Output File** holds
one run of `#` for the frame number, expanded by the script with zero padding; a name with no run or two runs,
or an extension other than `.jpg`, `.jpeg`, `.png`, `.exr`, `.tif`, `.tiff` or `.psd` (which picks the
`lux.RENDER_OUTPUT_*` format; TIFF and PSD are 8-bit), is refused at submission (pinned by
`TestPresetKeyShot_OutputFileRules`). **Width** and **Height** are INT parameters where 0, the default, leaves
the size to the scene: KeyShot's headless `renderImage` defaults both to the scene's own. The script reads
everything from an embedded `params.json` that `EXPR` builds with `repr_json`, so no job value is written into
Python source; `params.json` costs more as the chunk grows, which is why `FramesPerTask` stops at 1000 (pinned
by `TestPresetKeyShot_ParamsCostAtMaxChunk`).

Both declare [`SQI_LOG_CHECKS`](openjd-extensions.md) with the host-script markers: a task fails unless the
output contains `SQI_RENDER: OK`, and fails on a line containing `SQI_RENDER: FAILED:`. KeyShot documents no
exit codes, so its exit status is not relied on. The checks give their verdict only once KeyShot exits, and
whether it exits when the script ends is undocumented, so the readmes recommend a step timeout. The markers go
straight to file descriptor 1 (KeyShot documents `keyshot_headless` as supporting stdout on Windows; macOS is
unverified). **Headless scripting is a KeyShot Pro feature (9.3 and newer, "experimental" per KeyShot), a
trial watermarks its renders, and KeyShot has no Linux desktop product.** Frame numbering is unverified: the
reference does not say whether animation frames start at 0 or 1, so `Frames` is passed through unchanged
(KeyShot's own quick-start loop starts at 1). On macOS the detected `keyshot_headless` command is the app's
`keyshot` program with `-headless` put first, the form KeyShot documents there. No KeyShot has executed
either preset; the detector's install folders are inferred.

### Modo

| Preset | Renders | Shape | Gate |
|---|---|---|---|
| `modo-render` | a `.lxo` scene through its render outputs, in the current pass state | `FramesPerTask` frames per task, default 1, at most 1000 | `modo` |
| `modo-pass-group-render` | one named pass group (`PassGroup`) of a `.lxo` scene | as `modo-render` | `modo` |

**Read this first: Modo is discontinued, and the way these presets start it is untested.** `modo_cl` reads
its commands from standard input, and the worker leaves a task's standard input as the null device, which is at
end-of-file from the first read. The presets therefore hand Modo its work with the documented `-cmd:` startup switch,
and whether `modo_cl` runs that command to the end before it notices the end-of-file, or quits first, has not been
tried. If it quits first, every task fails, safely: no `SQI_RENDER: OK` line is ever printed. Try one single-frame
job before a longer one. Modo's last release was 17.1 (November 2024); downloads now need a Foundry support ticket,
and the end-of-life license is an RLM `uncounted hostid=ANY` key that Modo finds through `foundry_LICENSE`.

Host-script presets like VRED's. Each task runs `modo_cl -dboff:crashreport -cmd:@{<script>} {<params>}`: one
`-cmd:` command that runs the embedded `render.py` and hands it the path of `params.json` as its argument, each path
quoted in braces (Foundry documents quotes or braces for a name with spaces; a braced script path directly after `@` is
not shown anywhere, so it is unverified). The script opens the scene, sets the render item's `first`, `last` and `step`
from the task's chunk, and runs `render.animation {*}`, or for the pass-group preset `render.animation` with the group
as a keyword argument. Nothing from the job is ever written into Modo command text or Python source: the scene and the
pass group reach `lx.command` as keyword arguments, and the frames as integers, all read from an embedded
`params.json` that `EXPR` builds with `repr_json`. The chunks are `CONTIGUOUS`; `params.json` costs more as the chunk
grows, which is why `FramesPerTask` stops at 1000 (pinned by `TestPresetModo_ParamsCostAtMaxChunk`).

Both declare [`SQI_LOG_CHECKS`](openjd-extensions.md): a task fails unless the output contains `SQI_RENDER: OK`, and
fails on a line containing `SQI_RENDER: FAILED:`, `failed with` or `Unknown command:`. Modo documents no exit status,
so it is not relied on. The checks give their verdict only once Modo exits, and the script ends with `app.quit`,
but whether Modo then exits in headless mode is undocumented, so the readmes recommend a step timeout. Foundry says
`-cmd:` runs before licensing and networking start, and whether a scene can be opened and rendered that early is also
unknown. The detector looks for `modo_cl` under `C:\Program Files\Modo*` and `C:\Program Files\Foundry\Modo\*` on
Windows and in the `.app` bundle on macOS; on Linux there is no fixed path, so put `modo_cl` on the worker's `PATH`.
No Modo has executed either preset; the argument names of `scene.open` and `render.animation` used with keywords
(`filename`, `type`, `group`) are unverified, and a wrong one fails every task.

### Unreal Engine

| Preset | Renders | Shape | Gate |
|---|---|---|---|
| `unreal-mrq-job-render` | one Movie Render Queue job: a level sequence (`LevelSequence`) on a map (`Map`) with a saved config (`MovieRenderConfig`) | one task | `unreal`, Windows |
| `unreal-mrq-queue-render` | every job of a saved Movie Pipeline Queue asset (`QueueAsset`) | one task | `unreal`, Windows |

Host-script presets like VRED's, and the only P5b presets gated on the worker's OS: Windows is the one OS any vendor
Unreal render adaptor ships, so both require `attr.worker.os.family` = `windows` and their Tier 3 and outcome cases
run on Windows only. Each task runs `UnrealEditor-Cmd <project> -log -unattended -stdout -allowstdoutlogverbosity
-NoLoadingScreen -NoScreenMessages -RenderOffscreen -nohmd -windowed -execcmds=py <one line of Python>`, the command line
of AWS's Deadline Cloud adaptor for Unreal. The line of Python adds the task's session folder to Python's path and imports
the embedded `sqi_unreal_render.py`; it holds no comma (Unreal splits `-execcmds` at commas) and no double quote, and a
session folder whose path would need one (`worker.session_dir`) fails the task, saying so. The script builds the job, or
copies the saved queue's jobs into the editor's queue, applies `OutputDirectory` when it is not blank, starts the Movie
Render Queue's PIE executor and returns; the editor keeps running while it renders, as AWS's adaptor relies on, which is
why the presets use `-execcmds` rather than `-ExecutePythonScript` (reported to quit before an asynchronous render
completes). When the executor reports back, the script prints `SQI_RENDER: OK` if every job succeeded, or
`SQI_RENDER: FAILED:` with the reason, and calls `quit_editor()`; a fatal error ends the render the same way. No job
value is written into the command line or Python source: the asset paths and the output directory come from an embedded
`params.json` that `EXPR` builds with `repr_json`. With no frame parameter its cost is fixed, and every value it quotes
has a `maxLength` of 1024 (pinned by `TestPresetUnreal_ParamsCostIsFixed`).

Both declare [`SQI_LOG_CHECKS`](openjd-extensions.md): a task fails unless the output contains `SQI_RENDER: OK`, and
fails on a line containing `SQI_RENDER: FAILED:`, `LogPython: Error:` or `Render Executor: Error:`. The exit status is
not relied on. The checks give their verdict only once the editor exits, which it does only when the executor reports
back, so the readmes recommend a step timeout long enough for the first render on a worker, which compiles the
project's shaders. The project and its content must be at the same path on every worker, or path-mapped; maps and
queues must be saved; the project needs the Movie Render Queue and Python Editor Script plug-ins. The detector finds
only the Epic Games Launcher's default folder, `C:\Program Files\Epic Games\UE_5.*`; an install anywhere else needs an
`UnrealEditor-Cmd` entry in `worker.commands`. Epic's licence terms for render nodes are the studio's to check. No
Unreal has executed either preset.

### Cinema 4D, 3ds Max and After Effects

| Preset | Renders | Shape | Gate |
|---|---|---|---|
| `cinema4d-render` | a `.c4d` scene with Maxon's `Commandline`, using the scene's render settings | one frame per task by default | `cinema4d` |
| `cinema4d-take-render` | one named take of a `.c4d` scene (`Take`) | one frame per task by default | `cinema4d` |
| `3dsmax-render` | a `.max` scene with `3dsmaxcmd` | one frame per task by default | `threedsmax`, and `attr.worker.os.family` = `windows` |
| `after-effects-render-windows` | one render-queue item (`RenderQueueItem`) of an `.aep` project with `aerender` | up to `FramesPerTask` frames per task, default 10 | `aftereffects`, and `attr.worker.os.family` = `windows` |
| `after-effects-render-macos` | the same, on macOS | up to `FramesPerTask` frames per task, default 10 | `aftereffects`, and `attr.worker.os.family` = `macos` |
| `after-effects-queue-render-windows` | everything the saved project has queued | one task for the whole queue | `aftereffects`, and `attr.worker.os.family` = `windows` |
| `after-effects-queue-render-macos` | the same, on macOS | one task for the whole queue | `aftereffects`, and `attr.worker.os.family` = `macos` |

`3dsmax-render` is Windows-only because 3ds Max runs only on Windows. The two
Cinema 4D presets carry no OS gate. Each Cinema 4D render node must be licensed
before it can render. The `cinema4d` detector has no `exe` check, only
install-location globs, so a Cinema 4D installed elsewhere needs the tag set by
hand (see [built-in detectors](worker-capabilities.md#built-in-detectors)).

A detector can tag a worker from the install location alone, but the presets
run the bare commands `Commandline`, `3dsmaxcmd` and `aerender`, so each must
also be on the worker's `PATH` for the job to run.

**The After Effects wrapper.** `aerender` is reported to exit with status 0 when
a render fails, so all four After Effects presets run it through a small script
embedded in the preset: PowerShell on Windows, POSIX `sh` on macOS. The wrapper
passes `aerender`'s output to the task log unchanged and fails the task when
`aerender` exits non-zero or when any line of its output contains
`aerender ERROR`, matched case-insensitively. That marker comes from user
reports and has not been checked against a real failing render; a failure that
neither exits non-zero nor prints the marker is still reported as a success.
The Windows and macOS variants pass identical arguments to `aerender` and
differ only in the wrapper script, which the variant's `os.family` gate
selects.

Each After Effects render node needs `aerender` on its `PATH`, `powershell`
(Windows) or `sh` (macOS) on its `PATH`, and Adobe's render-only marker: an empty
file named `ae_render_only_node.txt` in `C:\Users\Public\Documents\Adobe` on
Windows or `/Users/Shared/Adobe` on macOS, which runs After Effects in its
non-royalty-bearing, render-only mode.

**A movie output must be one task.** After Effects cannot render one movie file
(a QuickTime or other container, as opposed to an image sequence) from several
machines. When the render-queue item writes a movie, set **Frames Per Task** to
at least the number of frames in the range, and enter the range with no step and
no gaps, such as `1-240`, because a `CONTIGUOUS` chunk always splits at a gap.
The preset cannot see the output format, so nothing enforces this. The queue
presets always run as one task.

**Houdini and Nuke chunking.** `houdini-rop-render` uses `NONCONTIGUOUS` chunks
of up to ten frames, so a task receives a compact frame range (`1-10`,
`1-19:2`, `1-3,7,8`) and a stepped range such as `1-60:2` is three tasks
rather than thirty. The embedded driver script renders each part of the range
with one `rop.render()` call and merges consecutive frames into one call, so
`1-19:2` is one call and `1-3,7,8` is two. `nuke-write-render` and
`nuke-script-render` use `CONTIGUOUS` chunks because Foundry documents no
comma-list form of `-F`, so a stepped range there is one task per frame.

## Transcoding reference presets

The official library also ships five `Transcoding`-category presets under
`presets/sqi/*.yaml`: plain ffmpeg jobs submitted directly rather than through
a DCC submitter. Each gates on the `attr.worker.tag.ffmpeg = "true"` capability
tag, which `sqi-worker` sets automatically on any worker with `ffmpeg` on
`PATH` (see [worker capability tags](worker-capabilities.md)). Only the
PowerShell-joined segmented variant adds an OS gate:

- `ffmpeg-transcode` converts one video file on one worker. It is base-spec
  OpenJD (declares no extensions), so the scheduler never withholds it from an
  `ffmpeg=true` worker because of that worker's EXPR limits.
- `ffmpeg-sequence-encode` turns a rendered image sequence into a movie on one
  worker, typically after a render job. It declares `EXPR`.
- Three segmented variants split a long source into fixed-length slices,
  transcode each slice as its own task, which may run on different workers,
  then join the slices into a single file. All three need Source Duration
  entered by hand, because nothing measures the source before submission:
  - `ffmpeg-segment-transcode-bash` joins with a bash script. It has no OS
    gate, so it runs on any `ffmpeg=true` worker with `bash` on `PATH`,
    including a Windows worker with Git Bash. The task runs `bash` with the
    script as an argument rather than executing the script directly, which
    Windows cannot do, and the script converts backslashes in a Windows output
    path to forward slashes. It removes its slice files once the join
    succeeds.
  - `ffmpeg-segment-transcode-powershell` joins with a PowerShell script and
    needs Windows workers. It removes its slice files once the join succeeds.
  - `ffmpeg-segment-transcode-expr` ("Portable") needs no shell, so it runs on
    any `ffmpeg=true` worker; the template itself generates the join's file
    list. That list is evaluated at submission and its cost grows with the
    slice count, so this variant suits jobs of up to 400 slices. Past that,
    use the Bash or PowerShell variant, whose cost does not grow with the slice
    count. It leaves its slice files beside the output for you to remove.

Install and use them exactly like the DCC reference presets above (Browse →
Install as a product).

## Standalone renderer reference presets

The official library also ships eight `Rendering`-category presets that render
an **exported scene description** with a standalone renderer. The application
that produced the scene is not loaded on the farm. These presets have no
in-application submitter; submit them from the web UI or the API. Each gates on
a capability tag that a built-in detector sets on any worker with the renderer
installed or on `PATH` (see [worker capability
tags](worker-capabilities.md#built-in-detectors)):

| Preset | Renders | Shape | Worker tag |
|---|---|---|---|
| `arnold-kick-render` | Arnold `.ass` files with `kick` | one file per frame | `arnold` |
| `arnold-kick-tile-render` | each `.ass` frame as a grid of `kick` render regions, then assembled with `oiiotool` | one file per frame, × tiles | `arnold` (Render), `oiiotool` (Assemble) |
| `vray-standalone-render` | one animated `.vrscene` with `vray` | frame range per task | `vray` |
| `husk-usd-render` | one USD stage with `husk`, through Karma or any Hydra delegate | frame range per task | `husk` |
| `redshift-standalone-render` | Redshift `.rs` files with `redshiftCmdLine` | one file per frame | `redshift` |
| `prman-render` | RenderMan `.rib` files with `prman` | one file per frame | `renderman` |
| `3delight-render` | 3Delight `.nsi` files with `renderdl` | one file per frame | `threedelight` |
| `mantra-render` | Mantra `.ifd` files with `mantra` (legacy; prefer husk) | one file per frame | `mantra` |

**One file per frame.** The presets that read one exported file per frame take
a **Scene File Pattern** that marks the frame number with a run of `#`
characters, such as `shot.####.ass`. Each task renders one frame. The run is
replaced with the frame number, zero-padded to the run's length: frame 7 is
`shot.0007.ass`. A frame number longer than the run is written in full. A
negative frame keeps its sign inside the padding: frame -2 is `shot.-002.ass`. A pattern with no run of `#`, or with more than
one, is rejected at submission. Only the file name is checked, so a `#` in a
directory of a `/`-separated path is allowed. A backslash-separated Windows
path is treated as one file name, so its directories must not contain `#`.
Output paths come from the exported files.

**Frame range per task.** `vray-standalone-render` and `husk-usd-render` read
the whole animation from one file. They split the frame range into chunks, as
the DCC presets do, with one frame per task by default.

**Tile rendering.** `arnold-kick-tile-render` splits each frame into a grid of
1 to 16 tiles on each axis and renders each tile as its own task. A dependent
Assemble step then joins each frame's tiles into one image. Assembly starts
only after **every** tile of **every** frame has finished. The tile list is
built at submission, and its cost grows with the grid. A 16 × 16 grid fits the
default EXPR limits. A server with `openjd.expr_memory_limit` at its minimum of
4,096 bytes accepts at most a 7 × 7 grid.

**Redshift and GPUs.** `redshift-standalone-render` gates on the Redshift
install, not on a detected GPU. On a worker with Redshift installed but no
usable GPU, list `redshift` under the worker's `capabilities.disable`.

These presets are verified at Tier 1 and Tier 3 only. See [Validation
tiers](#validation-tiers) and each preset's caveat in
`presets/validation-tiers.yaml`.

## Testing presets

The official library also ships four `Testing`-category presets for
smoke-testing a fresh farm without a DCC installed. They are authored under
`presets/testing/` and published to the library under a `testing/` namespace:

- `test-render-bash` / `test-render-powershell`: a no-op render simulator that
  sleeps per frame and can inject failures, hangs and progress, to exercise
  task chunking, the worker process executor, and the retry and timeout paths.
- `test-steps-bash` / `test-steps-powershell`: a multi-step job (render →
  publish → notify) that exercises step-dependency gating and cross-step
  ordering.

Install them like the reference presets (Browse → Install as a product), then
submit with the generated form. They require no capability tags. The
PowerShell variants gate on Windows workers. The bash variants carry no OS gate
but run their script directly, so they need a Linux or macOS worker.

---

## Validation tiers

`presets/validation-tiers.yaml` has 49 entries, the 42 presets under
`presets/sqi/` plus the seven built-in products documented in
[`docs/development.md`](development.md#adding-a-product), and states which
validation tier each has reached. The four `presets/testing/` presets are not
in the registry: they run no vendor application, so there is nothing for a
tier to stand in for. The tiers are:

- **Tier 1: argv snapshot.** The template is expanded and resolved through the
  production submit → assign → resolve code path (`internal/presettest`) and
  the resulting command line is compared byte for byte against a reviewed
  golden file.

  **What a passing Tier-1 test proves:** the preset expands to exactly the
  command line that was reviewed, and any change to expansion, resolution or
  the preset's template shows up as a golden diff.

  **What it does not prove:** that the vendor's application accepts that
  command line. No Maya, Nuke, Houdini, Blender, Cinema 4D, 3ds Max, After
  Effects, Mistika or standalone renderer installation runs at Tier 1, so the
  argv is only as correct as the documentation it was derived from. For the
  frame-range presets, the goldens pin what follows from their `CONTIGUOUS`
  chunks (all but Houdini's, below), each a plain `start-end` run of
  consecutive frames:
  - `nuke-write-render` and `nuke-script-render` never pass Nuke's `-F` flag an
    OpenJD step (`1-19:2`, where Foundry documents `1-19x2`). That Nuke reads
    `-F 1-1` as the single frame 1 is shown by the Tier-2 case, not here.
  - The presets that pass a chunk's first and last frame (Maya, Blender,
    Cinema 4D, 3ds Max, the two per-item After Effects presets and the three
    Mistika presets) and V-Ray's `-frames=START-END` never render a frame the
    range excluded: a stepped or listed range splits into one chunk per run of
    consecutive frames, so a stepped range yields one single-frame chunk per
    frame. See [sqi-chunk-bounds](openjd-extensions/sqi-chunk-bounds.md).
  - `houdini-rop-render` uses `NONCONTIGUOUS` chunks, so its goldens show a
    compact frame range (`1-8`, or `1-19:2` for a stepped range) that the
    driver script renders with one `rop.render()` call per run of frames.

  Read a preset's `caveat` field before trusting its golden for anything more
  than "the template still expands the way it did when this was reviewed."

- **Tier 2: real application.** The real application runs the resolved command
  and its output is inspected. Eleven entries have a `tier2` block, each naming
  a test that runs the real program end to end:
  - The five ffmpeg presets and the `python-sequence` built-in.
    `test/integration/ffmpeg_presets_test.go` runs real ffmpeg and decodes the
    file it produced, and `TestPresetPythonSequence_LauncherContract` runs
    `python-sequence`'s launcher under a real Python.
  - Five DCC presets: `blender-batch-render`, `blender-view-layer-render`,
    `nuke-write-render`, `nuke-script-render` and `houdini-rop-render`.
    `test/integration/dcc_presets_test.go` builds a scene with the application
    itself (`test/integration/testdata/dcc/`), submits the shipped preset
    through a real server and worker, and checks the files it renders: the exact
    frame set, and where it can be read back, their content (the view layer in
    an EXR header, a PNG's size, the frame a Houdini geometry file holds).

  The other thirty-two presets (Maya, Cinema 4D, 3ds Max, After Effects, the three
  Mistika products, the eight standalone renderers, Katana, Fusion and the two
  Silhouette, two VRED, two KeyShot, two Modo and two Unreal presets) have not reached Tier 2 and carry no `tier2` block.

  A DCC case cannot be required by platform, because CI cannot provide the
  application, so its block says `requires_app: blender` (or `nuke`, `houdini`)
  in place of `required_on`. The case skips when the application is absent, and
  that skip fails the registry check only when `SQI_TEST_REQUIRE_APPS` lists the
  application. CI sets it for Blender, which is free and needs no license, in the
  `blender-presets` job. Nuke and Houdini need a licensed install, so they run
  through `make test-dcc` on a machine that has them. **A run that does not
  require an application proves nothing about it:** look for the
  `--- PASS: TestPresetDCC_` lines.

- **Tier 3: real pipeline, no vendor license.** A real `sqi-server` and
  `sqi-worker`, wired together as in production, execute the preset end to
  end, but the worker's action target is `test/stubproc`, a recording stand-in
  binary, in place of the vendor executable. This proves the sqi side (task
  assignment, worker process execution, argument delivery, exit-status
  handling) without a vendor license, and cross-checks the arguments the stub
  observed against the Tier-1 golden for the same case in both directions, so
  a resolution that collapsed every task onto one argv would fail. Environment
  delivery is **not** covered: the stub records only the command, its
  arguments, its working directory and its stdin.

**The registry is verified.** `TestZZPresetTierRegistrySatisfied` in
`test/integration/preset_tiers_test.go` checks every claim in
`presets/validation-tiers.yaml` against what the suite ran. A Tier-1 claim
needs a fixture case and a golden that exist. A Tier-2 or Tier-3 claim needs a
named test that ran, and a case that skipped on a platform listed in its
`required_on` is a registry failure. An entry's `required_on` may leave out a
platform the preset is gated against, where a skip is expected. For example,
`script` (the POSIX-only built-in) gates on `attr.worker.os.family anyOf
["linux", "macos"]`, so its `tier3.required_on: [linux, darwin]` omits
`windows`. An empty `required_on: []` is rejected at load time, because no skip
could ever fail it.

### Adding a preset to the harness

1. **Fixture**: add a case file at `test/integration/testdata/preset-cases/<preset-name>.yaml`
   describing the job parameters for each scenario you want reviewed (at
   minimum a `default` case).
2. **Golden**: run
   `go test ./test/integration/ -run TestPresetTier1Argv -preset-update` to
   generate `test/integration/testdata/preset-argv/<preset-name>--<case>.golden`
   for each case.
3. **Registry entry**: add the preset to `presets/validation-tiers.yaml` with
   its `tier1.cases` list, a `caveat` stating what the golden is derived from
   and what it does not prove, and, if a Tier-3 case exists, a `tier3` block
   naming the test and the platforms it must not skip on (`required_on`).
4. **Review the diff.** A generated golden is not a passing test until someone
   has read it. Check the argv against the vendor's documented command line,
   not only against what the template was expected to produce.

`make test-preset-harness` runs the whole harness (Tiers 1, 2 and 3, plus the
registry check) in one `go test` invocation. It must be one invocation:
`TestZZPresetTierRegistrySatisfied` checks what the tier tests recorded in the
same process, so a split run fails the registry check with "never reported".
ffmpeg and Python must be on `PATH`, because the `tier2` cases need them and a
skip on a platform named in `required_on` is a registry failure.

### Tier 3 on Windows

Tier 3 runs on Windows as it does on Linux and macOS, and CI runs it in a
dedicated job (`preset-harness-windows`). Of the 49 registry entries, 37 have
`tier3.required_on: [linux, darwin, windows]`, so a skip on Windows fails the
registry check for each of them. The other 12 are gated to fewer platforms:

- POSIX-only (`required_on: [linux, darwin]`), gated on
  `attr.worker.os.family anyOf ["linux", "macos"]`: `script` and
  `command-sequence`.
- Windows-only (`required_on: [windows]`), gated on
  `attr.worker.os.family anyOf ["windows"]`: `script-powershell`,
  `command-sequence-powershell`, `ffmpeg-segment-transcode-powershell`,
  `3dsmax-render`, `after-effects-render-windows`,
  `after-effects-queue-render-windows`, `unreal-mrq-job-render` and
  `unreal-mrq-queue-render`. Only the Windows job runs them.
- macOS-only (`required_on: [darwin]`), gated on
  `attr.worker.os.family anyOf ["macos"]`: `after-effects-render-macos` and
  `after-effects-queue-render-macos`. No CI job runs the preset harness on
  macOS, so these two Tier-3 claims are not exercised in CI.
  `TestPresetAfterEffects_WrapperFailsOnLoggedError` runs their `sh` wrapper
  under Linux with the OS gate stripped; the Windows job runs the same test
  against the PowerShell wrapper.

`TestPresetTier3RequiredOnMatchesOSGate` fails any entry whose
`tier3.required_on` differs from the platforms its template's
`attr.worker.os.family` requirements admit.

`script-powershell` is `script`'s Windows counterpart: where `script` invokes
`/bin/sh -c`, `script-powershell` runs the command as an embedded `.ps1` with
`powershell -File`, so every worker platform has a generic single-command
built-in.
