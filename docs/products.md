# Products

A **product** is a named, versioned wrapper around an OpenJD template, stored
unmodified apart from YAML re-serialization when it comes from a definition
file. It gives the template a stable identity (`name`), human-readable metadata
(`title`, `description`, `category`, `version`), and a home in the catalog so
clients can list and submit jobs without ever handling a raw template file.

Parameters, their `userInterface` hints (control, label, group), machine
requirements, and step-level setup and teardown all live **inside** the OpenJD
template. The product adds only the catalog metadata around it.

---

## Concept

```
              ┌────────────────────────────────────────────┐
              │                  Product                   │
              │   name · title · description · category    │
              │   version · source                         │
              │   ┌──────────────────────────────────────┐ │
              │   │           OpenJD template            │ │
              │   │  parameters · steps · requirements   │ │
              │   └──────────────────────────────────────┘ │
              └────────────────────────────────────────────┘
```

When a job is submitted via a product (`POST /api/v1/products/{name}/jobs`), the
server loads the product's stored template and hands it to the same
`openjd.Submitter` pipeline as a raw `POST /api/v1/jobs`. The job row stores a
**snapshot** of the template at submission time, so later edits to the product
do not affect running or queued jobs.

A product submission accepts the same optional per-job overrides as a raw
`POST /api/v1/jobs`: `owner`, `submitter`, `priority`, `project`, `depends_on`
(IDs of upstream jobs in the same farm; the job starts blocked until they
complete), and the retry policy `max_attempts`, `retry_delay_seconds`,
`failure_limit`. An omitted field inherits the queue → farm → server default.
`internal/api/openapi.yaml` (`SubmitProductJobRequest`) is the authoritative
wire contract.

---

## Definition file format

A product is defined in a YAML file with two sections: metadata fields and an
inline `template:` key.

```yaml
name: python
title: Run a Python Script
description: Run a Python script with a chosen interpreter.
category: General
version: 1.0.0
template:
  specificationVersion: jobtemplate-2023-09
  name: Python
  parameterDefinitions:
    - name: Interpreter
      type: STRING
      default: python3
      userInterface:
        control: LINE_EDIT
        label: Interpreter
    - name: Script
      type: STRING
      userInterface:
        control: MULTILINE_EDIT
        label: Python Script
  steps:
    - name: Run
      script:
        embeddedFiles:
          # name is an OpenJD <Identifier> and is the key Task.File
          # references resolve against, so it cannot contain a dot. The
          # on-disk basename goes in filename.
          - name: script
            type: TEXT
            filename: script.py
            data: "{{Param.Script}}"
        actions:
          onRun:
            command: "{{Param.Interpreter}}"
            args: ["{{Task.File.script}}"]
```

### Metadata fields

| Field | Required | Description |
|---|---|---|
| `name` | yes | Stable slug identity. Lowercase letters, digits, `_` and `-`, with at most one `/` namespace separator (e.g. `studio/maya-render`). |
| `title` | yes | Human-readable display name. |
| `description` | no | Short plain-text catalog blurb, max 500 runes (a Unicode character count, not a byte count). Shown in cards and in DCC submitter dropdowns, and matched by product search. **Plain text, not Markdown** — it reaches consumers that cannot render markup, such as the Blender addon's tooltip. |
| `readme` | no | Long-form Markdown documentation, max 8000 runes. Rendered on the product's detail page in the web UI. **Not searched**, and not carried in the preset library index. |
| `category` | no | Free-form group label (e.g. `General`, `Rendering`). |
| `version` | no | Semver string used for future update-detection. |
| `template` | yes | Inline OpenJD job template (`specificationVersion: jobtemplate-2023-09`). |

The `name` slug must match `^[a-z0-9][a-z0-9_-]*(/[a-z0-9][a-z0-9_-]*)?$`.
The inline template is re-serialized and fully validated (`openjd.Parse` and
`openjd.ValidateWithBudget`) when the definition is parsed, so a malformed
template is rejected at load time. Validation is bounded: `ParseDefinition` and
`ValidateTemplate` take a required `product.ValidateOptions` carrying the
operator's configured EXPR limits (`openjd.expr_*`) and a per-request
wall-clock deadline (`openjd.expr_submission_deadline`). Every HTTP route that
reaches this package sets both. Callers outside a request pass `EnforceLimits`
alone: the built-in loader (`internal/product/builtins.go`), which runs at
package init before any configuration exists, the offline index builder
`internal/presetgen`, and the `internal/presettest` test harness. A template
that breaches the deadline returns `503` rather than `400`, because the same
body may validate on an idle server.

