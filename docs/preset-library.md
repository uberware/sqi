<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
# Preset Library

The **preset library** is a catalog of ready-to-use product definitions hosted at a
configurable URL. Each entry is a product definition file — the same format as a
hand-authored custom product (`name`, `title`, `description`, `category`, `version`,
and an inline OpenJD `template:`) — that can be fetched and installed on the farm
without writing any YAML.

> **Installed presets run commands on your farm. Only configure a preset library
> you trust.**

---

## How it works

The preset library operates at two levels:

1. **Index** — a single JSON file listing every available preset with metadata and a
   SHA-256 checksum of its definition file. `sqi-server` fetches the index when you
   open the Preset Library page and caches it in memory for a few minutes. The
   **Refresh** button forces a fresh fetch.

2. **Definition files** — individual product definition YAML files referenced by the
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
product is shown as having an update available). It is not a cryptographic authorship
signature — the trust boundary is the configured index URL itself.

The index carries `description` but **not** `readme`. `description` is there
because the preset list page searches it; `readme` is not searched, so shipping
it in the index would grow every client's cached index for nothing. A preset's
readme arrives with its definition when the detail page is opened.

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

All endpoints return 503 when `preset_library.url` is empty. A 503 also means
the definition's validation exceeded `openjd.expr_submission_deadline` on this
server (retry; it is not a rejection of the preset). An unreachable or
unparseable **index** is 502.

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
- **Uninstallable.** `DELETE /api/v1/products/{name}` works normally — removing the
  product from the catalog without affecting any jobs that were already submitted
  from it (those snapshot their template at submission time).
- **Update available.** When the library's SHA-256 for a preset changes, the Browse
  and Preview pages surface an **Update available** badge. Updating re-fetches the
  definition, verifies the new hash, and overwrites the stored product. Because
  installed products cannot be edited in place, no locally authored content is at
  risk of being clobbered.
- **Duplicate to custom.** Every product — including installed ones — has a
  **Duplicate to custom** action. Use it to create a locally-editable copy that is no
  longer tied to the library. The duplicate gets `source: custom` and behaves as a
  fully mutable product; it is not affected by future library updates.

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

