# DCC submitters

`sqi-submitter` is a Python package that adds in-application job submission to
Maya, Houdini, Nuke, and Blender, on top of [`sqi-sdk`](python-client.md). It
ships as its own PyPI package — `pip install sqi-submitter` — separate from
`sqi-sdk` because it depends on a DCC host or a Qt binding to do anything
useful, while `sqi-sdk` stays a zero-UI scripting library.

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

`core` is importable in a plain `python -c "import sqi_submitter.core"` with
no Qt and no DCC installed — it holds the server session (`SubmitterSession`),
the parameter form model (`FormModel`, mirroring the web submission form's
widget/validation rules), scene-context pre-fill (`prefill`), and the
`HostAdapter` extensibility contract. `qt` builds a generic submit dialog from
that same core model, used by Maya, Houdini, and Nuke (all of which embed a
Qt-compatible binding). `hosts/blender` is the exception: Blender ships no Qt,
so its UI is a native `bpy` N-panel bound directly to the same `FormModel` —
proof that the core is genuinely UI-agnostic, not just Qt with an adapter
layer.

The framework is **product-driven**: a submitter never bakes in "Maya has
these fields." It fetches the sqi product catalog (`GET /api/v1/products`),
lets the artist pick one, fetches its parameters (`GET
/api/v1/products/{name}/parameters`), and renders a form from that schema —
exactly what the web `/submit` page does. Everything DCC-specific is
scene/context extraction (a scene path, a frame range, which render target is
selected) that pre-fills matching parameters by **name**, never by which
product was chosen. See [Reference presets](#reference-presets) for the four
products this repo ships to give the framework something real to target, and
[The parameter convention contract](#the-parameter-convention-contract) for
exactly how pre-fill works.

---

## Installation per host

Install `sqi-submitter` into the Python the DCC actually uses — its bundled
interpreter, or a shared site directory the DCC is configured to see. It
depends only on `sqi-sdk`; no Qt install is ever required inside a DCC (Maya,
Houdini, and Nuke bundle a PySide binding already; Blender's UI needs no Qt at
all).

```sh
pip install sqi-submitter
```

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

`evalDeferred` waits until Maya's UI is ready before installing the menu, so
this is safe from `userSetup.py`. Restart Maya; a **sqi** menu appears.

> **For a submitted job to actually run,** the worker that renders it needs the
> `maya=true` capability tag and Maya's `Render` binary on its `PATH` —
> otherwise the job sits `ready` forever with no worker eligible to take it. See
> [Reference presets](#reference-presets).

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

> **For a submitted job to actually run,** the worker that renders it needs the
> `houdini=true` capability tag and `hython` on its `PATH` — otherwise the job
> sits `ready` forever with no worker eligible to take it. See
> [Reference presets](#reference-presets).

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

Nuke auto-executes `menu.py` files on its path at startup; a **sqi** menu
appears once Nuke restarts.

> **For a submitted job to actually run,** the worker that renders it needs the
> `nuke=true` capability tag and `nuke` on its `PATH` — otherwise the job sits
> `ready` forever with no worker eligible to take it. See
> [Reference presets](#reference-presets).

### Blender

```sh
/path/to/blender/python/bin/python3.11 -m pip install sqi-submitter
```

Blender's add-on system wants a single add-on file, not a `pip install`ed
package on its own — so after installing the package into Blender's Python,
enable the add-on that lives inside it:
`sqi_submitter/hosts/blender/addon.py`. In Blender: **Edit → Preferences →
Add-ons → Install…**, point it at that file (or copy it into your Blender
scripts/add-ons directory and enable it from the list), then enable **"sqi
Submitter"**.

A **sqi** tab appears in the 3D viewport's N-panel (sidebar). It's a
self-contained flow:

1. Click **Refresh Products** — fetches the product list and, for the
   currently selected product/target, its parameters (this can take a moment;
   it runs off the main thread so Blender's UI never blocks).
2. Pick a **Product** and, if it has renderable units, a **Target** (scene ×
   view layer).
3. Edit the generated parameter fields, job name, farm, and queue.
4. Click **Submit**.

**Changing the product or target does not automatically refetch parameters —
click Refresh Products again** after changing either dropdown, or the form
still shows the previous selection's fields.

> **For a submitted job to actually run,** the worker that renders it needs the
> `blender=true` capability tag and `blender` on its `PATH` — otherwise the job
> sits `ready` forever with no worker eligible to take it. See
> [Reference presets](#reference-presets).

### Standalone (`sqi-submit`)

For QA, testing outside a DCC, or any host not listed above, install the `qt`
extra to pull in PySide6 and get a console entry point:

```sh
pip install 'sqi-submitter[qt]'
sqi-submit                          # opens the dialog against the configured server
sqi-submit --server http://farm.example:8080
```

`sqi-submit` needs *some* Qt binding present — PySide6 (via `[qt]`) or a
PySide2 already on the interpreter's path (e.g. running it with a DCC's own
Python). With neither, it raises a clear error naming the remedy rather than
an import traceback.

### Server URL configuration

Every submitter resolves the server URL the same way, in order:

1. An explicit `server_url` argument (`sqi-submit --server ...`, or
   `open_for_adapter(adapter, server_url=...)` from host glue).
2. The `SQI_SERVER_URL` environment variable.
3. The `server_url` key in a small per-user settings file, `~/.sqi/submitter.json`
   (override the path with `$SQI_SUBMITTER_SETTINGS`). This file also
   remembers the last product used per host and the save-before-submit
   checkbox state.
4. `http://localhost:8080`.

---

## Reference presets

C1 ships four products under `presets/dcc/`, published through the [preset
library](preset-library.md) so `sqi-submitter` has something real to submit
to out of the box. Install them from **Admin → Preset Library** in the web
UI, or by hand via `POST /api/v1/products` (or **Admin → Products** in the
web UI) if you're not using a preset index. All four use the product
`category` `Rendering` and declare `TASK_CHUNKING` for frame distribution.

| Product | Command | Chunking | Parameters | Worker tag |
|---|---|---|---|---|
| `maya-batch-render` | `Render -r <renderer> -s/-e <frame> -rl <layer> -rd <dir> <scene>` | one frame per task | `SceneFile`, `Frames`, `OutputDir`, `Renderer` (default `file`), `RenderLayer` (default `masterLayer`) | `attr.worker.tag.maya = "true"` |
| `houdini-rop-render` | `hython` running an embedded chunk script that loads the hip file and calls `rop.render()` per frame range | chunks of 10 | `SceneFile`, `Frames`, `RopPath` | `attr.worker.tag.houdini = "true"` |
| `nuke-write-render` | `nuke -x -X <writeNode> -F <range> <script>` | chunks of 10 | `SceneFile`, `Frames`, `WriteNode` | `attr.worker.tag.nuke = "true"` |
| `blender-batch-render` | `blender -b <file> -o <output> -f <frame>` | one frame per task | `SceneFile`, `Frames`, `OutputPath` (optional; blank = scene setting) | `attr.worker.tag.blender = "true"` |

Maya and Blender are one-frame-per-task because both commands only take a
single frame at a time (`Render -s/-e` bounds one call; Blender's `-f` renders
one frame per invocation) — chunking them further would just mean re-running
the same single-frame render inside a bigger task for no benefit. Houdini and
Nuke get real multi-frame chunks (10 by default) because both the embedded
`hython` script and `nuke -F` natively accept a frame range per invocation, so
a chunk amortizes DCC startup cost across several frames.

Each preset's `hostRequirements` gates it to workers advertising the matching
`attr.worker.tag.<name>` capability tag — these are **operator-configured**,
not auto-detected. Add a `key=value` entry to the worker's
`capability_tags` (see [worker capability tags](worker-capabilities.md)),
on top of having the corresponding DCC binary on that worker's `PATH`:

```yaml
worker:
  capability_tags:
    - maya=true
```

(substitute `houdini=true`, `nuke=true`, or `blender=true` for the other
three presets). This satisfies the preset's `attr.worker.tag.maya` (etc.)
requirement. A job submitted against one of these products sits `ready`
forever if no worker advertises the tag.

---

## The parameter convention contract

Pre-fill (the "from scene" values an adapter supplies) binds to a chosen
product's parameters **purely by name**, matched case- and
separator-insensitively (`SceneFile`, `scene_file`, and `Scene-File` are all
the same key) — never by which product was picked. This is what lets a
studio duplicate a reference preset, rename it, rewrite the whole command, and
keep working submitters as long as the parameter names below are kept.

| Convention key | Matches parameter names | Source |
|---|---|---|
| `scene_path` | `SceneFile`, `ScenePath` | `SceneContext.scene_path` |
| `frame_range` | `Frames`, `FrameRange` | `SceneContext.frame_range`, overridden by the selected render target's range |
| `output_path` | `OutputDir`, `OutputPath` | `SceneContext.output_path`, overridden by the selected render target's output path |
| `renderer` | `Renderer` | `SceneContext.renderer` |

### The scene file is host-managed

Inside a DCC (whenever a `HostAdapter` is present), the scene-path parameter
(`SceneFile`/`ScenePath`) is **not shown in the submit form**. It is always
taken from the currently open file at submit time, and submitting fails with
"Save your scene before submitting" if the file has never been saved — the
farm renders the file on disk, so an unsaved scene has nothing to render. This
is enforced once in `core/submit.py::submit_form`, so every host (the Qt dialog
and the Blender panel alike) behaves identically. The standalone `sqi-submit`
dialog has no adapter, so there the scene field stays visible and editable.

Shared parameter labels are standardized across the reference presets so the
hosts read identically: `SceneFile` → "Scene File", `Frames` → "Frame Range",
Maya's `OutputDir` → "Output Directory", Blender's `OutputPath` → "Output Path".
Labels live in the product template (`userInterface.label`), so a product
created locally must be re-created from the updated preset to pick up a label
change.

**Blender output is optional.** `blender-batch-render`'s `OutputPath` is passed
straight to Blender's `-o` and defaults to blank. Left blank, `submit_form`
fills it from the scene's own output setting (`SceneContext.output_path`), so a
default submission renders exactly like a local render — Blender's own frame
numbering and file naming apply. Set it to override the destination (e.g. to
shared storage). This differs from Maya's `OutputDir`, which is a real output
*directory* (`Render -rd`); Blender's `-o` is a full path prefix, not a folder.

**`PATH` is type-first.** OpenJD has no file/directory *picker* control, and the
base spec requires a `control` whenever a `userInterface` (hence a `label`) is
set — so a labeled path parameter must declare `control: LINE_EDIT`. The
submitter clients treat `PATH` as type-first: a `LINE_EDIT` (or absent) control
does **not** suppress the picker. The Qt dialog derives the picker
(`CHOOSE_DIRECTORY` / `CHOOSE_OUTPUT_FILE` / `CHOOSE_INPUT_FILE`, from
`objectType`/`dataFlow`) and honors the label, so a labeled path shows a browse
button *and* its label. An explicit `HIDDEN` still wins. Templates stay valid
OpenJD; the picker is a client rendering affordance never represented in the
document. (The web UI renders paths as a labeled text field — a browser cannot
browse the farm filesystem — and the Blender panel uses a text property.)

A selected render target can also supply **exact-name** extras that aren't
part of the general convention (still case/separator-insensitive, but not
aliased to anything else): `RenderLayer` (Maya render layer), `RopPath`
(Houdini ROP path), `WriteNode` (Nuke Write node). Blender's target picker is
scene × view-layer, and each target's extras include `Scene` (the Blender
**scene name**, not a path — see `hosts/blender/adapter.py`) and `ViewLayer`.
The shipped `blender-batch-render` preset doesn't declare `Scene`/`ViewLayer`
parameters yet — selecting a target there only drives the
`frame_range`/`output_path` override — but a fork that adds a `Scene`
parameter gets the scene name unambiguously: extras always win over
convention aliases (below), and `scene` is deliberately not a `scene_path`
alias for exactly this reason.

Rules that make this a stable contract:

- **Additive-only.** New convention keys or aliases may be added; existing
  ones are never renamed or removed. A form field that doesn't match any
  known key or extra simply renders unfilled — it's never an error.
- **Extras win over convention aliases.** If a selected render target's
  `extra` map has a key matching a parameter's name, that value is used even
  if the same parameter name would otherwise match a convention alias.
- **Never keyed on product identity.** The mapping code has no notion of
  "this is the Maya preset" — it only ever asks "does this parameter's name
  match `scene_path`'s aliases?". A completely custom, differently-named
  product that happens to declare a parameter called `SceneFile` gets the
  same pre-fill as the reference preset.
- **Target overrides scene.** If a render target is selected and it declares
  its own frame range or output path, that value wins over the scene-level
  one for `frame_range`/`output_path`.

### PATH parameters and the client chooser fallback

This repo's OpenJD `userInterface` control enum (see
[`docs/openjd-extensions.md`](openjd-extensions.md) and
[`docs/products.md`](products.md#userinterface-parameter-hints)) has **no
file-chooser control** — the valid values are `LINE_EDIT`, `MULTILINE_EDIT`,
`DROPDOWN_LIST`, `CHECK_BOX`, `CHIP_INPUT`, `HIDDEN`, and `SPIN_BOX`. There is
no `CHOOSE_INPUT_FILE`/`CHOOSE_OUTPUT_FILE`/`CHOOSE_DIRECTORY` a template
author can declare, and a `userInterface` block that is present must carry a
`control` (the server rejects one without it).

Chooser widgets instead come from a **client-side type fallback**, and it
only exists in the Qt dialog: for a `PATH` parameter with **no**
`userInterface` block at all, the Qt dialog renders a text field plus a
browse button opening the appropriate chooser (`objectType: DIRECTORY` →
folder picker, `dataFlow: OUT` → save dialog, otherwise an open-file dialog).
The web submission form has no equivalent — `selectWidget` in
`web/src/lib/productForm.ts` falls through to a plain text input for a
`PATH` parameter with no `control` hint, same as any other unrecognized type.
An explicit `control` always wins over the Qt type fallback — **hinting a
`PATH` parameter as `LINE_EDIT` forces a plain text field with no browse
button, in both Qt and web**.

The rule for preset authors: **leave `PATH` parameters without a
`userInterface` hint** so the Qt dialog applies its chooser fallback (the web
form renders a plain text field either way). The four reference presets do
exactly this — their `PATH` parameters (`SceneFile` everywhere; `OutputDir`
in the Maya and Blender presets) carry no `userInterface` block, so Qt-hosted
artists get real file/directory pickers. The tradeoff is the field label
falls back to the raw parameter name (e.g. "SceneFile"), since a label can't
currently be set without also forcing a `control`. Non-`PATH` parameters keep
`LINE_EDIT` hints for friendly labels.

---

## Customizing for your pipeline

The reference presets are explicitly named to leave room for variants
(`maya-batch-render`, not "the Maya product") and are described in their own
`description` field as a starting point. To adapt one:

1. Open it in **Admin → Products**, or fetch it via
   `GET /api/v1/products/{name}` and re-`POST` under a new name.
2. Use **Duplicate to custom** (see [`docs/preset-library.md`](preset-library.md#installed-product-lifecycle))
   if you installed it from the library — this gives you a fully mutable
   copy that's no longer tied to future library updates.
3. Change anything inside the template: environment setup, licensing
   commands, added parameters, a completely different render command. Nothing
   about the product wrapper (name, title, category) constrains what the
   OpenJD template underneath does.
4. **Keep the convention parameter names** (`SceneFile`, `Frames`, `OutputDir`,
   `Renderer`, and any target extras you rely on) if you want `sqi-submitter`'s
   pre-fill to keep working. Renaming a parameter to something outside the
   convention table just means that field won't pre-fill — it still works,
   the artist just has to type it in.
5. Keep the `category` set to `Rendering` (or whatever your catalog convention
   is) — category is a relabelable grouping the catalog/preset UIs filter and
   display on; it isn't identity, so there's no reason forks need to diverge
   from it.

Note that the submit dialog's **"Suggested"** product grouping (see
[Overview](#overview)) is a cosmetic substring match on the host token against
a product's name/title/description — it never hides anything, and it has no
bearing on pre-fill. A renamed, fully custom Maya product that doesn't happen
to contain "maya" in its name/title/description just doesn't get promoted to
the Suggested group; it's still fully submittable and still pre-fills
normally.

---

## Writing a new adapter

A fifth Python-based tool plugs into the framework by subclassing
`sqi_submitter.core.HostAdapter` — the entire extensibility contract:

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

`scene_context()` and `render_targets()` are the only abstract methods.
Everything else — fetching the product catalog, building the parameter form,
running the pre-fill convention, validating, and submitting — is shared core
code your adapter never touches.

Then open the generic Qt dialog against your adapter:

```python
from sqi_submitter.qt.dialog import open_for_adapter

open_for_adapter(MyToolAdapter())
```

Or, for a native (non-Qt) host UI, drive `sqi_submitter.core` directly —
`SubmitterSession`, `FormModel.from_parameters(...)`, `prefill(...)`, and
`submit_form(...)` — the way `sqi_submitter/hosts/blender/addon.py` does over
`bpy`.

The canonical, minimal example is `MiniAdapter` in
`clients/submitter/tests/test_adapter_contract.py` — it drives the whole flow
(product fetch → parameters → pre-fill → validate → submit) with a fake
two-line scene/target implementation, and is the executable proof of the
extensibility contract.

**Launch-glue checklist** for a new host (see [Installation per host](#installation-per-host)
for the four existing examples):

1. An adapter module under `hosts/<name>/adapter.py`.
2. A menu/launch entry point (`hosts/<name>/menu.py`) that calls
   `open_for_adapter(YourAdapter())` for Qt-embedding hosts, or a native panel
   for hosts without Qt.
3. A startup snippet under `hosts/<name>/startup/` showing how to wire the
   entry point into the host's own plugin/module-path mechanism.
4. A section in this doc.

---

## Manual test checklist per DCC

Real-DCC verification (Maya, Houdini, Nuke) is manual — there's no CI runner
for licensed software. Blender is covered by an automated integration smoke
test (see `clients/submitter/tests/integration/test_e2e.py`,
`SQI_TEST_BLENDER=1`) since it's freely installable, but should still be spot
-checked manually after any change to its adapter/add-on.

For each host, after installing per [Installation per host](#installation-per-host):

1. **Open scene → launch submitter.** Open (or create) a scene with a saved
   path. Trigger the sqi menu item / shelf tool / N-panel.
2. **Dialog opens with a suggested product.** The product picker shows a
   **Suggested** group containing the host's reference preset (e.g. Maya
   shows `maya-batch-render` suggested) and an **All** group with every other
   product.
3. **Target picker lists render units.** If the scene has renderable units
   (Maya render layers, Houdini ROPs, Nuke Write nodes, Blender view layers),
   the target picker is visible and lists them; if there are none, it's
   hidden.
4. **Pre-fill is correct.** `SceneFile` matches the open scene's path,
   `Frames` matches the current frame range (or the selected target's range),
   and any target extra (`RenderLayer`/`RopPath`/`WriteNode`) matches the
   selected unit. Selecting a different target re-applies pre-fill.
5. **Submit → job appears in the web UI.** Pick a farm/queue, submit, and
   confirm the success dialog's job URL opens the job in the web UI with the
   parameters you expect.
6. **Unsaved-scene save prompt.** Modify the scene without saving, submit.
   The submitter saves the scene automatically (checkbox "Save scene before
   submit", on by default and remembered) before posting the job. If the
   scene has never been saved (no path to save to), submit refuses with a
   message asking you to save manually first.
7. **Unreachable-server banner.** Point `SQI_SERVER_URL` (or the standalone
   `--server` flag) at an address nothing is listening on, relaunch, and
   confirm the dialog shows a connection error banner with a **Reload**
   button instead of crashing or hanging.

---

## Language-neutral submission contract (for future non-Python submitters)

`sqi-submitter` is Python/Qt, which rules out apps that can't host a Python
process in-process — After Effects (ExtendScript/CEP/UXP JavaScript) being the
motivating example (see the tracker's **C3** row in
`docs/superpowers/specs/phase-2-tracker.md`). Nothing about the submission
contract requires Python, though: it's three REST calls against the same
server every other client uses.

| Call | Purpose |
|---|---|
| `GET /api/v1/products` | List the product catalog (name, title, description, category). |
| `GET /api/v1/products/{name}/parameters` | Fetch the chosen product's parsed parameter schema, including `userInterface` hints, to build a form. |
| `POST /api/v1/products/{name}/jobs` | Submit a job: body `{ "farm_id", "queue_id", "name", "parameters": { "<ParamName>": "<value>", ... } }`. |

`internal/api/openapi.yaml` is the authoritative wire contract for all three —
treat it, not this doc, as the source of truth if they ever disagree. The same
[parameter convention contract](#the-parameter-convention-contract) above
applies: a JS submitter that wants "pre-fill from the host app" pre-fills by
matching the same parameter names, with the same case/separator-insensitive
rules and the same additive-only guarantee. No Qt, no Python, and none of
`sqi-submitter`'s packaging is required to build a fully-functional submitter
against this contract — only an HTTP client.

---

## See also

- [`docs/python-client.md`](python-client.md) — `sqi-sdk`, the scripting
  library `sqi-submitter` is built on.
- [`docs/products.md`](products.md) — the product/preset catalog model and
  `userInterface` parameter hints.
- [`docs/preset-library.md`](preset-library.md) — installing the reference
  presets (or any preset) from a hosted index.
- [`docs/worker-capabilities.md`](worker-capabilities.md) — configuring the
  `maya`/`houdini`/`nuke`/`blender` capability tags the reference presets gate
  on.
- [`docs/development.md`](development.md#the-sqi-submitter-package) — the
  package's dev gate and layout.