### Writing a `readme`

Write `readme` as a YAML **literal** block (`|`), not the folded block (`>-`)
that `description` uses. A folded scalar collapses newlines, so paragraphs and
lists run together into one paragraph, with no error:

```yaml
description: >-
  Splits a video into segments and transcodes them in parallel.
readme: |
  # FFmpeg Segment Transcode

  Splits the source into fixed-length segments, transcodes each on its own
  worker, then concatenates the results.

  ## When to use it

  - Long sources where a single-worker transcode would take hours.
  - Codecs that tolerate segment boundaries.

  Set `SegmentSeconds` to trade parallelism against concat overhead.
```

`readme` is inline content, not a path: `readme: ./README.md` renders the
literal text `./README.md`.

`description` is plain text and is what product search matches. `readme` is
Markdown, rendered only on the detail page, and is **not** searched.

**Supported Markdown:** paragraphs, unordered and ordered lists, fenced code
blocks, ATX headings (`#` renders as `<h3>`, nested under the page's existing
heading structure, clamped at `<h6>`), `**bold**`, `*italic*`, `` `code` ``
and `[links](https://example.com)` using `http:`, `https:` or `mailto:` only.

**Lists are single-level only.** The renderer
(`web/src/components/Markdown.tsx`) matches list items only at column 0, so an
item indented under another renders as an ordinary paragraph with a stray
leading `-` or `1.`, with no error or warning. Write every list flat.

**Not supported,** and rendered as literal text: images, tables, blockquotes,
reference links, raw HTML, and nesting of any kind. Images are excluded
because a remote image in a preset readme would send a request, and the
viewer's IP address, to its host for every viewer.

### `userInterface` parameter hints

The `userInterface` block on each parameter is base-spec OpenJD, not a product
extension. sqi passes these hints to API clients and the web UI, which use them
to choose form controls.

---

## Built-in products

Seven products are embedded in the `sqi-server` binary. They are YAML files
under `internal/product/builtins/`, compiled in with `//go:embed`, parsed and
validated at process init, and served read-only from the catalog. PUT and
DELETE against a built-in return `403 Forbidden`.

### `script` — Run a Shell Command

The minimal product shape: one `STRING` parameter with a `MULTILINE_EDIT`
control. Runs `/bin/sh -c "{{Param.Command}}"`. Gated to
`attr.worker.os.family anyOf ["linux", "macos"]`, since Windows has no
`/bin/sh`; use `script-powershell` on Windows workers.

### `script-powershell` — Run a PowerShell Command

The Windows counterpart of `script`: runs one PowerShell command on a Windows
worker, as a single task. Gated to `attr.worker.os.family anyOf ["windows"]`.
One `STRING` parameter (`Command`) with a `MULTILINE_EDIT` control. The
command is written to an embedded `command.ps1` and run with
`powershell -NoProfile -NonInteractive -ExecutionPolicy Bypass -File`, so
pipelines, redirection, `;`, several lines, comments, non-ASCII text and
`& .\script.ps1` all work, and nothing can block waiting for input. It runs
`powershell` (Windows PowerShell 5.1, which ships with Windows) rather than
`pwsh`, which does not ship with Windows.

The command is a file rather than `-Command` text because OpenJD forbids a
line break in a template's literal `args`: a `-Command` wrapper would have to
join its statements with `;`, and a trailing `#` comment in the command would
then swallow the wrapper. Windows PowerShell 5.1 reads a `.ps1` with no
byte-order mark in the ANSI code page, so `command.ps1` starts with a UTF-8
BOM.

The exit status is decided after the whole command has run, mostly by its last
statement, as with `/bin/sh -c`:

- an explicit `exit N` exits `N`;
- a native executable's non-zero exit code is forwarded. `$LASTEXITCODE` keeps
  the most recent native command's code, so `a.exe; Write-Host done` still
  fails when `a.exe` did, but `a.exe; b.exe` reports only `b.exe`;
- a thrown exception, or a cmdlet error in the last statement, exits 1;
- otherwise it exits 0.

