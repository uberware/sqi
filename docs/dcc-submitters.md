# DCC submitters

`sqi-submitter` is a Python package that adds in-application job submission to
Maya, Houdini, Nuke, and Blender, on top of [`sqi-sdk`](python-client.md). It
is a separate PyPI package (`pip install sqi-submitter`) because it needs a DCC
host or a Qt binding, while `sqi-sdk` is a scripting library with no UI.

---

## Overview

The package is layered so the parts that need Qt or a DCC's Python API stay
out of the parts that don't:

```
sqi_submitter/
  core/     no Qt, no DCC imports — session, form model, pre-fill, HostAdapter
  qt/       Qt only (PySide6, falling back to PySide2) — the generic dialog
  hosts/    one subpackage per DCC — adapter + launch glue
    maya/ houdini/ nuke/ blender/
```

`core` imports with no Qt and no DCC installed (`python -c "import
sqi_submitter.core"`). It holds the server session (`SubmitterSession`), the
parameter form model (`FormModel`, which follows the web submission form's
widget and validation rules), scene-context pre-fill (`prefill`), and the
`HostAdapter` contract. `qt` builds a generic submit dialog from the same form
model; Maya, Houdini, and Nuke use it, since each bundles a PySide binding.
Blender ships no Qt, so `hosts/blender` is a native `bpy` N-panel bound to the
same `FormModel`.

