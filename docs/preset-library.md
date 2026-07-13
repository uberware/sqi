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

---

## Configuration

See [`docs/configuration.md`](configuration.md#preset_library-remote-preset-catalog)
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

All endpoints return 503 when `preset_library.url` is empty.

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/v1/presets` | List all presets with per-preset status. `?refresh=true` forces a re-fetch of the index. |
| `GET` | `/api/v1/presets/{name}` | Preview a preset — metadata, template, and status. 404 if not in the index. 422 if the definition cannot be fetched or parsed. |
| `POST` | `/api/v1/presets/{name}/install` | Install or update the named preset. 201 on first install, 200 on update or reinstall. 404 if not in the index. 409 if a built-in or custom product already uses that name. 422 on SHA-256 mismatch or validation failure. |

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
[`sqi-submitter`](dcc-submitters.md) in-application submitters.
Each declares its parameters using the [parameter convention
contract](dcc-submitters.md#the-parameter-convention-contract) (`SceneFile`,
`Frames`, `OutputDir`, plus per-host extras) so submitter pre-fill works out of
the box, and each gates on an operator-configured worker capability tag (see
[worker capability tags](worker-capabilities.md)). See
[`docs/dcc-submitters.md`](dcc-submitters.md) for the full reference,
including the chunking behavior and worker requirements per preset.

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
