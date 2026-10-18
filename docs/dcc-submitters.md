# DCC submitters

`sqi-submitter` is a Python package that adds in-application job submission to
Maya, Houdini, Nuke, 3ds Max, Blender, Cinema 4D and After Effects, on top of
[`sqi-sdk`](python-client.md). It is a separate PyPI package
(`pip install sqi-submitter`) because it needs a DCC host or a Qt binding,
while `sqi-sdk` is a scripting library with no UI.

---

## Overview

Every host opens the **same submit dialog**. What differs is how it gets
opened:

- **In-process.** Maya, Houdini, Nuke and 3ds Max run Python and bundle a Qt
  binding, so `sqi-submitter` is installed into the host's Python and the host
  opens the dialog itself from a menu item or shelf tool.
- **Handoff.** Blender, Cinema 4D and After Effects either bundle no Qt or
  cannot host `sqi_submitter` at all. A small self-contained file in the host
  reads the open scene, writes a **context file** (JSON), and starts
  `sqi-submit --context FILE` as a separate process. That process opens the
  same dialog, filled from the file. See [The handoff contract](#the-handoff-contract).

| Host | Opens the dialog | Installed as | Targets (per-target products) |
|---|---|---|---|
| Maya | in-process | `sqi.mod` module + `userSetup.py` | render layers |
| Houdini | in-process | package JSON + shelf tool | ROP nodes |
| Nuke | in-process | `menu.py` on `NUKE_PATH` | Write nodes |
| 3ds Max | in-process | Autodesk application package | none (whole-scene only) |
| Blender | handoff | one add-on file (`sqi-submit install-host blender`) | scene and view layer pairs |
| Cinema 4D | handoff | one `.pyp` plug-in file (`sqi-submit install-host cinema4d`) | checked takes |
| After Effects | handoff | a ScriptUI panel `.jsx` plus `json2.js` | `Queued` render-queue items |

The package is layered so the parts that need Qt or a DCC's Python API stay
out of the parts that don't:

```
sqi_submitter/
  core/     no Qt, no DCC imports — session, form model, pre-fill, HostAdapter,
            the handoff context file reader, multi-target submission
  qt/       Qt only (PySide6, falling back to PySide2) — the generic dialog
  hosts/    one subpackage per DCC
    maya/ houdini/ nuke/ max/           adapter + launch glue (in-process)
    blender/ cinema4d/ aftereffects/    one self-contained host file each (handoff)
  cli.py    the sqi-submit command
  install.py  sqi-submit install-host
```

`core` imports with no Qt and no DCC installed (`python -c "import
sqi_submitter.core"`). It holds the server session (`SubmitterSession`), the
parameter form model (`FormModel`, which follows the web submission form's
widget and validation rules), scene-context pre-fill (`prefill`), the
`HostAdapter` contract, and the context-file reader (`ContextAdapter`, which is
a `HostAdapter` backed by a file). `qt` builds the one dialog from the same
form model.

The files under `hosts/blender`, `hosts/cinema4d` and `hosts/aftereffects` are
**not importable library code**. Each runs inside a host that cannot import
`sqi_submitter`, so each is a single file that uses only that host's standard
library and ships in the wheel as package data.

The submitter is **product-driven** and hard-codes no per-DCC fields. It
fetches the product catalog (`GET /api/v1/products`), lets the artist pick a
product, fetches its parameters (`GET /api/v1/products/{name}/parameters`), and
renders a form from that schema, as the web `/submit` page does. The
DCC-specific part is reading the scene (its path, frame range, and render
targets) and pre-filling matching parameters by **name**, never by which
product was chosen. See [Reference presets](#reference-presets) for the DCC
products this repo ships, [The parameter convention
contract](#the-parameter-convention-contract) for how pre-fill works, and
[Submitting several targets](#submitting-several-targets) for what the dialog
does when a scene has more than one render target.

---

## Installation per host

There are two install shapes, matching the two ways the dialog opens (see the
[host table](#overview)).

- **In-process hosts (Maya, Houdini, Nuke, 3ds Max):** install `sqi-submitter`
  into the Python the DCC uses: its bundled interpreter, or a shared site
  directory the DCC is configured to see. It depends only on `sqi-sdk`. No Qt
  install is needed inside these hosts, since each bundles PySide. **Do not
  install the `[qt]` extra there:** the dialog prefers PySide6 over PySide2, so
  a PySide6 installed into a host that bundles PySide2 (for example 3ds Max
  2024) would be picked over the binding the host's own windows use.
- **Handoff hosts (Blender, Cinema 4D, After Effects):** nothing is installed
  into the host's Python. Copy one self-contained file into the host
  (`sqi-submit install-host` does it where it can), and install
  `sqi-submitter[qt]` on the same machine as an ordinary command-line tool, for
  example with `pipx`, so the `sqi-submit` command exists. See [`sqi-submit
  install-host`](#sqi-submit-install-host).

```sh
pip install sqi-submitter
```

> **Note:** `sqi-submitter` is not yet published to PyPI. Until it is, install
> the wheel attached to a GitHub release, as for
> [`sqi-sdk`](python-client.md#installation). The commands below assume the
> package is available to the interpreter you install it into.

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

### 3ds Max

3ds Max is Windows-only and runs the dialog in-process, so it installs like
Maya, plus an Autodesk application package for the menu. The package declares
`SeriesMin="2024"`.

1. Install the package into Max's own Python, using the `python.exe` in the
   `Python` folder of the 3ds Max install (pip is bundled). Plain
   `sqi-submitter`, not `[qt]`; Max supplies PySide:

   ```sh
   "C:\Program Files\Autodesk\3ds Max 2025\Python\python.exe" -m pip install sqi-submitter
   ```

2. Copy the contents of the package folder
   `sqi_submitter/hosts/max/package/` (a `PackageContents.xml` and a `Contents`
   folder) into `%ProgramData%\Autodesk\ApplicationPlugins\sqi\` for every
   user, or `%APPDATA%\Autodesk\ApplicationPlugins\sqi\` for the current one.
3. Restart 3ds Max.

The package's startup script defines a macroScript, **Submit to sqi...** in the
`sqi` category, which runs `open_submitter()`, and registers a **sqi > Submit to
sqi...** entry in the main menu bar from the `#cuiRegisterMenus` callback. The
menu registration is wrapped in `try`/`catch`, so a Max build that lacks it
still loads the macroScript. **3ds Max 2024 is expected to get the macroScript
only:** add it to a toolbar, quad menu or shortcut through **Customize User
Interface**, category `sqi`. Neither the package loading nor the menu appearing
has been confirmed in a real 3ds Max; see the [3ds Max
checklist](#3ds-max-checklist).

`MaxAdapter` offers no render targets, so every product is whole-scene (see
[Submitting several targets](#submitting-several-targets)). The dialog's
**Save scene before submit** box saves the scene with `saveMaxFile` before the
job is posted; an untitled scene is refused with "Save your scene before
submitting".

> **For a submitted job to run,** the rendering worker needs the `3dsmax=true`
> capability tag and `3dsmaxcmd` on its `PATH`, on Windows. Without a tagged
> worker the job stays `ready`. See [Reference presets](#reference-presets).

### Blender

Blender hands the open scene to `sqi-submit`. The add-on is one file,
`sqi_blender_addon.py`, that uses only Blender's own Python and never imports
`sqi_submitter`.

1. Install `sqi-submit` on the machine, outside Blender's Python:

   ```sh
   pipx install "sqi-submitter[qt]"
   ```

2. Run `sqi-submit install-host blender`. It prints the add-on's path; it does
   not copy anything, because Blender installs add-ons through its own UI.
3. In Blender, use **Edit → Preferences → Add-ons → Install from Disk…**, choose
   that file, and enable **sqi Submitter**. (In Blender 4.2 and later, Install
   from Disk is in the drop-down at the top right of the preferences window.)

A **sqi** tab appears in the 3D viewport's sidebar (press `N`). Its **Submit to
sqi** button saves the file if it has unsaved changes (it does not ask), writes
the context file and starts `sqi-submit`. An unsaved scene is refused with
"Save your scene before submitting". The scene's view layers, one per scene and
view layer pair, are the targets: `blender-view-layer-render` is per-target and
`blender-batch-render` is whole-scene.

If `sqi-submit` is not on Blender's `PATH`, set its full path in the add-on's
preferences (**sqi-submit path**) or as `sqi_submit_command` in
`~/.sqi/submitter.json`; see [How `sqi-submit` is
found](#how-sqi-submit-is-found).

**Upgrading from the native panel.** Earlier alpha builds installed
`sqi-submitter` into Blender's own Python with `pip` and enabled the add-on
`sqi_submitter.hosts.blender.addon`, a native panel with its own form. That
add-on is gone, and there is no migration shim: sqi is alpha. To upgrade:

1. In **Edit → Preferences → Add-ons**, disable and remove the old **sqi
   Submitter** add-on.
2. Optionally remove the package from Blender's Python
   (`/path/to/blender/python/bin/python3.11 -m pip uninstall sqi-submitter`).
   Leaving it does no harm, but nothing uses it.
3. Install the new file as above. The old and the new add-on are both named
   "sqi Submitter", so make sure only the new one is enabled.

> **For a submitted job to run,** the rendering worker needs the `blender=true`
> capability tag and `blender` on its `PATH`. Without a tagged worker the job
> stays `ready`. See [Reference presets](#reference-presets).

### Cinema 4D

Cinema 4D hands the open scene to `sqi-submit`. The plug-in is one file,
`sqi_submit.pyp`, that uses only Cinema 4D's own Python and never imports
`sqi_submitter`.

1. Install `sqi-submit` on the machine, outside Cinema 4D's Python:
   `pipx install "sqi-submitter[qt]"`.
2. Run `sqi-submit install-host cinema4d`. When it finds exactly one `Maxon
   Cinema 4D *` preferences folder, it copies the file into that folder's
   `plugins` folder; otherwise it prints the file's path and what to do (see
   [`sqi-submit install-host`](#sqi-submit-install-host)).
3. Restart Cinema 4D. **Extensions → Submit to sqi…** appears.

The command refuses an unsaved document ("Save your scene before submitting")
and saves a changed one without asking. Its targets are the **checked** takes
other than the main take, each with its own frame range and output path from
the take's effective render settings. A scene with no such take has no targets,
so every product is treated as whole-scene. Takes that share a name (under
different parents, say) are disambiguated by the dialog with `(#<position>)`.

**The plug-in ID is a placeholder.** Cinema 4D identifies a plug-in by a number
that must be registered with Maxon. The file carries `PLUGIN_ID = 1000001`,
from the range Maxon documents for testing, with a `TODO(release)` comment.
**Register an ID at developers.maxon.net and replace it before this plug-in is
used outside a test machine.** Until then, any other plug-in that uses the same
test ID collides with it, and so can a second sqi install: Cinema 4D registers
only one plug-in per ID.

> **For a submitted job to run,** the rendering worker needs the `cinema4d=true`
> capability tag and Cinema 4D's `Commandline` on its `PATH`. Without a tagged
> worker the job stays `ready`. See [Reference presets](#reference-presets).

### After Effects

After Effects hands its render queue to `sqi-submit`. The host side is a
ScriptUI panel written in ExtendScript, `Submit to sqi.jsx`, plus the public
domain `json2.js` it includes (ExtendScript has no native `JSON`). The two
files must sit in the same folder.

1. Install `sqi-submit` on the machine: `pipx install "sqi-submitter[qt]"`.
2. Run `sqi-submit install-host aftereffects`. It prints the two files' paths
   and the destination pattern, `<After Effects install>/Support
   Files/Scripts/ScriptUI Panels`, and copies nothing, because that folder is
   under the program folder and needs administrator rights. Copy both files
   there yourself.
3. In After Effects, turn on **Preferences → Scripting & Expressions → Allow
   Scripts to Write Files and Access Network**, then restart After Effects.
4. Open **Window → Submit to sqi.jsx**. The panel docks like any other. (The
   script also runs as a floating palette from **File → Scripts → Run Script
   File**.)
5. Click **Locate sqi-submit…** and choose the `sqi-submit` program (on
   Windows, `sqi-submit.exe` in the Python `Scripts` folder or pipx's `bin`
   folder). The panel remembers it.

**After Effects does not search `PATH` and does not read
`~/.sqi/submitter.json`.** It uses only the location given to **Locate
sqi-submit…**, kept in After Effects' own settings, because an ExtendScript
panel cannot cheaply parse that file or search `PATH`.

The panel checks, with a plain message for each: a project is open; it has been
saved (a changed project is offered a save); the file/network preference is on;
at least one render-queue item has status **Queued**; `sqi-submit` is
located and still exists; and it is new enough for the panel. The last check
runs `sqi-submit --version` and waits for it (`cmd /c ""<exe>" --version"` on
Windows, `"<exe>" --version` on macOS, with the launch's quoting rules), before
the context file is written. The panel launches `sqi-submit` with `--detached`
(see [Launching it](#launching-it)), which an older `sqi-submit` rejects with a
usage error in a console that closes at once, so the artist would see nothing;
an `sqi-submit` that does not print `sqi-submit <version>` is reported as too
old or not runnable, with how to update it (`pip install -U
"sqi-submitter[qt]"`, then `sqi-submit install-host aftereffects` again) or
pick another with **Locate sqi-submit…**, and nothing is launched. A success is
remembered for that path until After Effects quits, so the check costs one run
per path per session; a failure is not, so an update takes effect on the next
submit. Every **Queued** item is a target, named after its
composition, with a frame range from the composition's display start and the
item's time span. When two queued items render the same composition (a ProRes
and an H.264 output, say), the first keeps the composition name and each later
one is disambiguated with `(#<queue index>)`, its position in the render queue,
so every target, and so every job, gets a distinct name.

An output module file name with a `[#...]` frame token is an image sequence;
anything else is a movie. A movie target carries its whole frame count as
`FramesPerTask` and its own queue-item range as `Frames`, and locks both, so it
renders as one task, since After Effects cannot write one movie from several
machines: a widened range, or one with a step or a gap, would split it. The
dialog therefore always submits a movie's own range; to render another range,
change the render-queue item in After Effects and submit again. With movies and
image sequences ticked together, a **Frame Range** edit applies to the
image-sequence items only. See [Submitting several
targets](#submitting-several-targets).

> **For a submitted job to run,** the rendering worker needs the
> `aftereffects=true` capability tag, `aerender` on its `PATH`, and a
> render-only marker file, and the product must match the worker's OS
> (`windows` or `macos`). See [Reference presets](#reference-presets).

### `sqi-submit install-host`

```
sqi-submit install-host {blender,cinema4d,aftereffects} [--dest DIR]
```

Copies a handoff host's files out of the installed wheel into the host's
per-user location, or into `--dest DIR` (created if missing), and prints what it
did and the next step. The files are package data, so this works with no Qt
binding installed.

| Host | Files | Default destination | Without `--dest` |
|---|---|---|---|
| `blender` | `sqi_blender_addon.py` | none | prints the file's path and the install steps; copies nothing |
| `cinema4d` | `sqi_submit.pyp` | the `plugins` folder under the user's `Maxon Cinema 4D *` preferences folder (`%APPDATA%\Maxon` on Windows, `~/Library/Preferences/Maxon` on macOS), **only when exactly one such folder exists** | copies it; with none or several (or on Linux), prints the path and asks you to copy it or pass `--dest` |
| `aftereffects` | `Submit to sqi.jsx`, `json2.js` | none (needs administrator rights) | prints both paths and the destination pattern; copies nothing |

With `--dest`, every host's files are copied into that one directory, and the
command prints `copied <path>` for each file, then the host's next step. The
copy overwrites a file of the same name.

Exit codes: **0** success (including "printed instructions instead of
copying"); **1** a file could not be copied into the destination (`--dest`, or
Cinema 4D's detected folder): the destination could not be written, or this
build is missing one of the host's files. The message names the folder and the
reason, and says how many files were already copied. **2** unknown host (the
message, on standard error, lists the known hosts). With no destination nothing
is copied, so a missing package file is not detected: its path is printed like
any other and the command exits 0.

### Standalone (`sqi-submit`)

For testing outside a DCC, or for a host not listed above, install the `qt`
extra, which pulls in PySide6, and use the `sqi-submit` console command:

```sh
pip install "sqi-submitter[qt]"
sqi-submit                          # opens the dialog against the configured server
sqi-submit --server http://farm.example:8080
sqi-submit --context FILE           # open the dialog for a handoff context file
sqi-submit --version                # print "sqi-submit <version>" (needs no Qt)
```

`sqi-submit` needs a Qt binding: PySide6 (from `[qt]`) or a PySide2 already on
the interpreter's path, such as a DCC's own Python. With neither, it fails with
an error that names the install command. Run with no `--context`, the dialog has
no host: the scene file field stays editable and there are no targets.

### Server URL configuration

Every submitter resolves the server URL the same way, in order:

1. An explicit `server_url` argument (`sqi-submit --server ...`, or
   `open_for_adapter(adapter, server_url=...)` from host glue).
2. For a handoff, the context file's `server_url`, when it is not `null`.
3. The `SQI_SERVER_URL` environment variable.
4. The `server_url` key in the per-user settings file, `~/.sqi/submitter.json`
   (override the path with `$SQI_SUBMITTER_SETTINGS`). The Qt dialog also
   stores the last product used per host, the save-before-submit checkbox
   state and the last ticked targets there.
5. `http://localhost:8080`.

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

Fourteen of the seventeen have a matching `sqi-submitter` host (Maya, Houdini,
Nuke, 3ds Max, Blender, Cinema 4D and After Effects). The three Mistika presets
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
chunk maps onto one invocation. All three render `.rnd` job files, whose
output format the job file sets. A movie output cannot be split across tasks,
and these presets render up to ten frames per task, so a movie longer than ten
frames needs the step's `chunks.defaultTaskCount` raised to cover the whole
range. How Mistika writes a movie from a partial `-s`/`-e` range is unverified.

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
see the output format, so nothing in the preset enforces this; the After
Effects submitter does, by locking a movie target's range and frame count
(see [After Effects](#after-effects)).

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
dialog behaves the same whether the host is in-process or a handoff. A bare
`sqi-submit` (no `--context`) has no adapter, so there the scene field stays
visible and editable.

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
submitter (the web UI, the API, a bare `sqi-submit`) nothing fills it: the presets
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

A host's render targets supply **exact-name** extras outside the general
convention (matched ignoring case and separators, but with no aliases). These
are the **target parameters**: the names that identify one target.

| Host | Target is | Target parameter (`extra` key every target carries) | Per-target products | Whole-scene products |
|---|---|---|---|---|
| Maya | a renderable, non-referenced render layer (the default layer is named `masterLayer`) | `RenderLayer` | `maya-layer-render` | `maya-scene-render` |
| Houdini | a ROP under `/out` or `/stage` | `RopPath` (the node's full path) | `houdini-rop-render` | none |
| Nuke | an enabled Write node | `WriteNode` | `nuke-write-render` | `nuke-script-render` |
| 3ds Max | none: the host offers no targets | none | none | `3dsmax-render` |
| Blender | one view layer of one scene, named `<scene> / <view layer>` | `Scene` (the **scene name**, not a path) and `ViewLayer` | `blender-view-layer-render` | `blender-batch-render` |
| Cinema 4D | a checked take other than the main take | `Take` (the take's name) | `cinema4d-take-render` | `cinema4d-render` |
| After Effects | a `Queued` render-queue item, named after its composition | `RenderQueueItem` (the item's 1-based index; an `INT` parameter, minimum 1; extra values are strings) | `after-effects-render-windows`, `after-effects-render-macos` | `after-effects-queue-render-windows`, `after-effects-queue-render-macos` |

Houdini has a per-target product only: its output target is the ROP the script
renders, not a flag that can be left out. A target's name is for display and
for job names; only the `extra` values reach the product's parameters.

After Effects targets also carry `FramesPerTask` and `Frames` on **movie**
targets only, and lock both there (see below). A key that only some targets
carry, or that any target locks, is not a target parameter.

A parameter named `Scene` always gets the scene name: extras win over
convention aliases (below), and `scene` is not a `scene_path` alias.

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

### Submitting several targets

The dialog decides from the **product** whether a scene is submitted as one job
or several, purely by parameter names, for every host:

- A product parameter is a **target parameter** when its name (ignoring case
  and `_`/`-`) matches a key present in the `extra` of **every** one of the
  host's targets and `locked` by **none** of them: `RenderLayer`, `Take`,
  `RenderQueueItem` and so on.
- A product with at least one target parameter is a **per-target product**.
  Any other product is a **whole-scene product**, whatever targets the host
  offered.
- A key that only *some* targets carry is a **per-target pre-fill value**. It
  pre-fills its parameter for that target's job, like a target's frame range,
  and never makes a product per-target. A target without it gets the form's
  value (the product default, or the artist's edit). After Effects' movie
  targets carry `FramesPerTask` and `Frames` this way.
- A key a target **locks** is a per-target pre-fill value too, even when every
  target carries it (a render queue of movies only). Locked values are never
  target parameters and never overridable, so a generic sequence product that
  merely declares `FramesPerTask` or `Frames` stays a whole-scene product.

**Whole-scene product.** The target list is hidden and a line under the product
picker says "One job for the whole scene, as configured". One job is submitted,
pre-filled from the scene (not from any target), named by the job name field.

**Per-target product.** The target picker is a checklist and the line reads "One
job per ticked target". Submit sends **one job per ticked target**, and is
disabled, with the hint "Tick at least one target.", while none is ticked.

- **Which targets start ticked:** those ticked on the last submit that created a job
  for this host and scene file (kept in `~/.sqi/submitter.json` as
  `last_targets.<host>.<scene path>`), minus any that no longer exist. With
  none remembered, only the first target.
- **Target parameters** (`RenderLayer`, `Take`, ...) come from each target's
  `extra`. With more than one target ticked they are greyed out in the form
  (tooltip "From each target"); with exactly one ticked, the artist may edit
  them, as in a single-target submission.
- **Other fields** (frame range, output path, and the rest) take each target's
  own pre-filled value **until the artist edits the field**. An edited field
  applies to the job for every ticked target. Changing the product clears the
  edits.
- **Locked values.** If a target lists a key in its `locked` array, an edit
  never overrides that target's own value for it, whether one target is ticked
  or many (tooltip "Kept per target (e.g. a movie renders as one task)"). When
  every ticked target locks the field, its editor is disabled and shows the
  first ticked target's value; when only some do, it stays editable and the
  edit applies to the others. This is generic; After Effects uses it for
  **movie protection**: a movie target's `FramesPerTask` is its whole frame
  count and its `Frames` is its own range, both `locked`, so lowering **Frames
  Per Task** or editing **Frame Range** for the sequence targets does not split
  the movie across machines. Image-sequence targets take the edit. With only
  movies ticked, both editors are disabled and show the first movie's values.
- **Job names** are `<job name field> - <target name>`, for example `Cinema 4D
  Take Render - Hero`. The field defaults to the product's title; whatever the
  artist types there is the prefix for every job.
- **Saving.** In-process hosts save once, before the first job, when **Save
  scene before submit** is ticked and the scene is modified. A handoff host has
  already saved before launching, so that box is hidden.
- **Failures do not stop the others.** Jobs are submitted one after another,
  each validated on its own; a failure in one is reported and the rest still go.
  There is no rollback, since the jobs are independent. The result dialog lists
  every target with its job ID and a link, or `FAILED` and the reason, and is
  titled "Job submitted" (one target), "Jobs submitted", "Some jobs failed" or "No jobs submitted". If the
  save fails, nothing is submitted and every target reports that.

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
a labeled text field, since a browser cannot browse the farm filesystem. Both
read the same `control` and `label` hints.

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
host token against a product's name, title and description, ignoring case,
spaces, `-` and `_` on both sides (so `cinema4d` finds "Cinema 4D" and
`aftereffects` finds `after-effects-render`). It only orders the list, hides
nothing, and does not affect pre-fill: a custom Maya product without "maya" in
those fields is listed in the **All** group and still pre-fills. The
per-target and whole-scene products for one host sit together in **Suggested**.

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

A host that cannot run the Qt dialog in its own process, or that does not run
Python, does not write an adapter at all: it uses [the handoff
contract](#the-handoff-contract). `sqi-submit --context` builds the adapter
(`ContextAdapter`) from the file.

The minimal example is `MiniAdapter` in
`clients/submitter/tests/test_adapter_contract.py`, which drives the whole flow
(product fetch → parameters → pre-fill → validate → submit) against a fake
scene and target.

**Launch-glue checklist** for a new in-process host (see [Installation per
host](#installation-per-host) for the four existing examples: Maya, Houdini,
Nuke and 3ds Max):

1. An adapter module under `hosts/<name>/adapter.py`.
2. A menu/launch entry point (`hosts/<name>/menu.py`) that calls
   `open_for_adapter(YourAdapter())`, passing the host's main window as
   `parent` where it has one.
3. A startup snippet under `hosts/<name>/startup/` showing how to wire the
   entry point into the host's own plugin/module-path mechanism.
4. A section in this doc.

For a **handoff** host, instead: one self-contained file that reads the scene
into a version 1 context file and launches `sqi-submit --context`, using only
the host's standard library. Copy the launch block that Blender and Cinema 4D
share (`hosts/_handoff_block.py`; a test keeps the copies identical), add the
file to `HOST_FILES` in `install.py` so `sqi-submit install-host` ships it,
and write a section in this doc with a manual checklist.

---

## Manual test checklist per DCC

Every host is verified by hand, since CI has no runner with licensed software.
What CI does cover is the host-independent core (form, pre-fill, multi-target
submission, the context file) and each host's scene-reading code run against a
**fake** host module (`pymxs`, `c4d`, `bpy`, ...). A fake only proves the code
does what the author *believed* the host API does. The sections below say which
beliefs are still unchecked; treat every item in a "Verified only against fakes
or documentation" list as untested until someone ticks it on a real install.

The Blender integration smoke test
(`clients/submitter/tests/integration/test_e2e.py`, run with
`SQI_TEST_BLENDER=1`) renders a frame through the `blender-batch-render`
preset on a real worker. It does not exercise the add-on.

### Every host

After installing per [Installation per host](#installation-per-host):

1. **Open scene → launch submitter.** Open (or create) a scene with a saved
   path. Trigger the sqi menu item / shelf tool / sidebar button / plug-in
   command / panel button.
2. **Dialog opens with a suggested product.** The product picker shows a
   **Suggested** group containing the host's reference presets (Maya suggests
   `maya-layer-render` and `maya-scene-render`; After Effects suggests the four
   `after-effects-*` presets) and an **All** group with every other product.
3. **Target checklist appears for a per-target product only.** Pick a per-target
   product (see the [table](#the-parameter-convention-contract)): the targets
   are listed as a checklist and the line under the picker reads "One job per
   ticked target". Pick a whole-scene product: the checklist is hidden and the
   line reads "One job for the whole scene, as configured". With nothing
   ticked, **Submit** is disabled and "Tick at least one target." shows.
4. **Pre-fill is correct.** `SceneFile` is not shown (it is taken from the open
   file). `Frames` matches the current frame range (or the ticked target's), and
   the target parameter (`RenderLayer`/`RopPath`/`WriteNode`/`Scene` and
   `ViewLayer`/`Take`/`RenderQueueItem`) matches the ticked unit.
5. **Several targets.** Tick two. The target parameters are greyed out ("From
   each target"). Submit: two jobs appear in the web UI, named `<job name> -
   <target name>`, each with its own target parameter and frame range. Edit
   **Frames** first and submit again: both jobs take the edited value.
6. **Unsaved scene.** Modify the scene without saving, then submit. An
   in-process host saves the scene before posting (checkbox **Save scene before
   submit**, on by default, remembered); a handoff host has already saved before
   launching `sqi-submit` (it has no such checkbox). A scene that has never been
   saved is refused with "Save your scene before submitting".
7. **Unreachable server.** Point `SQI_SERVER_URL` at an address nothing listens
   on, relaunch, and confirm the dialog shows a connection error banner with a
   **Reload** button, rather than crashing or hanging.
8. **Partial failure.** With two targets ticked, make one job invalid (for
   example a parameter the server rejects) and submit. The result dialog is
   titled "Some jobs failed", lists the good job's ID and the bad one as
   `FAILED` with the reason, and the good job is in the web UI.

### 3ds Max checklist

**Check by hand (3ds Max 2024, and 2025 or later):**

1. Copy the package folder's contents into `ApplicationPlugins\sqi` and restart.
   The **Submit to sqi...** macroScript exists under Customize User Interface,
   category `sqi`, with no error in the MAXScript Listener.
2. On 2025 or later: a **sqi** menu with **Submit to sqi...** appears in the
   main menu bar, and it opens the dialog. On 2024: no menu is expected;
   confirm the macroScript works from a toolbar button.
3. The dialog is parented to the Max main window (it stays above Max, and
   closing it does not close or crash Max). Open and close it twice.
4. Frame range for each **Time Output** choice in the Render Setup dialog:
   Single, Active Time Segment, Range, and Frames (e.g. `1,3,5-12`). The
   pre-filled **Frame Range** matches. A Frames value that is not plain
   `n`, `n-m` and commas (spaces, other characters) pre-fills nothing.
5. **Every Nth Frame is not read.** The adapter ignores `rendNThFrame`, so a
   scene set to every 2nd frame pre-fills every frame. Confirm, and note it in
   the job if it matters.
6. Output path: **Save File** ticked pre-fills `OutputPath` from the file name
   in Render Setup; unticked leaves it blank.
7. Save: modify, submit with **Save scene before submit** ticked; the `.max`
   on disk is current and no Max dialog interrupts. An untitled scene is
   refused.
8. Submit `3dsmax-render` and confirm a Windows worker renders it.

**Verified only against fakes or documentation:**

- `PackageContents.xml`: that Max accepts these attributes (`SchemaVersion`,
  `ProductType="Application"`, `Platform="3dsMax|3dsMaxDesign"`,
  `SeriesMin="2024"`), that the package is found in both
  `%ProgramData%\Autodesk\ApplicationPlugins` and `%APPDATA%`, and the
  `UniqueId`.
- Whether the `ComponentEntry` `ModuleName` pointing at a `.ms` under
  `Contents/scripts/startup` is what makes Max run it, or whether it only loads
  because it sits in a `startup` folder.
- Whether the menu registration really works on 2025 and later: the script
  uses `menuMan` (`registerMenuContext`, `createMenu`, `createSubMenuItem`) from
  a `#cuiRegisterMenus` callback, and nobody has seen the menu appear. The
  design assumed the 2025 `CuiMenuManager`; the script is wrapped in
  `try`/`catch` and prints "sqi: could not register the main menu" if it fails.
- That 2024 gets only the macroScript.
- The `pymxs` values the adapter reads: `rendTimeType` (1 current frame,
  2 active segment, 3 range, 4 pickup frames), the `.frame` property of
  `sliderTime`, `animationRange.start/end` and `rendStart`/`rendEnd` (whether
  it is frames or ticks, and whether it rounds), `rendPickupFrames`,
  `rendOutputFilename`, `rendSaveFile`, `classOf(renderers.current)`,
  `getSaveRequired()` and `saveMaxFile(path, quiet=True)`.
- `qtmax.GetQMaxMainWindow()` as the dialog's parent, on both PySide2 (2024)
  and PySide6 (2025+).
- That a Max-side `python.exe -m pip install` is enough (no `[qt]`).

### Cinema 4D checklist

**Check by hand (Windows and macOS):**

1. Run `sqi-submit install-host cinema4d`, restart, and confirm **Extensions →
   Submit to sqi…** exists and the plug-in console shows no registration error.
   The command has no icon.
2. An unsaved document is refused with "Save your scene before submitting".
   A changed document is saved without a dialog and the file on disk is current.
3. Frame range for each **Frame Range** choice in Render Settings: Current
   Frame, All Frames, Preview Range, Manual. The pre-filled **Frame Range**
   matches, including for a project whose frame rate is not 24.
4. Takes: check two takes, leave one unchecked, nest one under another. The
   checklist lists exactly the checked takes, not the main take, each with its
   own range and output. A take that overrides the render settings shows its
   own range; one that overrides nothing shows the main one's. Do this with the
   main take current and again with a different take current.
5. Output path: with **Save** ticked in Render Settings, `OutputPath` is
   pre-filled with `$prj`/`$take` tokens expanded; with it unticked, blank.
   Unexpanded tokens, if any survive, are passed through as-is.
6. Submit `cinema4d-take-render` for two takes: two jobs, `Take` correct on
   each. Submit `cinema4d-render`: one job.
7. `sqi-submit` not found (rename it or clear `PATH`): a Cinema 4D dialog shows
   the "sqi-submit was not found" message, and nothing hangs.
8. **The child's environment.** The `sqi-submit` window opens, the dialog loads
   products and submits. Cinema 4D passes its own environment to the process it
   starts, and `PYTHONHOME`, `PYTHONPATH` or `DYLD_*` set by the host could break
   a Python-based `sqi-submit`. Check on each OS.
9. **Window focus.** On Windows the dialog comes to the front. On macOS, check
   it is not left behind Cinema 4D.
10. On macOS, `sqi-submit` is found. A GUI app does not inherit the shell
    `PATH`, so if it is not found set `sqi_submit_command` in
    `~/.sqi/submitter.json` to its absolute path.

**Verified only against fakes or documentation:**

- The plug-in ID `1000001` is a **placeholder** (see [Cinema 4D](#cinema-4d)).
  It must be replaced with an ID registered at developers.maxon.net before
  release.
- `RegisterCommandPlugin(..., icon=None, ...)` is accepted by a real Cinema 4D
  (the registration call is only run against a fake).
- That the command appears under **Extensions**, and that a `.pyp` in the user
  `plugins` folder loads.
- `take.IsChecked()`, `IsMain()`, `GetDown()`/`GetNext()` traversal and
  `GetMainTake()`.
- `TakeData.GetEffectiveRenderData(take)[0]`: that it returns a sequence whose
  first element is the render data, and what it returns for a take that is not
  the current one (overrides may only resolve for the current take).
- The preview range from `doc.GetLoopMinTime()`/`GetLoopMaxTime()`, the
  document range from `GetMinTime()`/`GetMaxTime()`, and
  `BaseTime.GetFrame(fps)` as the frame number.
- `FilenameConvertTokens(path, {"_doc", "_rData", "_rBc", "_take"})`: the
  parameter names and whether the result is the expanded path.
- `SaveDocument(doc, path, SAVEDOCUMENTFLAGS_NONE, FORMAT_C4DEXPORT)`: whether it
  clears the document's changed flag, whether the file it writes is the one the
  farm should render, and that it shows no dialog.
- macOS: `subprocess.Popen` of an absolute path from inside Cinema 4D, and
  whether the window activates.
- The context temp file location: `<tmp>/sqi-handoff/context-<id>.json`,
  where `<tmp>` is Python's `tempfile.gettempdir()`.

### After Effects checklist

**Check by hand (Windows and macOS):**

1. Copy both files, restart, and open **Window → Submit to sqi.jsx**. The panel
   shows "sqi-submit: not set." and two buttons. Run it also from **File →
   Scripts → Run Script File** and confirm the palette works.
2. **Locate sqi-submit…** stores the path. Restart After Effects and confirm
   the panel still shows it (see "app.settings" below).
3. Each pre-check shows its message: no project open; never saved; changed (the
   save prompt, then a decline); the file/network preference off; nothing
   `Queued`; `sqi-submit` not located; the saved location deleted.
4. Queue two compositions, one rendering to an image sequence (`[#####]` in the
   file name) and one to a movie, and a third with status other than
   **Queued**. Submit: the dialog lists the two queued items by composition
   name, not the third, with `RenderQueueItem` equal to each item's position in
   the render queue. Duplicate one of the queued items (same composition, a
   second output): the duplicate is listed as `<comp> (#<queue index>)`.
5. **Frame range.** For a composition whose start time is not 0, and whose
   render queue item has a custom time span, confirm the pre-filled **Frame
   Range** is what After Effects shows (`S-E`, inclusive), and that `aerender`
   renders those same frames: the preset passes `-s`/`-e` to `aerender`, and
   whether they count from the composition's display start has not been
   confirmed.
6. **Movie protection.** Tick only the movie: **Frame Range** and **Frames Per
   Task** are greyed out and show the item's own range and frame count. Tick
   both targets, raise **Frames Per Task** to 50 and change **Frame Range**,
   submit. The sequence job takes both edits; the movie job keeps its own range
   and its whole frame count, so it is one task.
7. **Launch.** The dialog opens without After Effects freezing while it is open
   and without a console window staying on screen (Windows). Test with
   `sqi-submit` installed in a path containing spaces.
8. **Rejected paths.** A path to `sqi-submit` or the temp folder containing a
   quote, `%` (Windows) or `$`, backtick or backslash (macOS) is refused with
   the "cannot launch a program or temp file whose path contains..." message
   rather than launched.
9. **Version check.** Locate an `sqi-submit` from before `--version` existed
   (or any program that prints something else, such as `python.exe`) and
   submit: the "too old for this panel, or could not be run" message shows, no
   context file is written and nothing launches. Locate the current
   `sqi-submit` and submit again without restarting After Effects: the dialog
   opens. Submit a second time: there is no second `--version` run (no console
   flash, no delay). Note how long the first submit's check takes on each
   platform.
10. The context file appears in `<temp>/sqi-handoff/<millisecond timestamp>.json`
    and is gone once the dialog has opened.
11. **Window focus** on macOS (see below).

**Verified only against fakes or documentation:**

- **The version check.** That `system.callSystem` returns the program's
  standard output (and not its standard error) for `cmd /c ""<exe>" --version"`
  on Windows and `"<exe>" --version` on macOS, that the Windows run does not
  flash a visible console window, and how long it takes (a Python start-up,
  once per path per session). The tests run only the command strings, the
  output test and the per-path memory under Node, against a fake `callSystem`.

- **The launch recipes.** Windows: `system.callSystem('cmd /c start "" "<exe>"
  --context "<file>" --detached')`. macOS: `system.callSystem('"<exe>" --context
  "<file>" --detached >/dev/null 2>&1 &')`. Both are community practice, not documented by Adobe;
  the tests check only the string that is built, under Node. Whether
  `callSystem` returns at once with each (it normally blocks), whether the
  window gets focus and whether quoting survives paths with spaces are all
  untested.
- **The preference.** `Pref_SCRIPTING_FILE_NETWORK_SECURITY` under
  `app.preferences.getPrefAsLong` is read from two sections, `Main Pref Section
  v2` then `Main Pref Section`; which one a given After Effects version uses
  is unknown. The panel blocks only when it can read a section and every
  readable one is off; if neither is readable it carries on and lets the file
  write report the problem. The wording of the message, which names the
  **Preferences → Scripting & Expressions** option, was written from
  documentation and may not match a given version's menu.
- **`app.settings` persistence**: that `saveSetting`/`getSetting` keep the
  located path across restarts for a panel in `Scripts/ScriptUI Panels`.
- The render-queue reads: `RQItemStatus.QUEUED`, `item.comp`,
  `comp.displayStartFrame`, `item.timeSpanStart`/`timeSpanDuration`,
  `comp.frameDuration`, `item.outputModule(1).file`, `File.displayName` (decoded)
  and `File.fsName`. The arithmetic and the movie/sequence test are run under
  Node (when `node` is installed) against stubs, not against After Effects. Only
  the first output module is read.
- ExtendScript specifics: `#include "json2.js"` resolving next to the script,
  `Folder.temp`, `File.write` of UTF-8, `$.os`, the panel docking
  (`parent instanceof Panel`).
- macOS: window activation behind After Effects, and `sqi-submit` found by the
  absolute path from **Locate**.

### Blender checklist

**Check by hand (Blender 4.2 or later, and the oldest release you support; the add-on declares 3.0 as its minimum):**

1. **Install from disk.** Install `sqi_blender_addon.py` through Preferences →
   Add-ons → Install from Disk, enable **sqi Submitter**, and confirm no error
   in the console. Disable, re-enable, and restart Blender; it still registers.
2. A **sqi** tab with a **Submit to sqi** button is in the 3D viewport sidebar,
   and the add-on's preferences show **sqi-submit path**.
3. **Upgrade.** On a Blender that had the old pip-installed add-on enabled:
   follow the upgrade steps and confirm only one **sqi Submitter** is listed.
4. An unsaved scene shows "Save your scene before submitting" and does not
   start `sqi-submit`. A changed saved scene is saved, then the dialog opens.
5. Targets: a file with two scenes, one with two view layers, lists
   `<scene> / <view layer>` for every one. A view layer with **Use for
   Rendering** off is still listed. Pick `blender-view-layer-render` and tick
   two: two jobs, each with the right `Scene` and `ViewLayer`. A scene or layer
   name containing a space or quotes renders correctly.
6. `blender-batch-render` is whole-scene: no checklist, `OutputPath` pre-fills
   from the scene's output setting.
7. **sqi-submit path** set to a bad path, then cleared with `sqi-submit` on
   `PATH`: the first falls through to `PATH` or the settings file; with none
   found the "sqi-submit was not found" message shows in Blender's status bar.
8. **The child's environment.** The dialog opens and submits when started from
   Blender. Blender passes its environment to the process it starts, and
   `PYTHONHOME`/`PYTHONPATH` set by the host could break a Python-based
   `sqi-submit`. Check on each OS, in particular with a pipx install.
9. **Window focus** on Windows (should come to the front), macOS and Linux.
10. **A failed launch is shown.** Make `sqi-submit` fail to start (install it
    without the `[qt]` extra, or uninstall PySide6 from its environment), start
    Blender from the desktop menu or an icon rather than a terminal, and click
    **Submit to sqi**: an error naming the failure and the path of
    `submit-error.log` appears (a message box on Windows, a dialog on macOS).
    On Linux, try a GNOME desktop (`zenity`), a KDE one (`kdialog`) and one with
    neither: there the message comes from `xmessage` or a `notify-send`
    notification, and with none of the four installed only the log has it.
    Check X11 and Wayland sessions.

**Verified only against fakes or documentation:**

- The Linux failure message: that the `zenity`, `kdialog`, `xmessage` and
  `notify-send` command lines `sqi-submit` builds show the text as given
  (`zenity --no-markup` takes it literally), that a dialog started from a
  process Blender launched reaches the artist's desktop, and that `DISPLAY` or
  `WAYLAND_DISPLAY` is set in it.
- The preferences class is built at runtime with `type(...)` and a computed
  `__annotations__` entry, and registered with `bpy.utils.register_class`. A
  fake accepts it; real Blender may not.
- The preferences lookup `context.preferences.addons[__name__]`: that the module
  name when installed from disk is the one `bl_idname` uses.
- That a legacy add-on with `bl_info` (minimum Blender 3.0 as declared) installs
  and enables from disk on Blender 4.2 and later, and on 3.x.
- `bpy.data.filepath`, `bpy.data.is_dirty`, `bpy.ops.wm.save_mainfile()` called
  from an operator, `bpy.data.scenes[].view_layers`, `scene.render.filepath` and
  `render.engine`, `bpy.app.version_string`.
- The context file location `<tmp>/sqi-handoff`, and that
  `AllowSetForegroundWindow(ASFW_ANY)` (Windows) brings the window forward.

---

## The handoff contract

A host that cannot run the Qt dialog in its own process, such as Blender and
Cinema 4D (which bundle no Qt and no `sqi_submitter`) or After Effects (scripted
in ExtendScript), does not need an adapter or any sqi library. It does three
things:

1. Saves the open scene.
2. Writes a **context file**, a small JSON document describing the scene and its
   render targets.
3. Starts `sqi-submit --context FILE` as a separate, detached process.

`sqi-submit` reads the file into a `ContextAdapter` (an ordinary `HostAdapter`
backed by the file) and opens the same dialog an in-process host opens. This is
the recommended way to add any application. A studio that wants its own UI
instead can use [the three REST calls](#the-rest-calls) directly.

### The context file, version 1

| Field | Type | Meaning |
|---|---|---|
| `version` | `1` | Required. Any other value is refused. |
| `host.name` | string | Lowercase token for the host (`blender`, `cinema4d`, `aftereffects`). Drives the **Suggested** product group and the per-host remembered settings (`last_product.<name>`, `last_targets.<name>.<scene path>`). Matching ignores case, spaces, `-` and `_`. |
| `host.display_name` | string | Human name for the host. |
| `host.version` | string or `null` | The host's version. Informational. |
| `scene.path` | string | Required, non-empty. The saved scene file; the farm renders this file on disk. |
| `scene.frame_range` | string or `null` | The scene's frame range in the `Frames` convention: `"1-100"`, or `"5"` for one frame. |
| `scene.output_path` | string or `null` | The scene's output path. |
| `scene.renderer` | string or `null` | The scene's renderer, for a `Renderer` parameter. |
| `targets` | array | Optional, default empty. The render targets. |
| `targets[].name` | string | Required. Shown in the checklist and used in job names. |
| `targets[].kind` | string | Required. A free label such as `take` or `render_queue_item`; informational. |
| `targets[].frame_range` | string or `null` | Replaces the scene's range for this target's job. |
| `targets[].output_path` | string or `null` | Replaces the scene's output path for this target's job. |
| `targets[].extra` | object of strings | The target parameters (`Take`, `RenderQueueItem`, ...) and any per-target pre-fill values. Every value **must be a string**, so an `INT` parameter such as `RenderQueueItem` is sent as `"2"`. |
| `targets[].locked` | array of strings | Optional, default empty. Keys of `extra` the artist cannot override for this target. Each must be a key of that target's `extra`. |
| `server_url` | string or `null` | Used ahead of the environment and settings file; `null` or absent means normal resolution (see [Server URL configuration](#server-url-configuration)). An explicit `sqi-submit --server` wins over it. |

Everything is name-for-name what the in-process adapters produce:
`SceneContext`, a list of `RenderTarget`, and the host's `host_name` and
`display_name`. How `targets[].extra` and `locked` drive the form is described
in [Submitting several targets](#submitting-several-targets).

- **Unknown keys are ignored**, at every level, so a newer host can add fields.
- **Target names must be unique within a context**, because job names, remembered
  ticks and results are keyed on them. When a host sends duplicates, the dialog
  keeps the first and renames each later one `<name> (#<position>)`, its 1-based
  position in `targets`. Names that are already unique are untouched.
- **Saving happens in the host, before launch.** The dialog reports the scene as
  unmodified and hides its **Save scene before submit** box.
- **A bad file never gives a traceback.** An unreadable file, invalid JSON, an
  unsupported `version`, a missing `scene.path` or any wrongly typed field makes
  the dialog open with a banner that names the file and the problem, for example
  `The submit context file context-3f2a.json is invalid: scene.path is missing
  or empty`, or `... is invalid: unsupported version 2 (this sqi-submit reads
  version 1)`. The dialog then behaves like a bare `sqi-submit`: no host, no
  targets.
- **The file is deleted after a successful read** (best effort; a file that
  cannot be deleted does not stop the submit). A file that fails to read or
  validate is left in place for debugging. Files for a launch that never
  happens, or that `sqi-submit` never reads, are not cleaned up: the shipped hosts
  write them to `<temp folder>/sqi-handoff/` (Python's `tempfile.gettempdir()`;
  After Effects' `Folder.temp`).
- Blender and Cinema 4D keep the file private to the artist, because on Linux
  the temp folder is the shared `/tmp` and the file names the server the API key
  is sent to. The folder is created `0700` (an existing one this user owns is
  tightened to `0700`), and each file is created exclusively, `0600`. If
  `sqi-handoff` is a symlink, not a directory, or owned by another user, they
  write to a fresh private `<temp folder>/sqi-handoff-<random>/` instead.
- A UTF-8 byte order mark at the start of the file is accepted.

The JSON Schema for version 1 is
[`clients/submitter/src/sqi_submitter/core/context.schema.json`](https://github.com/uberware/sqi/blob/main/clients/submitter/src/sqi_submitter/core/context.schema.json)
(installed as `sqi_submitter/core/context.schema.json`). The schema cannot
express "each `locked` key is a key of `extra`"; the reader checks that.

### How `sqi-submit` is found

Each handoff host resolves the command in this order and uses the first that
names a file that exists:

1. A path the artist set in the host: Blender's add-on preference
   **sqi-submit path**; After Effects' **Locate sqi-submit…** button. Cinema 4D
   has no such setting.
2. `sqi_submit_command` in `~/.sqi/submitter.json` (override the file with
   `$SQI_SUBMITTER_SETTINGS`). Blender and Cinema 4D read it; After Effects does
   not.
3. `sqi-submit` on the host's `PATH`. Blender and Cinema 4D only. This is
   unreliable inside macOS GUI applications, which do not inherit the shell's
   `PATH`; on macOS set it in step 1 or 2.

A configured path that does not exist is skipped without complaint. If nothing
is found, Blender reports (in its status bar) and Cinema 4D shows (in a dialog):

```
sqi-submit was not found. Install it on this machine with: pip install "sqi-submitter[qt]" — then set its full path in the sqi add-on/plugin settings, or as sqi_submit_command in ~/.sqi/submitter.json.
```

After Effects shows its own message instead, since it only knows the located
path: `sqi-submit was not found. Install it with pip install "sqi-submitter[qt]"
(or pipx install "sqi-submitter[qt]"), then click "Locate sqi-submit…" in this
panel and choose the sqi-submit program.` Both messages double-quote the package
name, since `cmd.exe` passes single quotes through literally.

### Launching it

On Windows the Python hosts start `sqi-submit` detached (`DETACHED_PROCESS` and
`CREATE_NEW_PROCESS_GROUP`) and then call `AllowSetForegroundWindow` with
`ASFW_ANY` so its window can come to the front. Not the child's process ID: the
`sqi-submit.exe` that pip, pipx and uv install is a launcher that starts the real
`python.exe` as its own child, so the window lives in a grandchild that a
per-process grant never reaches. On macOS and Linux they start it in a new
session. After Effects uses `system.callSystem` with
`cmd /c start "" "<exe>" --context "<file>" --detached` on Windows and
`"<exe>" --context "<file>" --detached >/dev/null 2>&1 &` on macOS, and refuses
a path that contains a character it cannot quote. `--detached` is a hidden flag
for this launch (see below); the Python hosts do not pass it. Because an older
`sqi-submit` rejects that flag, After Effects first checks `sqi-submit
--version` (see [After Effects](#after-effects)). The child inherits the host's environment. The
dialog calls `show()`, `raise_()` and `activateWindow()`. There is no macOS app
bundle for `sqi-submit`, so on macOS the window may open behind the host; the
[checklists](#manual-test-checklist-per-dcc) record that as unverified.

### When `sqi-submit` fails to start

A detached `sqi-submit` has no console, so an error printed to stderr reaches
nobody. When it cannot open the dialog, most often because no Qt binding is
installed (`pip install "sqi-submitter[qt]"`), or it stops on an unexpected
error, it still prints the message to stderr, and also:

- appends a timestamped entry to `submit-error.log` in the settings directory,
  `~/.sqi/` (the directory of `SQI_SUBMITTER_SETTINGS` when that is set). The
  entry holds only the message, plus the traceback for an unexpected error. The
  settings file and the context file are never read into the log, but an
  unexpected exception's own message and traceback are logged as they are.
  Past 64 KiB the log is cut to its last 16 KiB before the entry is added.
- when stderr is not a terminal, or the launch passed `--detached`, shows the
  message and the log's path in a native error box: a message box on Windows,
  an `osascript` dialog on macOS. On Linux, when `DISPLAY` or `WAYLAND_DISPLAY`
  is set, it runs the first of these found on `PATH`: `zenity --error
  --no-markup`, `kdialog --error`, `xmessage -center`, then `notify-send -u
  critical` (a notification rather than a dialog), passing the text as a single
  argument with no shell, and waits for it to close. With no display, or none of
  the four installed, Linux only logs, as do other platforms.

It exits 1 either way. Every handoff host (Blender, Cinema 4D, After Effects)
launches `sqi-submit` detached and relies on this to report a failed launch.
Blender and Cinema 4D start it with no stderr on Windows and with the host's
own stderr on macOS and Linux, so a Blender started from a terminal gets the
message in that terminal, and one started from a desktop icon or menu (whose
stderr is not a terminal) gets the native box. After Effects passes the
hidden `--detached` flag, because `cmd /c start` gives `sqi-submit` a console of
its own (a real terminal) that closes when it exits. `--detached` is that
host-launch flag, not an option for users. Run from a terminal, `sqi-submit`
prints only; a non-interactive caller with stderr redirected (a script, CI)
gets the modal box before it exits 1. A bad context file is not a
startup failure: the dialog opens with its banner as described above. The
native box, the macOS dialog and the Linux tools are tested only against fakes;
whether they come to the front over the host is unverified.

### A worked example

A Cinema 4D scene with two checked takes, `Hero` and `Hero_closeup`. This file
is valid: it loads with `sqi_submitter.core.handoff.load_context` and passes the
schema above.

```json
{
  "version": 1,
  "host": { "name": "cinema4d", "display_name": "Cinema 4D", "version": "2025" },
  "scene": {
    "path": "D:/projects/spot/spot_v012.c4d",
    "frame_range": "1-120",
    "output_path": "D:/renders/spot/beauty_",
    "renderer": null
  },
  "targets": [
    {
      "name": "Hero",
      "kind": "take",
      "frame_range": "1-120",
      "output_path": "D:/renders/spot/hero_",
      "extra": { "Take": "Hero" },
      "locked": []
    },
    {
      "name": "Hero_closeup",
      "kind": "take",
      "frame_range": "40-80",
      "output_path": null,
      "extra": { "Take": "Hero_closeup" },
      "locked": []
    }
  ],
  "server_url": null
}
```

The host saved to `D:/projects/spot/spot_v012.c4d`, wrote this to
`<temp>/sqi-handoff/context-<id>.json`, and ran:

```sh
sqi-submit --context "<temp>/sqi-handoff/context-<id>.json"
```

In the dialog, `Cinema 4D Take Render` (which has a `Take` parameter, so it is
per-target) shows a checklist of `Hero` and `Hero_closeup`, and `Cinema 4D
Render` (no `Take`) shows none and submits one whole-scene job. Ticking both
and submitting creates `Cinema 4D Take Render - Hero` with `Take=Hero` and
`Frames=1-120`, and `Cinema 4D Take Render - Hero_closeup` with
`Take=Hero_closeup` and `Frames=40-80`. `Hero_closeup` has no output path of its
own, so its `OutputPath` is the scene's, `D:/renders/spot/beauty_`.

A locked per-target value looks like an After Effects movie target as the
panel writes it (a smaller one is in the test fixture
`clients/submitter/tests/fixtures/context/multi-target-locked.json`):

```json
{ "name": "Main Comp", "kind": "render_queue_item", "frame_range": "1-240",
  "output_path": "D:\\out\\spot.mov",
  "extra": { "RenderQueueItem": "1", "FramesPerTask": "240", "Frames": "1-240" },
  "locked": ["FramesPerTask", "Frames"] }
```

### Adding another host

1. Read the scene and its targets in the host's own scripting language. Choose
   `extra` key names that match a product's parameter names; the targets only
   become a checklist when a key is present on **every** target.
2. Write the file as above, with `host.name` a short lowercase token, and save
   the scene first.
3. Resolve and launch `sqi-submit --context FILE` as described. For a Python
   host, copy the block between the `sqi handoff` markers in
   `sqi_submitter/hosts/_handoff_block.py`; it is standard library only and
   Python 3.9 compatible, and a test keeps the copies in Blender and Cinema 4D
   identical to it.
4. Do not wait for the process. The dialog is the artist's from here on.

### The REST calls

The handoff file is for adding an application. A studio that wants its own UI
needs no sqi code either: the submission contract is three REST calls against
the same server every other client uses.

| Call | Purpose |
|---|---|
| `GET /api/v1/products` | List the product catalog (name, title, description, category). |
| `GET /api/v1/products/{name}/parameters` | Fetch the chosen product's parsed parameter schema, including `userInterface` hints, to build a form. |
| `POST /api/v1/products/{name}/jobs` | Submit a job: body `{ "farm_id", "queue_id", "name", "parameters": { ... }, plus optional "owner", "submitter", "priority", "project", "depends_on", "max_attempts", "retry_delay_seconds", "failure_limit" }`. |

`internal/api/openapi.yaml` is the authoritative wire contract for all three;
where it and this doc disagree, it wins. The [parameter convention
contract](#the-parameter-convention-contract) applies unchanged: a submitter
that pre-fills from its host matches the same parameter names, with the same
case- and separator-insensitive rules and the same additive-only guarantee.
Building one needs only an HTTP client.

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