The submitter is **product-driven** and hard-codes no per-DCC fields. It
fetches the product catalog (`GET /api/v1/products`), lets the artist pick a
product, fetches its parameters (`GET /api/v1/products/{name}/parameters`), and
renders a form from that schema, as the web `/submit` page does. The
DCC-specific part is reading the scene (its path, frame range, and selected
render target) and pre-filling matching parameters by **name**, never by which
product was chosen. See [Reference presets](#reference-presets) for the DCC
products this repo ships and [The parameter convention
contract](#the-parameter-convention-contract) for how pre-fill works.

---

## Installation per host

Install `sqi-submitter` into the Python the DCC uses: its bundled interpreter,
or a shared site directory the DCC is configured to see. It depends only on
`sqi-sdk`. No Qt install is needed inside a DCC: Maya, Houdini, and Nuke bundle
PySide, and the Blender UI does not use Qt.

```sh
pip install sqi-submitter
```

> **Note:** `sqi-submitter` is not yet published to PyPI. Until it is, install
> the wheel attached to a GitHub release, as for
> [`sqi-sdk`](python-client.md#installation). The commands below assume the
> package is available to the DCC's interpreter.

### Maya

```sh
mayapy -m pip install sqi-submitter
```

Wire the launch glue with a Maya module. The package ships a **template** at
`sqi_submitter/hosts/maya/startup/sqi.mod`:

```
+ sqi 1.0 <path>
PYTHONPATH +:= .
```

Copy it into a directory on `MAYA_MODULE_PATH` and replace the literal
`<path>` placeholder with the directory that contains the installed
`sqi_submitter` package (e.g. the `mayapy` site-packages directory, or your
shared site dir). Also copy `sqi_submitter/hosts/maya/startup/userSetup.py`
next to it (or anywhere already on `PYTHONPATH` that Maya's `userSetup.py`
mechanism picks up):

```python
from maya import cmds

cmds.evalDeferred("from sqi_submitter.hosts.maya.menu import install_menu; install_menu()")
```

`evalDeferred` waits until Maya's UI is ready before installing the menu.
Restart Maya; a **sqi** menu appears.

> **For a submitted job to run,** the rendering worker needs the `maya=true`
> capability tag and Maya's `Render` binary on its `PATH`. Without a tagged
> worker the job stays `ready`. See [Reference presets](#reference-presets).

### Houdini

```sh
hython -m pip install sqi-submitter
```

Houdini packages are JSON files. `sqi_submitter/hosts/houdini/startup/sqi_submitter.json`
is a **template**:

```json
{
  "env": [{ "PYTHONPATH": { "value": "<path>", "method": "append" } }]
}
```

Copy it into a Houdini packages directory (e.g. `$HOUDINI_USER_PREF_DIR/packages/`)
and replace the literal `<path>` placeholder with the directory containing the
installed `sqi_submitter` package. Then add a shelf tool whose body is:

```python
from sqi_submitter.hosts.houdini.menu import open_submitter
open_submitter()
```

Restart Houdini; click the shelf tool to open the submit dialog.

> **For a submitted job to run,** the rendering worker needs the `houdini=true`
> capability tag and `hython` on its `PATH`. Without a tagged worker the job
> stays `ready`. See [Reference presets](#reference-presets).

### Nuke

```sh
/path/to/Nuke14.0/python -m pip install sqi-submitter
```

Copy `sqi_submitter/hosts/nuke/startup/menu.py` into any directory on
`NUKE_PATH`. It is a two-line snippet:

```python
from sqi_submitter.hosts.nuke.menu import install_menu
install_menu()
```

Nuke runs `menu.py` files on its path at startup; a **sqi** menu appears once
Nuke restarts.

> **For a submitted job to run,** the rendering worker needs the `nuke=true`
> capability tag and `nuke` on its `PATH`. Without a tagged worker the job stays
> `ready`. See [Reference presets](#reference-presets).

### Blender

```sh
/path/to/blender/python/bin/python3.11 -m pip install sqi-submitter
```

Blender's add-on system takes a single add-on file, so after installing the
package into Blender's Python, enable the add-on inside it:
`sqi_submitter/hosts/blender/addon.py`. In Blender, use **Edit → Preferences →
Add-ons → Install…** and point it at that file (or copy the file into your
Blender scripts/add-ons directory), then enable **"sqi Submitter"**.

A **sqi** tab appears in the 3D viewport's N-panel (sidebar):

1. Click **Refresh Products**. It fetches the product list, the parameters of
   the selected product and target, and the farms and queues. It runs off the
   main thread, so Blender's UI does not block while it loads.
2. Pick a **Product** and, if the scene has renderable units, a **Target**
   (scene × view layer).
3. Edit the generated parameter fields, job name, farm, and queue.
4. Optionally, under **Advanced (job overrides)**, set owner, priority,
   project, or the retry policy (max attempts, retry delay, failure limit). A
   field left at 0 or blank inherits the queue → farm → server default.
5. Click **Submit**.

**Changing the product or target does not refetch parameters.** Click
**Refresh Products** again after changing either dropdown, or the form still
shows the previous selection's fields.

> **For a submitted job to run,** the rendering worker needs the `blender=true`
> capability tag and `blender` on its `PATH`. Without a tagged worker the job
> stays `ready`. See [Reference presets](#reference-presets).

### Standalone (`sqi-submit`)

For testing outside a DCC, or for a host not listed above, install the `qt`
extra, which pulls in PySide6, and use the `sqi-submit` console command:

```sh
pip install 'sqi-submitter[qt]'
sqi-submit                          # opens the dialog against the configured server
sqi-submit --server http://farm.example:8080
```

`sqi-submit` needs a Qt binding: PySide6 (from `[qt]`) or a PySide2 already on
the interpreter's path, such as a DCC's own Python. With neither, it fails with
an error that names the install command.

### Server URL configuration

Every submitter resolves the server URL the same way, in order:

1. An explicit `server_url` argument (`sqi-submit --server ...`, or
   `open_for_adapter(adapter, server_url=...)` from host glue).
2. The `SQI_SERVER_URL` environment variable.
3. The `server_url` key in the per-user settings file, `~/.sqi/submitter.json`
   (override the path with `$SQI_SUBMITTER_SETTINGS`). The Qt dialog also
   stores the last product used per host and the save-before-submit checkbox
   state there.
4. `http://localhost:8080`.

### Owner field and `jobs.submit_as`

The Owner field appears only when the session's credential holds the
`jobs.submit_as` permission, checked once per session through `client.me()`.
Without it the server takes the owner from the authenticated user and rejects
a client-supplied owner with 403. When the permissions cannot be read (a server
whose `/auth/me` has no `permissions` field, or any error), the field stays
visible, since the server enforces the permission either way.

---

## Reference presets

sqi ships seventeen DCC reference products under `presets/sqi/`, published
through the [preset library](preset-library.md). The directory holds thirty
products in total. The other thirteen are eight [standalone renderer
presets](preset-library.md#standalone-renderer-reference-presets), which render
exported scene files without loading the DCC, and five `Transcoding`-category
[ffmpeg presets](preset-library.md#transcoding-reference-presets). Install the
DCC presets from **Admin → Preset Library** in the web UI or, without a preset
index, create them through `POST /api/v1/products` or **Admin → Products**. All
seventeen use the product `category` `Rendering`. All but the two After Effects
queue presets (one task each) declare `TASK_CHUNKING` to split the frame range;
the Maya, Blender, Cinema 4D, 3ds Max, After Effects render and Mistika presets
also declare `SQI_CHUNK_BOUNDS` to expose each chunk's start and end frame (see
[OpenJD extensions](openjd-extensions.md)).

Seven of the seventeen have a matching `sqi-submitter` host (Maya, Houdini,
Nuke, Blender). The other ten (Mistika, Cinema 4D, 3ds Max and After Effects)
have none and are submitted from the web UI or the API.

| Product | Command | Chunking | Parameters | Worker tag |
|---|---|---|---|---|
| `maya-layer-render` | `Render -r <renderer> -s/-e <frame> -rl <layer> -rd <dir> <scene>` | one frame per task by default (raise the chunk size to batch) | `SceneFile`, `Frames`, `OutputDir`, `Renderer` (default `file`), `RenderLayer` (default `masterLayer`) | `attr.worker.tag.maya = "true"` |
| `maya-scene-render` | `Render -r <renderer> -s/-e <frame> -rd <dir> <scene>` (no `-rl`) | one frame per task by default (raise the chunk size to batch) | `SceneFile`, `Frames`, `OutputDir`, `Renderer` (default `file`) | `attr.worker.tag.maya = "true"` |
| `houdini-rop-render` | `hython` running an embedded chunk script that loads the hip file and calls `rop.render()` once per `lo-hi:step` part of the chunk's frames, merging consecutive parts | chunks of 10 | `SceneFile`, `Frames`, `RopPath` | `attr.worker.tag.houdini = "true"` |
| `nuke-write-render` | `nuke -x -X <writeNode> -F <range> <script>` | chunks of 10 | `SceneFile`, `Frames`, `WriteNode` | `attr.worker.tag.nuke = "true"` |
| `nuke-script-render` | `nuke -x -F <range> <script>` (no `-X`; all enabled Write nodes) | chunks of 10 | `SceneFile`, `Frames` | `attr.worker.tag.nuke = "true"` |
| `blender-batch-render` | `blender -b <file> -o <output> -s/-e <frame range> -a` | one frame per task by default (raise the chunk size to batch) | `SceneFile`, `Frames`, `OutputPath` (always passed as `-o`; a host submitter fills a blank one from the scene) | `attr.worker.tag.blender = "true"` |
| `blender-view-layer-render` | `blender -b <file> -S <scene> -o <output> -s/-e <frame range> --python-exit-code 1 --python-expr <select-layer> -a -- <layer>` (one named view layer) | one frame per task by default (raise the chunk size to batch) | `SceneFile`, `Frames`, `OutputPath`, `Scene`, `ViewLayer` | `attr.worker.tag.blender = "true"` |
| `cinema4d-render` | `Commandline -nogui -render <scene> -frame <start> <end> -oimage <path>` | one frame per task by default (raise the chunk size to batch) | `SceneFile`, `Frames`, `OutputPath` | `attr.worker.tag.cinema4d = "true"` |
| `cinema4d-take-render` | `Commandline -nogui -render <scene> -frame <start> <end> -take <take> -oimage <path>` | one frame per task by default (raise the chunk size to batch) | `SceneFile`, `Frames`, `Take`, `OutputPath` | `attr.worker.tag.cinema4d = "true"` |
| `3dsmax-render` | `3dsmaxcmd -v:5 -rfw:0 -start:<n> -end:<n> -outputName:<path> <scene>` | one frame per task by default (raise the chunk size to batch) | `SceneFile`, `Frames`, `OutputPath` | `attr.worker.tag.3dsmax = "true"` and `attr.worker.os.family = "windows"` |
| `after-effects-render-windows` | `aerender -project <scene> -rqindex <n> -s <start> -e <end> -v ERRORS_AND_PROGRESS -close DO_NOT_SAVE_CHANGES`, run through an embedded PowerShell wrapper | up to `FramesPerTask` per task, default 10 | `SceneFile`, `Frames`, `RenderQueueItem`, `FramesPerTask` | `attr.worker.tag.aftereffects = "true"` and `attr.worker.os.family = "windows"` |
| `after-effects-render-macos` | the same `aerender` arguments, run through an embedded POSIX `sh` wrapper | up to `FramesPerTask` per task, default 10 | `SceneFile`, `Frames`, `RenderQueueItem`, `FramesPerTask` | `attr.worker.tag.aftereffects = "true"` and `attr.worker.os.family = "macos"` |
| `after-effects-queue-render-windows` | `aerender -project <scene> -v ERRORS_AND_PROGRESS -close DO_NOT_SAVE_CHANGES` (renders the saved queue as is), through the PowerShell wrapper | one task for the whole queue | `SceneFile` | `attr.worker.tag.aftereffects = "true"` and `attr.worker.os.family = "windows"` |
| `after-effects-queue-render-macos` | the same queue render, through the `sh` wrapper | one task for the whole queue | `SceneFile` | `attr.worker.tag.aftereffects = "true"` and `attr.worker.os.family = "macos"` |
| `mistika-boutique-render` | `mistika -r <scene> -s/-e <frame range>` | chunks of 10 | `SceneFile` (`*.rnd`), `Frames` | `attr.worker.tag.mistika = "true"` |
| `mistika-vr-render` | `vr -r <scene> -s/-e <frame range>` | chunks of 10 | `SceneFile` (`*.rnd`), `Frames` | `attr.worker.tag.mistikavr = "true"` |
| `mistika-workflows-render` | `workflows -r <scene> -s/-e <frame range>` | chunks of 10 | `SceneFile` (`*.rnd`), `Frames` | `attr.worker.tag.mistikaworkflows = "true"` |

Maya and Nuke each ship a **single-target** product (`-rl <layer>` /
`-X <writeNode>`) and a **whole-scene** product that omits that flag: Maya's
renders the scene's renderable layers, Nuke's every enabled Write node.
Houdini has one product, because its output target is the ROP the script
renders rather than a flag that can be left out. Blender, Cinema 4D and After
Effects each ship a **per-target** and a **whole-scene** product:
`blender-view-layer-render` renders one named view layer, `blender-batch-render`
the scene as saved; `cinema4d-take-render` renders one named take,
`cinema4d-render` names no take; `after-effects-render-*` renders one
render-queue item in frame chunks, `after-effects-queue-render-*` everything
the saved project has queued, as one task. 3ds Max ships only `3dsmax-render`.
Each After Effects preset has a Windows and a macOS variant with the same
parameters and `aerender` arguments; only the wrapper script differs. Pick the
one that matches your render nodes.

Maya, Blender, Cinema 4D and 3ds Max default to one frame per task (chunk size
1) and declare `SQI_CHUNK_BOUNDS`: their start and end frame arguments come
from the chunk's `Frame.Start`/`Frame.End`, so raising the step's
`chunks.defaultTaskCount` renders a contiguous frame range per DCC launch with
no other template change. Houdini and Nuke default to chunks of 10, because
the embedded `hython` script and `nuke -F` both take several frames per
invocation, which spreads DCC startup across several frames. Houdini's chunks
are `NONCONTIGUOUS`: the script receives a compact frame range such as
`1-19:2`, so a stepped range such as `1-60:2` still renders ten frames per
task, in one `rop.render()` call. Nuke's are `CONTIGUOUS`, because Foundry
documents no comma-list form of `-F`, so a stepped range runs one task per
frame.

The three Mistika presets use `SQI_CHUNK_BOUNDS` like Maya and Blender but
default to chunks of 10: every Mistika renderer takes `-s START -e END`, so a
chunk maps onto one invocation. All three render `.rnd` job files. For a
single movie-file render, submit `Frames` as `1`.

**After Effects runs `aerender` through a wrapper, and a movie output must be
one task.** `aerender` is reported to exit 0 when a render fails, so the four
After Effects presets run it through a small embedded script (PowerShell on
Windows, POSIX `sh` on macOS). The wrapper passes `aerender`'s output to the
task log unchanged and fails the task when `aerender` exits non-zero or when
any output line contains `aerender ERROR`, in any letter case. A failure that
does neither is still reported as a success. Each render node needs `aerender`
on its `PATH` (and `powershell` on Windows; macOS runs the wrapper with
`/bin/sh`), and Adobe's render-only marker, an empty file named
`ae_render_only_node.txt` in `C:\Users\Public\Documents\Adobe` (Windows) or
`/Users/Shared/Adobe` (macOS). After Effects cannot render one movie file from
several machines, so when the render-queue item writes a movie, set
`FramesPerTask` to at least the number of frames in the range and enter the
range with no step and no gaps, such as `1-240`, so the whole range is one
task. A stepped or listed range always splits at each gap. The preset cannot
see the output format, so nothing enforces this.

Each preset's `hostRequirements` gates it to workers advertising the matching
`attr.worker.tag.<name>` capability tag. `3dsmax-render` and the After Effects
presets also require `attr.worker.os.family` (`windows` for `3dsmax-render`,
`windows` or `macos` for the matching After Effects variant). At startup
`sqi-worker` detects a standard Maya, Nuke, Houdini, Blender, Cinema 4D, 3ds
Max, After Effects or Mistika install, on `PATH` or in its usual install
location, and advertises the matching tag (`maya`, plus a version variant such
as `maya-2025`) with value `"true"`; see [Capability
auto-detection](worker-capabilities.md#capability-auto-detection-built-in-dcc-detectors).
That satisfies the `anyOf: ["true"]` requirement each of the seventeen presets
declares, so a worker with a standard install needs no per-worker
configuration. Run `sqi-worker capabilities` (or `start --dry-run`) to see
which DCCs were found.

**The Mistika detectors probe install locations only, never `PATH`.** The
binaries are named `mistika`, `vr` and `workflows`; the last two are generic
enough that a `PATH` probe would tag workers with no Mistika installed, which
would then be handed render work they cannot run. Detection and execution
therefore use different evidence: a worker is tagged because Mistika is
installed under the SGO layout, but the preset invokes the bare command, so
the binary must also be on the worker's `PATH`. If your SGO install is not on
`PATH`, add it, or duplicate the preset and give `command:` an absolute path.

The Cinema 4D, 3ds Max and After Effects detectors have the same gap. They tag
a worker from where the application is installed (and, for 3ds Max and After
Effects, from the command on `PATH`), but a registry or install-location match
does not put `Commandline`, `3dsmaxcmd` or `aerender` on the worker's `PATH`,
and the presets invoke the bare command. Add the install folder to `PATH` on
each such node.

Manual tags are needed only when a DCC is installed somewhere the built-in
detectors do not check, or for in-house tools they do not cover. Add a
`key=value` entry to the worker's `capability_tags` (see [worker capability
tags](worker-capabilities.md)):

```yaml
worker:
  capability_tags:
    - maya=true
```

(substitute `houdini=true`, `nuke=true`, `blender=true`, `cinema4d=true`,
`3dsmax=true`, `aftereffects=true`, `mistika=true`, `mistikavr=true`, or
`mistikaworkflows=true` for the other presets), or add a [custom
detector](worker-capabilities.md#writing-custom-detectors). A job submitted
against one of these products stays `ready` until a worker advertises the tag,
whether detected or set by hand.

---

## The parameter convention contract

Pre-fill (the values an adapter reads from the scene) binds to a product's
parameters **by name only**, ignoring case and `_`/`-` separators (`SceneFile`,
`scene_file`, and `Scene-File` are the same key), never by which product was
picked. A studio can therefore duplicate a reference preset, rename it, and
rewrite its command, and submitters keep pre-filling it as long as the
parameter names below are kept.

| Convention key | Matches parameter names | Source |
|---|---|---|
| `scene_path` | `SceneFile`, `ScenePath` | `SceneContext.scene_path` |
| `frame_range` | `Frames`, `FrameRange` | `SceneContext.frame_range`, overridden by the selected render target's range |
| `output_path` | `OutputDir`, `OutputPath` | `SceneContext.output_path`, overridden by the selected render target's output path |
| `renderer` | `Renderer` | `SceneContext.renderer` |

### The scene file is host-managed

Inside a DCC (whenever a `HostAdapter` is present), the scene-path parameter
(`SceneFile`/`ScenePath`) is **not shown in the submit form**. It is taken from
the open file at submit time, and submitting fails with "Save your scene
before submitting" if the file has never been saved, since the farm renders
the file on disk. `submit_form` in `core/submit.py` enforces this, so the Qt
dialog and the Blender panel behave the same. The standalone `sqi-submit`
dialog has no adapter, so there the scene field stays visible and editable.

The reference presets share parameter labels, so the hosts read the same:
`SceneFile` → "Scene File", `Frames` → "Frame Range", Maya's `OutputDir` →
"Output Directory", Blender's `OutputPath` → "Output Path". Labels live in the
product template (`userInterface.label`), so a product created locally must be
re-created from the updated preset to pick up a label change.

**Blender output is optional in a host submitter.** The Blender presets pass
`OutputPath` to Blender's `-o`, and it defaults to blank. Left blank,
`submit_form` fills it from the scene's own output setting
(`SceneContext.output_path`), so a default submission renders like a local
render, with Blender's frame numbering and file naming. Set it to send output
elsewhere, such as shared storage. If the field and the scene's output setting
are both blank, `submit_form` refuses the job with "No output path set" rather
than let Blender write into the worker's working directory. Outside a host
submitter (the web UI, the API, `sqi-submit`) nothing fills it: the presets
always pass `-o`, so a blank value reaches Blender as an empty path, and what
Blender does with that is unverified. Set it there. Maya's `OutputDir` is an
output directory (`Render -rd`); Blender's `-o` is a path prefix.

**`PATH` parameters get file and directory pickers.** A `PATH` parameter's
`userInterface.control` must be `CHOOSE_INPUT_FILE`, `CHOOSE_OUTPUT_FILE`,
`CHOOSE_DIRECTORY`, `DROPDOWN_LIST`, or `HIDDEN`; the server rejects
`LINE_EDIT` there with a 422. The Qt dialog shows a browse button and the
label for a `CHOOSE_*` control, and no picker for `HIDDEN`. See [PATH
parameters, labels, and pickers](#path-parameters-labels-and-pickers) below,
including the fallback for a `PATH` parameter with no `userInterface` block.

A selected render target can also supply **exact-name** extras outside the
general convention (matched ignoring case and separators, but with no
aliases):

| Target parameter | Host | Meaning |
|---|---|---|
| `RenderLayer` | Maya | render layer |
| `RopPath` | Houdini | ROP path |
| `WriteNode` | Nuke | Write node |
| `Scene`, `ViewLayer` | Blender | the **scene name** (not a path — see `hosts/blender/adapter.py`) and the view layer |
| `Take` | Cinema 4D | take name |
| `RenderQueueItem` | After Effects | the render-queue item's 1-based index (an `INT` parameter, minimum 1; extra values are strings) |

Blender's target picker is scene × view-layer. `blender-view-layer-render`
declares both `Scene` and `ViewLayer`, so selecting a target there fills them;
`blender-batch-render` declares neither, so selecting a target there only
drives the `frame_range`/`output_path` override. A parameter named `Scene` always
gets the scene name: extras win over convention aliases (below), and `scene`
is not a `scene_path` alias.

`Take` and `RenderQueueItem` are the names `cinema4d-take-render` and
`after-effects-render-*` declare. `sqi-submitter` has no Cinema 4D or After
Effects host, so nothing fills them automatically; enter them by hand in the
web UI or pass them through the API.

Rules that keep this contract stable:

- **Additive-only.** New convention keys or aliases may be added; existing
  ones are never renamed or removed. A form field that matches no known key or
  extra is left unfilled, which is not an error.
- **Extras win over convention aliases.** If the selected render target's
  `extra` map has a key matching a parameter's name, that value is used even
  when the name also matches a convention alias.
- **Never keyed on product identity.** The mapping code only asks whether a
  parameter's name matches an alias, such as one of `scene_path`'s. A custom,
  differently named product that declares a parameter called `SceneFile` gets
  the same pre-fill as the reference preset.
- **Target overrides scene.** If the selected render target has its own frame
  range or output path, that value replaces the scene-level one for
  `frame_range`/`output_path`.

### PATH parameters, labels, and pickers

The legal `userInterface` controls depend on the parameter type (see
[`docs/openjd-extensions.md`](openjd-extensions.md) and
[`docs/products.md`](products.md#userinterface-parameter-hints)). On a `PATH`
parameter they are `CHOOSE_INPUT_FILE`, `CHOOSE_OUTPUT_FILE`,
`CHOOSE_DIRECTORY`, `DROPDOWN_LIST`, and `HIDDEN`. `LINE_EDIT`,
`MULTILINE_EDIT`, `CHECK_BOX`, and `SPIN_BOX` belong to other types, and the
server rejects them on a `PATH` with a 422. A `userInterface` block, when
present, must carry a `control`.

Every reference preset gives its `PATH` parameters a label and the `CHOOSE_*`
control matching `objectType`/`dataFlow`: `CHOOSE_INPUT_FILE` for an input
such as `SceneFile`, `CHOOSE_DIRECTORY` for a directory output,
`CHOOSE_OUTPUT_FILE` for a file output. The Qt dialog renders that chooser
with the label beside it, both read from the template.

A `PATH` parameter with **no** `userInterface` block (no control and no label)
still gets a picker in the submitter: `FormField.widget` in
`clients/submitter/src/sqi_submitter/core/schema.py` derives it from
`objectType`/`dataFlow` (`objectType: DIRECTORY` → folder picker,
`dataFlow: OUT` → save dialog, otherwise an open-file dialog), where an
unlabeled `STRING` would get a plain text field. An explicit `HIDDEN` shows no
picker.

Only the Qt dialog has a browse button, and nothing beyond the declared
control is stored in the OpenJD document. The web submission form
(`selectWidget` in `web/src/lib/productForm.ts`) renders a `PATH` parameter as
a labeled text field, since a browser cannot browse the farm filesystem, and
the Blender panel uses a `bpy` string property. All three read the same
`control` and `label` hints.

---

## Customizing for your pipeline

The reference presets are named to leave room for variants
(`maya-layer-render` / `maya-scene-render` rather than one "Maya product"), and
their descriptions present them as starting points. To adapt one:

1. Open it in **Admin → Products**, or fetch it with
   `GET /api/v1/products/{name}` and `POST` it again under a new name.
2. If you installed it from the library, use **Duplicate to custom** (see
   [`docs/preset-library.md`](preset-library.md#installed-product-lifecycle))
   to get an editable copy that library updates no longer touch.
3. Change anything in the template: environment setup, licensing commands,
   extra parameters, a different render command. The product wrapper (name,
   title, category) places no constraint on the OpenJD template.
4. **Keep the convention parameter names** (`SceneFile`, `Frames`, `OutputDir`,
   `Renderer`, and any target extras you rely on) to keep `sqi-submitter`'s
   pre-fill working. A parameter renamed outside the convention table still
   works, but the artist has to fill it in.
5. Keep the `category` (`Rendering`, or your catalog's convention). Category is
   a display grouping the catalog and preset UIs filter on, not an identity.

The submit dialog's **Suggested** product group is a substring match of the
host token against a product's name, title and description. It only orders the
list, hides nothing, and does not affect pre-fill: a custom Maya product
without "maya" in those fields is listed in the **All** group and still
pre-fills.

---

## Writing a new adapter

Another Python-hosting tool plugs in by subclassing
`sqi_submitter.core.HostAdapter`, the whole extension contract:

```python
from sqi_submitter.core import HostAdapter, RenderTarget, SceneContext


class MyToolAdapter(HostAdapter):
    host_name = "mytool"       # lowercase token used for suggested-product
                                # grouping and per-host settings keys
    display_name = "My Tool"

    def scene_context(self) -> SceneContext:
        # Import your host's API lazily, inside the method body — never at
        # module import time — so this module stays importable (and testable)
        # without the host installed.
        import mytool_api

        return SceneContext(
            scene_path=mytool_api.current_scene_path(),
            frame_range=mytool_api.current_frame_range(),
        )

    def render_targets(self) -> list[RenderTarget]:
        import mytool_api

        return [
            RenderTarget(name=t.name, kind="pass", extra={})
            for t in mytool_api.render_targets()
        ]

    # Optional — default implementations assume "already saved" / "always
    # savable". Override if your host can report modified/unsaved state:
    def is_scene_modified(self) -> bool: ...
    def save_scene(self) -> bool: ...
```

`scene_context()` and `render_targets()` are the only abstract methods. Fetching
the catalog, building the form, pre-filling, validating, and submitting are
shared core code.

Then open the generic Qt dialog with your adapter:

```python
from sqi_submitter.qt.dialog import open_for_adapter

open_for_adapter(MyToolAdapter())
```

For a native (non-Qt) host UI, call `sqi_submitter.core` directly
(`SubmitterSession`, `FormModel.from_parameters(...)`, `prefill(...)`, and
`submit_form(...)`), as `sqi_submitter/hosts/blender/addon.py` does over `bpy`.

The minimal example is `MiniAdapter` in
`clients/submitter/tests/test_adapter_contract.py`, which drives the whole flow
(product fetch → parameters → pre-fill → validate → submit) against a fake
scene and target.

**Launch-glue checklist** for a new host (see [Installation per
host](#installation-per-host) for the four existing examples):

1. An adapter module under `hosts/<name>/adapter.py`.
2. A menu/launch entry point (`hosts/<name>/menu.py`) that calls
   `open_for_adapter(YourAdapter())` for Qt-embedding hosts, or a native panel
   for hosts without Qt.
3. A startup snippet under `hosts/<name>/startup/` showing how to wire the
   entry point into the host's own plugin/module-path mechanism.
4. A section in this doc.

---

## Manual test checklist per DCC

Maya, Houdini, and Nuke are verified by hand, since CI has no runner with
licensed software. Blender, which is free to install, has an automated
integration smoke test (`clients/submitter/tests/integration/test_e2e.py`, run
with `SQI_TEST_BLENDER=1`); still check it by hand after changing its adapter
or add-on.

For each host, after installing per [Installation per host](#installation-per-host):

1. **Open scene → launch submitter.** Open (or create) a scene with a saved
   path. Trigger the sqi menu item / shelf tool / N-panel.
2. **Dialog opens with a suggested product** (Qt hosts). The product picker
   shows a **Suggested** group containing the host's reference presets (Maya
   suggests `maya-layer-render` and `maya-scene-render`) and an **All** group
   with every other product. The Blender panel lists products ungrouped.
3. **Target picker lists render units.** If the scene has renderable units
   (Maya render layers, Houdini ROPs, Nuke Write nodes, Blender view layers),
   the target picker is visible and lists them; with none, it is hidden.
4. **Pre-fill is correct.** `SceneFile` matches the open scene's path,
   `Frames` matches the current frame range (or the selected target's range),
   and any target extra (`RenderLayer`/`RopPath`/`WriteNode`/`Scene`/`ViewLayer`)
   matches the selected unit. Selecting a different target re-applies
   pre-fill (in Blender, after **Refresh Products**).
5. **Submit → job appears in the web UI.** Pick a farm and queue, submit, and
   confirm the success dialog's job URL opens the job in the web UI with the
   parameters you expect.
6. **Unsaved scene.** Modify the scene without saving, then submit. The
   submitter saves the scene before posting the job (checkbox "Save scene
   before submit", on by default; the Qt dialog remembers it). If the scene
   has never been saved, submit refuses with a message asking you to save it
   first.
7. **Unreachable server.** Point `SQI_SERVER_URL` (or the standalone
   `--server` flag) at an address nothing listens on, relaunch, and confirm the
   Qt dialog shows a connection error banner with a **Reload** button, or the
   Blender panel reports the error on **Refresh Products**, rather than
   crashing or hanging.

---

## Language-neutral submission contract (for future non-Python submitters)

`sqi-submitter` is Python and Qt, so it cannot run inside an application that
does not host Python, such as After Effects (scripted in ExtendScript, CEP or
UXP JavaScript). The submission contract does not need Python: it is three
REST calls against the same server every other client uses.

| Call | Purpose |
|---|---|
| `GET /api/v1/products` | List the product catalog (name, title, description, category). |
| `GET /api/v1/products/{name}/parameters` | Fetch the chosen product's parsed parameter schema, including `userInterface` hints, to build a form. |
| `POST /api/v1/products/{name}/jobs` | Submit a job: body `{ "farm_id", "queue_id", "name", "parameters": { ... }, plus optional "owner", "submitter", "priority", "project", "depends_on", "max_attempts", "retry_delay_seconds", "failure_limit" }`. |

`internal/api/openapi.yaml` is the authoritative wire contract for all three;
where it and this doc disagree, it wins. The [parameter convention
contract](#the-parameter-convention-contract) applies unchanged: a JavaScript
submitter that pre-fills from its host matches the same parameter names, with
the same case- and separator-insensitive rules and the same additive-only
guarantee. Building one needs only an HTTP client.

---

## See also

- [`docs/python-client.md`](python-client.md) — `sqi-sdk`, the scripting
  library `sqi-submitter` is built on.
- [`docs/products.md`](products.md) — the product/preset catalog model and
  `userInterface` parameter hints.
- [`docs/preset-library.md`](preset-library.md) — installing the reference
  presets (or any preset) from a hosted index.
- [`docs/worker-capabilities.md`](worker-capabilities.md) — the capability
  tags the reference presets gate on, and their built-in detectors.
- [`docs/development.md`](development.md#the-sqi-submitter-package) — the
  package's dev gate and layout.