The official library ships `Rendering`-category presets — `maya-layer-render`,
`maya-scene-render`, `houdini-rop-render`, `nuke-write-render`,
`nuke-script-render`, `blender-batch-render` — as ready targets for the
[`sqi-submitter`](dcc-submitters.md) in-application submitters, plus
`mistika-boutique-render`, `mistika-vr-render` and `mistika-workflows-render`,
which have no in-application submitter and are submitted from the web UI or
the API. Each names its parameters from the [parameter convention
contract](dcc-submitters.md#the-parameter-convention-contract) so submitter
pre-fill works out of the box — all nine declare `SceneFile` and `Frames`; an
output parameter is declared only where the command takes one (`OutputDir` on
the two Maya presets, `OutputPath` on Blender), alongside per-host extras such
as `Renderer`, `RenderLayer`, `RopPath` and `WriteNode`. Each gates on a worker
capability tag that `sqi-worker` auto-detects from a standard install with no
per-worker configuration — manual tags are needed only for nonstandard
install paths (see [capability
auto-detection](worker-capabilities.md#capability-auto-detection-built-in-dcc-detectors)).
See [`docs/dcc-submitters.md`](dcc-submitters.md) for the full reference,
including the chunking behavior and worker requirements per preset. Every
software tag a shipped preset requires must be emitted by a built-in detector —
enforced by `TestBuiltinDetectors_CoverPresets`, so a new reference preset
cannot ship without one.

## Transcoding reference presets

The official library also ships five `Transcoding`-category presets under
`presets/sqi/*.yaml` — plain ffmpeg jobs meant to be submitted directly rather
than through a DCC submitter. Each gates on the `attr.worker.tag.ffmpeg =
"true"` capability tag, which `sqi-worker` sets automatically on any worker
with `ffmpeg` on `PATH` (see [worker capability
tags](worker-capabilities.md)); only the PowerShell-joined segmented variant
adds an OS gate on top of that:

- `ffmpeg-transcode` — converts one video file on one worker, start to
  finish. Base-spec OpenJD (declares no extensions), so it runs on any
  `ffmpeg=true` worker regardless of that worker's EXPR limit configuration.
- `ffmpeg-sequence-encode` — turns a rendered image sequence into a movie on
  one worker, the step that typically follows a render job. Declares `EXPR`.
- Three segmented variants split a long source into fixed-length slices,
  transcode each slice — possibly on a different worker, though the
  scheduler may also reuse one — then join the slices back into a single
  file. All three need Source Duration entered by hand, since nothing can
  measure it before submission. Pick one based on your farm:
  - `ffmpeg-segment-transcode-bash` — joins with a bash script. Carries no OS
    gate: bash is not POSIX-only, since git-bash puts it on Windows too, so
    this variant runs on any `ffmpeg=true` worker with `bash` on `PATH`. It
    invokes `bash` explicitly with the script as an argument rather than
    exec'ing the script directly, because executing a `#!` script is a POSIX
    kernel feature Windows has no equivalent of, and the script folds
    backslashes in the output path to forward slashes so its `dirname`,
    `basename`, and glob work on a Windows path. Removes its slice files once
    the join succeeds.
  - `ffmpeg-segment-transcode-powershell` — joins with a PowerShell script;
    needs Windows workers. Removes its slice files once the join succeeds.
  - `ffmpeg-segment-transcode-expr` ("Portable") — needs no shell at all, so
    it runs on any `ffmpeg=true` worker without even requiring `bash`; its join
    file list is generated by the template itself. That list-building cost is
    charged at
    submission and grows with the slice count, so it suits jobs of up to 400
    slices — past that, use the Bash or PowerShell variant, whose cost does
    not grow with slice count. Unlike the two shell variants, it leaves its
    slice files beside the output for you to remove.

Install and use them exactly like the DCC reference presets above (Browse →
Install as a product).

## Testing presets

The official library also ships four `Testing`-category presets — authored in
this repo under `presets/testing/` and published to the library under a
`testing/` namespace — for smoke-testing a fresh farm without a DCC installed:

- `test-render-bash` / `test-render-powershell` — a no-op render simulator that
  sleeps per frame and can inject failures, hangs, and progress (bash and
  PowerShell variants), to exercise task chunking, the worker process executor,
  and the retry/timeout paths.
- `test-steps-bash` / `test-steps-powershell` — a multi-step job (render →
  publish → notify) that exercises step-dependency gating and cross-step
  ordering.

Install them exactly like the reference presets (Browse → Install as a product),
then submit with the generated form. Because they depend on no software, they
require no worker capability tags — any online worker on the matching OS can run
them.

---

## Validation tiers

`presets/validation-tiers.yaml` tracks exactly 17 entries — the 14 presets
under `presets/sqi/` plus the three built-in products documented in
[`docs/development.md`](development.md#adding-a-product) — stating which
validation tier each job type has actually reached. The four `presets/testing/`
presets above are intentionally out of scope for this registry: unlike the
reference presets, they need no vendor application to prove anything — they
*are* the stand-in executable, meant to be installed and run directly against
a real farm rather than reviewed tier by tier. The tiers are:

- **Tier 1 — argv snapshot.** The template is expanded and resolved through
  the real production submit → assign → resolve pipeline
  (`internal/presettest`, driving the same code path a live server and worker
  use) and the resulting command line is compared byte-for-byte against a
  reviewed golden file.

  **What a green Tier-1 test proves:** the preset expands to *exactly* the
  command line a human reviewed, and any future change to expansion,
  resolution, or the preset's own template shows up as a golden diff someone
  has to look at.

  **What it does NOT prove: that the vendor's application accepts that
  command line.** No Maya, Nuke, Houdini, Blender, or Mistika installation
  executes anything at Tier 1 — the argv is correct only insofar as the
  documentation it was derived from is correct and current. This is not a
  hypothetical gap: the Tier-1 retrofit that built this harness found two real,
  currently unfixed defects this way, both recorded as `caveat` text on the
  affected entries in `presets/validation-tiers.yaml` rather than fixed
  (fixing a preset is out of scope for this harness):
  - `nuke-write-render` and `nuke-script-render` pass Nuke's `-F` flag an
    OpenJD-syntax stepped range (`1-19:2`). Foundry documents the increment
    separator as `x` (`1-19x2`), not a colon — so the flag Nuke actually
    receives may not be the flag Nuke actually accepts. Only a real Nuke (Tier
    2) can settle it.
  - The three Mistika presets — and, latently, Maya and Blender if an
    operator raises their chunk size above the shipped default — can
    **silently render more frames than requested**: a stepped `Frames` range
    collapses to its contiguous span (`-s`/`-e`) once it crosses a chunk
    boundary, because `SQI_CHUNK_BOUNDS` has no way to express a step. Ten
    requested frames become nineteen rendered ones.

  Read a preset's `caveat` field before trusting its golden for anything more
  than "the template still expands the way it did when this was reviewed."

- **Tier 2 — real application.** A real, licensed copy of the vendor
  application runs the resolved command and its output is inspected. The nine
  render presets (Maya, Nuke, Houdini, Blender, the three Mistika products)
  have not reached Tier 2 — none of those vendor applications has a
  redistributable, CI-friendly way to run headless in this project's CI, so
  their entries in `presets/validation-tiers.yaml` carry no `tier2` block.
  The five ffmpeg presets *are* Tier 2: ffmpeg is freely available, so each of
  their registry entries carries a `tier2` block, and
  `test/integration/ffmpeg_presets_test.go` runs real ffmpeg end to end and
  decodes the produced file — real Tier-2-grade evidence, checked against the
  registry the same way Tier 1 and Tier 3 are.

- **Tier 3 — real pipeline, no vendor license.** A real `sqi-server` and
  `sqi-worker`, wired together exactly as in production, execute the preset
  end to end — but the worker's OpenJD action target is
  `test/stubproc`, a recording stand-in binary rather than the real vendor
  executable. This proves the *sqi-side* plumbing (task assignment, worker
  process execution, argument delivery, exit-status handling)
  without needing a Maya or Nuke license in CI, and cross-checks the
  arguments the stub actually observed against the Tier-1 golden for the same
  case — in both directions, so a resolution that collapsed every task onto one
  argv would fail. Environment delivery is **not** covered: the stub records
  only the command, its arguments, its working directory and its stdin, so
  nothing here observes the variables a task's process was handed.

**The registry is verified, not maintained by hand.** Every claim in
`presets/validation-tiers.yaml` is checked by
`test/integration/preset_tiers_test.go`'s `TestZZPresetTierRegistrySatisfied`
against what the suite actually ran — not what the file merely asserts. A
Tier-1 claim needs a fixture case and a golden that exist; a Tier-3 claim
needs a named test that actually ran, and **a Tier-3 case that skipped on a
platform listed in that entry's `required_on` is a registry failure**, not a
quiet no-op — every container-backed target in this repo can exit 0 while
running nothing, and this is enforced in Go rather than by a CI job asserting
test names by hand. An entry's `tier3.required_on` need not name every
platform: it may leave out a platform the preset itself is gated against, so a
skip there is expected rather than a registry failure. For example, `script`
(the POSIX-only built-in) gates on `attr.worker.os.family anyOf ["linux",
"darwin"]`, so a Windows worker could never run it — its `tier3.required_on:
[linux, darwin]` deliberately omits `windows`, rather than naming a platform
the skip check could never see a real run on. An empty `required_on: []` is
rejected at load time instead, because it would make the skip check
permanently unfalsifiable on every platform.

### Adding a preset to the harness

1. **Fixture** — add a case file at `test/integration/testdata/preset-cases/<preset-name>.yaml`
   describing the job parameters for each scenario you want reviewed (at
   minimum a `default` case).
2. **Golden** — run
   `go test ./test/integration/ -run TestPresetTier1Argv -preset-update` to
   generate `test/integration/testdata/preset-argv/<preset-name>--<case>.golden`
   for each case.
3. **Registry entry** — add the preset to `presets/validation-tiers.yaml` with
   its `tier1.cases` list, a `caveat` describing what the golden is derived
   from and what it does not prove, and — if a Tier-3 case exists and can run
   in this environment — a `tier3` block naming the test and the platforms it
   must not skip on (`required_on`).
4. **Review the diff.** A regenerated or newly generated golden is not a
   passing test by itself; reading the diff *is* the test. Check the argv
   against the vendor's actual documented CLI, not just against what the
   template was expected to produce.

`make test-preset-harness` runs the whole harness (Tiers 1, 2 and 3, plus the
registry verification) locally, in one `go test` invocation —
`TestZZPresetTierRegistrySatisfied` asserts on what the tier tests recorded in
that same process, so splitting the run across two `go test` invocations makes
the registry check fail with "never reported" in both. ffmpeg must be on
`PATH`: the registry's `tier2` blocks require the real-ffmpeg cases to run,
and a skip on a platform named in `required_on` is a registry failure, not a
harmless no-op.

### Tier 3 on Windows

Tier 3 runs on Windows exactly as it does on Linux and macOS — a real
`sqi-server` and `sqi-worker`, wired together, executing every preset end to
end against `test/stubproc` in place of the vendor executable — and CI proves
it in a dedicated job (`preset-harness-windows`) rather than assuming a
Linux-passing suite behaves the same way on another host. Of the 18 entries in
`presets/validation-tiers.yaml`, 15 require **all three** platforms
(`tier3.required_on: [linux, darwin, windows]`), so a skip on Windows is a
registry failure for every one of them, not a harmless no-op. The other 3 are
gated to a narrower platform subset, because their Tier 3 case cannot run
everywhere by construction — `script` (POSIX-only, `required_on: [linux, darwin]`),
`script-powershell` (Windows-only, `required_on: [windows]`), and
`ffmpeg-segment-transcode-powershell` (gated on
`attr.worker.os.family anyOf ["windows"]`, `required_on: [windows]`). The
latter two still name `windows` and are required to run on this Windows job —
they simply aren't required anywhere else.
`script-powershell` is `script`'s Windows counterpart: where `script` invokes
`/bin/sh -c`, `script-powershell` invokes `powershell -NoProfile -Command`, so
between the two every worker platform sqi ships has a generic single-command
built-in.