A native command writing to stderr is **not** a failure, even when the stderr
is redirected (`2>&1`, `2> log.txt`). Windows PowerShell 5.1 records each
redirected stderr line as a `NativeCommandError` and clears `$?`; the wrapper
ignores those, since tools such as ffmpeg write their progress there. As a
result, a cmdlet error in an earlier statement does not fail the task if later
statements succeed. To stop at the first cmdlet error, begin the command with
`$ErrorActionPreference = 'Stop'`; 5.1 then also stops on redirected native
stderr. `TestScriptPowerShell_ExitStatus` in
`test/integration/preset_exec_test.go` tests each of these rules against a
real `powershell.exe`.

### `command-sequence` — Run a Shell Command per Frame

The fan-out counterpart of `script`: runs one shell command per frame, or per
chunk of consecutive frames, across a range. Its parameters are `Command`
(`MULTILINE_EDIT`), `Frames` (OpenJD range syntax) and `FramesPerTask` (`INT`,
default 1, minimum 1). `FramesPerTask` sets the largest chunk through a
format-string `defaultTaskCount`; a range that does not divide evenly is split
into near-equal chunks. Each task's command sees `SQI_FRAME_START`,
`SQI_FRAME_END`, `SQI_FRAMES` (always `START-END`) and `SQI_FRAME` (equal to
`SQI_FRAME_START`), exported by a prefix on the `/bin/sh -c` string. A chunk
never spans a gap in the range, so `-s $SQI_FRAME_START -e $SQI_FRAME_END`
renders exactly the requested frames. Gated to Linux and macOS, like `script`.

It runs arbitrary code as the worker's account. On a shared farm, set the
queue's `run_as_user` so tasks run as an operator-chosen account (see *Queue
identity* in [configuration](configuration.md) and the worker's [`isolation`
settings](worker-configuration.md)).

### `command-sequence-powershell` — Run a PowerShell Command per Frame

The Windows counterpart of `command-sequence`, and the fan-out counterpart of
`script-powershell`: the same parameters and the same four variables, read as
`$env:SQI_FRAME_START` and so on. The command runs from `script-powershell`'s
BOM-prefixed `command.ps1`, with the variables assigned before `Command` and
that product's exit-status handling unchanged after it. Gated to Windows.
The same `run_as_user` recommendation applies.

### `python-sequence` — Run a Python Script per Frame

The fan-out counterpart of `python`: the same `Interpreter` and `Script`
parameters plus `Frames` and `FramesPerTask`, and the same four variables, read
from `os.environ`. A small embedded launcher sets them and runs the script as
`script.py` in a child process started with `sys.executable` (the
`Interpreter`), so `__name__ == "__main__"`, `sys.argv[0]`, `sys.path[0]` and
Windows `multiprocessing` behave as when the script is run by hand. The
launcher forwards the script's exit code (a POSIX signal death exits
`128 + N`) and survives a cancellation signal, so that exit code is the one
reported. Not OS-gated; `python3` is often missing on Windows, so set
`Interpreter` there. The same `run_as_user` recommendation applies.

### `python` — Run a Python Script

Two parameters (`Interpreter` and `Script`), with an OpenJD `embeddedFiles`
block that writes the script body to a file named `script.py`. The interpreter
defaults to `python3`.

### `container` — Run a Docker Image

Uses `hostRequirements.attributes` to gate tasks to workers tagged
`docker=true`: the step requires `attr.worker.tag.docker` with `anyOf: ["true"]`,
which a worker satisfies via `capability_tags: ["docker=true"]` in its
configuration (see [worker-capabilities.md](worker-capabilities.md)). Runs
`docker run --rm {{Param.Image}}`.

