# Products

A **product** is a named, versioned wrapper around an OpenJD template, stored
unmodified apart from YAML re-serialization when it comes from a definition
file. It gives the template a stable identity (`name`), human-readable metadata
(`title`, `description`, `category`, `version`), and a home in the catalog so
clients can list and submit jobs without ever handling a raw template file.

Products are thin by design. Parameters, their `userInterface` hints (control,
label, group), machine requirements, and step-level setup/teardown all live
**inside** the OpenJD template — not as separate product fields. The product
layer adds only the catalog envelope on top.

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
`openjd.Submitter` pipeline as a raw `POST /api/v1/jobs`. The resulting job row
contains a **snapshot** of the template at submission time, so later edits to the
product do not affect running or queued jobs.

A product submission also accepts the same optional per-job overrides as a raw
`POST /api/v1/jobs`: `owner`, `submitter`, `priority`, `project`, `depends_on`
(IDs of upstream jobs in the same farm; the job starts blocked until they
complete), and the retry policy `max_attempts`, `retry_delay_seconds`,
`failure_limit`. Each is optional;
an omitted field inherits the queue → farm → server default. See
`internal/api/openapi.yaml` (`SubmitProductJobRequest`) for the authoritative
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

The `name` slug constrains to `^[a-z0-9][a-z0-9_-]*(/[a-z0-9][a-z0-9_-]*)?$`.
The inline template is re-serialized and fully validated (via `openjd.Parse` +
`openjd.ValidateWithBudget`) when the definition is parsed — a malformed
template is rejected at load time. Validation is bounded: `ParseDefinition` and
`ValidateTemplate` take a **required** `product.ValidateOptions` carrying the
operator's configured EXPR limits (`openjd.expr_*`) and a per-request wall-clock
deadline (`openjd.expr_submission_deadline`). Every HTTP route that reaches this
package sets both. Two callers pass `EnforceLimits` alone, and neither is a
request path: the built-in loader (`internal/product/builtins.go`), which runs
from package init before any configuration exists, and `internal/presetgen`,
the offline index-build tool. A template that breaches
the deadline is a `503`, not a `400` — the same body would validate on an idle
server.

### Writing a `readme`

`readme` must use a YAML **literal** block (`|`), not the folded block (`>-`)
that `description` uses. A folded scalar collapses newlines, which silently
destroys paragraph breaks and list structure — the result is one run-on
paragraph, with no error to tell you:

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

`readme` is inline content, not a path — `readme: ./README.md` renders the
literal text `./README.md`.

`description` is plain text and is what product search matches. `readme` is
Markdown, rendered only on the detail page, and is **not** searched.

**Supported Markdown:** paragraphs, unordered and ordered lists, fenced code
blocks, ATX headings (`#` renders as `<h3>`, nested under the page's existing
heading structure, clamped at `<h6>`), `**bold**`, `*italic*`, `` `code` ``
and `[links](https://example.com)` using `http:`, `https:` or `mailto:` only.

**Lists are single-level only — nesting is not supported.** The renderer
(`web/src/components/Markdown.tsx`) matches list items with a regular
expression anchored at column 0. An item indented under another list item
does not become a nested list; it silently renders as an ordinary paragraph
with a stray leading `-` or `1.`, with no error or warning. This has already
caught one author on this branch — write every list flat, with no indented
sub-items.

**Not supported,** and rendered as literal text: images, tables, blockquotes,
reference links, raw HTML, and nesting of any kind. Images are excluded
deliberately — a remote image in a preset readme is an IP beacon firing for
every viewer.

### `userInterface` parameter hints

The `userInterface` block on each parameter is base-spec OpenJD, not a product
extension. sqi passes these hints through to API clients and the web UI so they
can render appropriate form controls.

---

## Built-in products

Three products are embedded directly in the `sqi-server` binary. They are
defined as YAML files under `internal/product/builtins/`, compiled in via
`//go:embed`, parsed and validated at process init, and served read-only from the
catalog. Mutations (PUT, DELETE) against a built-in return `403 Forbidden`.

### `script` — Run a Shell Command

Demonstrates the minimal product shape: one `STRING` parameter with a
`MULTILINE_EDIT` control. Executes `/bin/sh -c "{{Param.Command}}"`.

### `python` — Run a Python Script

Demonstrates two parameters (`Interpreter` and `Script`), an OpenJD
`embeddedFiles` block that materializes the script body as a file named
`script.py`, and a configurable interpreter path defaulting to `python3`.