**A note on the DCC reference presets and auto-detection.** The
`presets/sqi/*.yaml` reference products (Maya, Nuke, Houdini, Blender,
Cinema 4D, 3ds Max, After Effects, Mistika Boutique/VR/Workflows; see
[docs/dcc-submitters.md](dcc-submitters.md#reference-presets)) gate the same
way: a `hostRequirements.attributes` entry requiring `attr.worker.tag.<app>`
with `anyOf: ["true"]`. `sqi-worker` detects a standard install of each of
those applications and advertises the matching tag (e.g. `maya`) with value
`"true"`, with no configuration (see [Capability
auto-detection](worker-capabilities.md#capability-auto-detection-built-in-dcc-detectors)),
so a worker with a standard install satisfies these gates. `docker` has no
built-in detector, so the `container` product still needs the manual
`capability_tags: ["docker=true"]` entry. For an install in a nonstandard
location, or an in-house tool no built-in detector covers, add a manual tag or
a [custom detector](worker-capabilities.md#writing-custom-detectors). The
[standalone renderer
presets](preset-library.md#standalone-renderer-reference-presets) gate the same
way on `arnold`, `vray`, `husk`, `redshift`, `renderman`, `threedelight` and
`mantra`, and the tile preset's assembly step gates on `oiiotool`. Each of
these tags has a built-in detector.

---

## Sources

Every product carries a `source` field identifying where it came from:

| Source | Value | Description |
|---|---|---|
| Built-in | `builtin` | Embedded in the binary. Served read-only; cannot be mutated or deleted. |
| Custom | `custom` | Authored on this server via `POST /api/v1/products`. Mutable. |
| Installed | `installed` | Installed from the preset library. Read-only (PUT returns 403); can be deleted. |

Built-in names are reserved: `POST /api/v1/products` rejects a `name` that
matches an existing built-in.

---

## Installing from the preset library

The **preset library** is a hosted catalog of ready-made product definitions
you can browse and install without writing YAML. Installed presets become
products with `source: installed`.

See [`docs/preset-library.md`](preset-library.md) for the full guide, including
the index format, configuration, and the browse → preview → install flow.

The official library ships twenty-one DCC `Rendering`-category presets, plus
eight [standalone renderer
presets](preset-library.md#standalone-renderer-reference-presets). Fourteen of
the DCC presets (`maya-layer-render`, `maya-scene-render`,
`houdini-rop-render`, `nuke-write-render`, `nuke-script-render`,
`blender-batch-render`, `blender-view-layer-render`, `cinema4d-render`,
`cinema4d-take-render`, `3dsmax-render` and the four After Effects presets
`after-effects-render-windows`, `after-effects-render-macos`,
`after-effects-queue-render-windows`, `after-effects-queue-render-macos`) are
the targets of the [`sqi-submitter`](dcc-submitters.md) in-application
submitters. The other seven have no in-application submitter and are submitted
from the web UI or the API: `mistika-boutique-render`, `mistika-vr-render`,
`mistika-workflows-render`, `katana-render`, `silhouette-render`,
`silhouette-render-all` and `fusion-render`.
`3dsmax-render` and the After Effects presets also gate on the worker's OS.
Parameter names follow a documented, versioned, additive-only convention:
`SceneFile` and `Frames` on all but the two After Effects queue presets (which
take only `SceneFile`), an output parameter only where the command takes one
(`OutputDir` on the Maya presets, `OutputPath` on Blender, Cinema 4D and
3ds Max), plus per-host extras like
`Renderer`/`RopPath`/`WriteNode`/`Scene`/`ViewLayer`/`Take`/`RenderQueueItem`.
Duplicate one and keep the parameter names to keep submitter pre-fill
working. Full reference:
[`docs/dcc-submitters.md`](dcc-submitters.md).

The library also ships five `Transcoding`-category ffmpeg presets
(`ffmpeg-transcode`, `ffmpeg-sequence-encode`,
`ffmpeg-segment-transcode-bash`, `ffmpeg-segment-transcode-powershell`,
`ffmpeg-segment-transcode-expr`). They have no host-application submitter and
declare no scene-file parameters for pre-fill, so install and submit them from
the web UI or REST API. Each gates on the `attr.worker.tag.ffmpeg = "true"`
capability tag, which `sqi-worker` sets on any worker with `ffmpeg` on `PATH`
(see [worker capability tags](worker-capabilities.md)). The three segmented
variants split a source into slices across the farm. They differ in how they
join the slices back together (bash, PowerShell, or a shell-free EXPR
template), in what that join requires of the worker (only the PowerShell
variant is gated to one OS), and in whether they delete the slice files
afterwards. Full reference:
[`docs/preset-library.md`](preset-library.md#transcoding-reference-presets).

Key points about installed products:

- **Read-only.** `PUT /api/v1/products/{name}` returns 403. The template changes
  only through the preset library install flow, when the library publishes an
  update and you choose to apply it.
- **Uninstallable.** `DELETE /api/v1/products/{name}` works normally.
- **Duplicate to custom.** Every product, installed and built-in included, has a
  **Duplicate to custom** action that creates an editable local copy
  (`source: custom`) no longer tied to the library.

---

## Version and stable-name identity

`name` is the stable identity of a product across its lifetime. The `version`
string (e.g. `1.0.0`) is stored with the template as a label only; nothing
updates a product automatically when its version changes.

---

## REST surface

All endpoints are under `/api/v1/products`.

### `GET /api/v1/products`

Returns all products, built-ins and stored products together, ordered by name.

Response: `200 OK`, array of `Product` objects.

### `POST /api/v1/products`

Creates a custom product. The server validates the template before writing it
to the state store, and rejects a name that matches a built-in.

Request body:

```json
{
  "name": "my-render",
  "title": "My Renderer",
  "description": "Renders a frame range.",
  "category": "Rendering",
  "version": "1.0.0",
  "template": "<OpenJD YAML or JSON string>",
  "format": "yaml"
}
```

`format` is `"yaml"` (default) or `"json"`.

Responses: `201 Created` (product), `400 Bad Request`, `403 Forbidden` (auth on,
missing `products.manage`), `409 Conflict`, `503 Service Unavailable` (template
validation exceeded `openjd.expr_submission_deadline`; retry, since the same
body may validate on an idle server).

### `GET /api/v1/products/{name}`

Returns the named product, preferring a built-in over a stored product of the
same name.

Responses: `200 OK` (product), `404 Not Found`.

### `PUT /api/v1/products/{name}`

Replaces the mutable fields of a stored product. Built-ins return
`403 Forbidden`.

Request body: same shape as `POST /api/v1/products` (name in path takes
precedence over name in body).

Responses: `200 OK`, `400`, `403`, `404`, `503` (validation deadline exceeded).

### `DELETE /api/v1/products/{name}`

Deletes a stored product. Built-ins return `403 Forbidden`. Does not affect
already-submitted jobs (which snapshot their template at submission).

Responses: `204 No Content`, `403`, `404`.

### `GET /api/v1/products/{name}/parameters`

Returns the parsed job parameters from the named product's template, including
`userInterface` hints. The product submission form calls it to build its
fields.

Response: `200 OK`, array of `ProductParameter` objects in template order:

```json
[
  {
    "name": "Interpreter",
    "type": "STRING",
    "description": "",
    "default": "python3",
    "allowed_values": null,
    "min_value": null,
    "max_value": null,
    "min_length": null,
    "max_length": null,
    "object_type": "",
    "data_flow": "",
    "user_interface": {
      "control": "LINE_EDIT",
      "label": "Interpreter",
      "group_label": "",
      "decimals": null
    },
    "file_filters": null,
    "file_filter_default": null,
    "item": null
  }
]
```

`file_filters` / `file_filter_default` carry a PATH parameter's file-dialog
filters (the Mistika presets use them to offer `*.rnd`); `item` carries a
`LIST[*]` parameter's per-element constraints and is `null` for a scalar.

Responses: `200 OK` (array), `403 Forbidden`, `404 Not Found` (product not
found), `422 Unprocessable Entity` (the product's stored template cannot be
parsed; templates are validated on create and update, so this normally appears
only for a row written some other way).

### `POST /api/v1/products/{name}/jobs`

Submits a job from the named product's template. The server loads the template
from the catalog, applies the supplied parameters, expands the parameter space
into tasks, and stores the job, as `POST /api/v1/jobs` does with a raw template
body.

Request body:

```json
{
  "farm_id": "<uuid>",
  "queue_id": "<uuid>",
  "name": "My Python Job",
  "owner": "alice",
  "submitter": "my-tool/1.0",
  "priority": 50,
  "project": "my-project",
  "parameters": {
    "Script": "print('hello')",
    "Interpreter": "python3"
  }
}
```

`farm_id` and `queue_id` are required. `name` is optional and overrides the
template's job name; when omitted, the template's own name is used. The web
submission form defaults it to `"<product title> <timestamp>"`. `parameters` is
a flat `string→string` map whose keys must match the parameter names declared
in the product's template. A `LIST[*]` parameter's value is the JSON encoding
of the list, as a string, for example `"[\"main\",\"closeup\"]"`. Missing keys with defaults are filled
automatically; a missing required key returns 422.

Responses: `201 Created` (`Job` object, same shape as `POST /api/v1/jobs`),
`400`, `403`, `404`, `422 Unprocessable Entity` (template/parameter validation
failure), `503` (expression evaluation exceeded
`openjd.expr_submission_deadline`).

---

## Submitting from a product (web)

The `/submit` route is the product picker: it lists every available product and
lets the user choose one. Selecting a product navigates to
`/submit/product/:name`, which fetches the product's parameters via
`GET /api/v1/products/{name}/parameters` and renders a form with one field per
parameter, using its `userInterface` control hint. The form defaults the
job name to `"<product title> <timestamp>"` and posts to
`POST /api/v1/products/{name}/jobs` on submit.

The raw OpenJD editor is at `/submit/raw`, for one-off submissions that do not
use the catalog.

## Path translation

Path translation lets a product specify how concrete paths reach the
application. It uses the `SQI_PATH_TRANSLATION` extension and offers five
delivery mechanisms:

1. **`swap_in_place`** — String substitution of path parameters in the template
   (sqi convenience, not in OpenJD spec).
2. **`translation_file`** — Native OpenJD `pathmapping-1.0` file format, served
   via the `{{Session.PathMappingRulesFile}}` format string. Prefer this when your
   app can read a mapping file.
3. **`command_flags`** — Append individual `src`/`dest` pairs as command-line flags
   (pattern: `--remap {src}={dest}`). Use only for apps needing per-pair flags.
4. **`environment`** — Set an environment variable to `src=dest` pairs joined by
   the OS path-list separator. Use only for apps needing per-pair environment
   variables.
5. **`stage_locally`** — Copy job-level PATH parameters with `dataFlow` IN/INOUT
   to worker-local scratch before the run, and copy OUT/INOUT back after (sqi
   convenience, not in OpenJD spec). It works with no worker configuration,
   since an unconfigured worker falls back to a TEMP scratch directory and
   sqi's built-in copy, but a farm spanning several compute locations needs
   `staging.scratch_dir` and `staging.sync_command` set on the worker for
   remote transfer.

Deliveries run in a fixed order and can be combined; a product can declare all
five. With no `SQI_PATH_TRANSLATION` extension declared, the default is the
first two (`swap_in_place`, `translation_file`).

**Relationship to native OpenJD:** base OpenJD provides the
`{{Session.PathMappingRulesFile}}` format string for apps that read a mapping
file; use it through `translation_file`. `command_flags` and `environment`
serve apps that need individual pairs, which native OpenJD cannot express.
`swap_in_place` and `stage_locally` are sqi conveniences for applications that
do not understand path mapping, and for worker-local staging.

**Staging requirement:** Staging (`stage_locally`) applies only to job-level PATH
parameters declared with `dataFlow: IN`, `OUT`, or `INOUT`. Step-level parameters
and non-PATH types are not staged.

For full details, schema reference, and examples, see
[`docs/openjd-extensions/path-translation.md`](openjd-extensions/path-translation.md).

## Limiting product concurrency

There is no separate product limit setting. Use **usage pools**: a usage pool
is a named cap that the scheduler enforces by counting active claims across the
whole farm, whatever the compute location, and releases a slot when a task
reaches a terminal state.

To cap how many tasks from a product may run at once, register a pool on the
**Usage Pools** page (or via `POST /api/v1/usage-pools`) and declare a
requirement for it on the relevant steps of the product's template:

```yaml
steps:
  - name: render
    hostRequirements:
      amounts:
        # "maya" must match a usage pool registered on the server.
        - name: amount.worker.usagepool.maya
          min: 1
    # ...
```

A pool can stand for a renderer version, a show, a client code, or a software
license, and a step may name several at once (a per-show cap and a license
count enforced together). Capacity lives in the operator-owned pool rather
than in the product, so there is one seat count, and a raw submission cannot
bypass it.

> **Note.** A product references a pool *by name*, so an installed or
> duplicated product whose template names a pool that does not exist on this
> server is treated as zero-capacity: its tasks stay `ready` until an admin
> registers the pool.

See the [usage pools section of the OpenJD submission
guide](openjd-submission.md#4-usage-pools) for the full reference, multi-pool
behavior, and the live utilization view.