### `container` — Run a Docker Image

Demonstrates `hostRequirements.attributes` to gate tasks to workers tagged
`docker=true`: the step requires `attr.worker.tag.docker` with `anyOf: ["true"]`,
which a worker satisfies via `capability_tags: ["docker=true"]` in its
configuration (see [worker-capabilities.md](worker-capabilities.md)). Runs
`docker run --rm {{Param.Image}}`.

**A note on the DCC reference presets and auto-detection.** The
`presets/sqi/*.yaml` reference products (Maya, Nuke, Houdini, Blender, Mistika
Boutique/VR/Workflows — see
[docs/dcc-submitters.md](dcc-submitters.md#reference-presets)) gate the same
way: a `hostRequirements.attributes` entry requiring `attr.worker.tag.<app>`
with `anyOf: ["true"]`. `sqi-worker` auto-detects a standard install of each
of those applications and advertises the matching tag (e.g. `maya`) with
value `"true"` and no configuration — see [Capability
auto-detection](worker-capabilities.md#capability-auto-detection-built-in-dcc-detectors)
— which satisfies the `anyOf: ["true"]` match above directly, so a worker with
a standard install matches these built-in gates with zero per-worker
configuration. `docker` has no built-in detector, so the `container` product
above still needs the manual `capability_tags: ["docker=true"]` entry.
Nonstandard install paths for Maya/Nuke/Houdini/Blender/Mistika, or an in-house tool
not covered by a built-in detector, can add a manual tag or a [custom
detector](worker-capabilities.md#writing-custom-detectors) instead.

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

The **preset library** lets you browse and install ready-made product definitions
from a hosted catalog without writing any YAML. Installed presets become products
with `source: installed`.

See [`docs/preset-library.md`](preset-library.md) for the full guide, including the
index format, configuration, and the browse → preview → install flow.

The official library ships nine `Rendering`-category presets. Six of them —
`maya-layer-render`, `maya-scene-render`, `houdini-rop-render`,
`nuke-write-render`, `nuke-script-render`, `blender-batch-render` — exist to give
the [`sqi-submitter`](dcc-submitters.md) in-application submitters something real
to target. The other three, `mistika-boutique-render`, `mistika-vr-render` and
`mistika-workflows-render`, have no in-application submitter and are submitted
from the web UI or the API. They name their parameters from a documented,
versioned, additive-only convention — `SceneFile` and `Frames` on all nine, an
output parameter only where the command takes one (`OutputDir` on the Maya
presets, `OutputPath` on Blender), plus per-host extras like
`Renderer`/`RopPath`/`WriteNode` — duplicate one and keep the parameter names
to keep submitter pre-fill working. Full reference:
[`docs/dcc-submitters.md`](dcc-submitters.md).

The library also ships five `Transcoding`-category ffmpeg presets
(`ffmpeg-transcode`, `ffmpeg-sequence-encode`,
`ffmpeg-segment-transcode-bash`, `ffmpeg-segment-transcode-powershell`,
`ffmpeg-segment-transcode-expr`). Unlike the DCC presets above, these are not
submitted from a host application and declare no scene-file parameters for
submitter pre-fill to bind — install and submit them directly from the web
UI or REST API. Each gates on the `attr.worker.tag.ffmpeg = "true"`
capability tag, which `sqi-worker` sets automatically on any worker with
`ffmpeg` on `PATH` (see [worker capability
tags](worker-capabilities.md)); the three segmented variants split a source
into slices across the farm and differ in how they join the slices back
together (bash, PowerShell, or a shell-free EXPR template), which OS they can
run on, and whether they delete the slice files afterwards. Full reference:
[`docs/preset-library.md`](preset-library.md#transcoding-reference-presets).

Key points about installed products:

- **Read-only.** `PUT /api/v1/products/{name}` returns 403. The template is updated
  only via the preset library install flow (when the library publishes an update and
  you choose to apply it).
- **Uninstallable.** `DELETE /api/v1/products/{name}` works normally.
- **Duplicate to custom.** Every product — including installed ones and built-ins —
  has a **Duplicate to custom** action. Use it to create a fully mutable local copy
  (`source: custom`) that is no longer tied to the library and can be edited freely.

---

## Version and stable-name identity

`name` is the stable identity of a product across its lifetime. The `version`
string (e.g. `1.0.0`) is stored alongside the template and is available for
future tooling to detect when an installed product's template has been
superseded by a newer release. No automatic update behavior is implemented in
Phase 2; `version` is a label only.

---

## REST surface

All endpoints are under `/api/v1/products`.

### `GET /api/v1/products`

Returns all products — built-ins merged with stored products — ordered by name.

Response: `200 OK`, array of `Product` objects.

### `POST /api/v1/products`

Creates a new custom product. The server validates the inline template before
writing to SQLite. Rejects names that shadow a built-in.

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
validation exceeded `openjd.expr_submission_deadline` — retry; the same body may
validate on an idle server).

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
`userInterface` hints. This is the endpoint the product submission form calls to
build its field list before the user fills it in.

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
parsed; templates are validated at create/update time, so this normally only
appears for a row written by an older or external path).

### `POST /api/v1/products/{name}/jobs`

Submits a job using the named product's template. The template is loaded from
the catalog, the supplied parameters are applied, the parameter space is
expanded into tasks, and the job is persisted — identical to the `POST
/api/v1/jobs` path, but driven by the catalog rather than a raw template body.

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

`farm_id` and `queue_id` are required. `name` is optional: when supplied it
overrides the job name from the product's template; when omitted, the template's
own name is used. The web submission form defaults it to
`"<product title> <timestamp>"`. `parameters` is a flat `string→string` map;
keys must match the parameter names declared in the product's template. A
`LIST[*]` parameter's value is the JSON encoding of the list, still as a string
— for example `"[\"main\",\"closeup\"]"`. Missing keys with defaults are filled
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
`GET /api/v1/products/{name}/parameters` and renders a dynamic form — one field
per parameter, styled by its `userInterface` control hint. The form defaults the
job name to `"<product title> <timestamp>"` and posts to
`POST /api/v1/products/{name}/jobs` on submit.

The raw OpenJD editor remains reachable at `/submit/raw` for one-off submissions
that do not use the catalog.

## Path translation

Path translation lets a product specify how concrete paths reach the application.
It rides the `SQI_PATH_TRANSLATION` extension and offers five delivery mechanisms:

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
   convenience, not in OpenJD spec). Works with no worker configuration — an
   unconfigured worker falls back to a TEMP scratch directory and sqi's own
   built-in copy — but a farm spanning multiple compute locations needs an
   explicit `staging.scratch_dir` and `staging.sync_command` on the worker for
   real remote transfer.

Deliveries execute in fixed order and are mutually compatible — a product can
declare all five simultaneously. The first two (`swap_in_place`, `translation_file`)
are the default when no `SQI_PATH_TRANSLATION` extension is declared, preserving
existing behavior.

**Relationship to native OpenJD:** The base OpenJD spec provides the `{{Session.PathMappingRulesFile}}`
format string for apps that read a mapping file. Use it (via `translation_file`).
`command_flags` and `environment` exist for apps that need individual pairs —
neither can be expressed by native OpenJD alone. `swap_in_place` and `stage_locally`
are sqi-specific conveniences for path-mapping-unaware applications and for
worker-local staging.

**Staging requirement:** Staging (`stage_locally`) applies only to job-level PATH
parameters declared with `dataFlow: IN`, `OUT`, or `INOUT`. Step-level parameters
and non-PATH types are not staged.

For full details, schema reference, and examples, see
[`docs/openjd-extensions/path-translation.md`](openjd-extensions/path-translation.md).

## Limiting product concurrency

There is no separate "product limit" knob. A product is a thin wrapper over an
OpenJD template, and OpenJD already has a global concurrency mechanism that does
exactly this job: **usage pools**. A usage pool is a named cap that the
scheduler enforces by counting active claims across the whole farm — regardless
of compute location — releasing a slot when a task reaches a terminal state.

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

The same mechanism covers every dimension a "product limit" might mean — a
renderer version, a show, a client code, or a software license — and pools
compose, so a step may name several at once (a per-show cap *and* a license
count enforced together). Capacity lives in the operator-owned pool, not in the
product, which keeps a single authoritative seat count that a raw submission
cannot bypass.

> **Note.** Because a product caps itself by referencing a pool *by name*, an
> installed or duplicated product whose template names a pool that does not
> exist on this server is treated as zero-capacity — its tasks stay `ready`
> until an admin registers the pool. This is intentional: the admin owns real
> capacity.

See the [usage pools section of the OpenJD submission
guide](openjd-submission.md#4-usage-pools) for the full reference, multi-pool
behavior, and the live utilization view.
