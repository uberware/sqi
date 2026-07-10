# OpenJD Extensions in sqi

OpenJD templates may declare feature extensions via a top-level `extensions: [NAME]`
list. Extension names are uppercase identifiers matching `[A-Z_0-9]{3,128}`. sqi
validates every declared extension against a registry (`internal/openjd/extension.go`)
and **rejects any it does not implement** — silently accepting an unknown extension
would mis-run the template.

## Registry

Each supported extension is one `Extension{Name, Origin, Status, Summary, DocPath}`
entry. `Origin` is:

- **official** — defined by the upstream OpenJD specification (e.g. `TASK_CHUNKING`).
- **vendor** — defined by sqi.

## Vendor namespacing & promotion path

sqi-defined (vendor) extensions **must** carry an `SQI_` prefix (e.g.
`SQI_PATH_TRANSLATION`). This guarantees a vendor name can never collide with a
future official OpenJD name. The rule is enforced by a registry invariant test
(`TestRegistryVendorNamespacing`).

If a vendor extension is contributed to and adopted by upstream OpenJD, it is
**promoted**: the `SQI_` prefix is dropped, the entry's `Origin` flips to
`official`, and its contribution doc moves to reflect the official name.

## Authoring a new extension

1. Add an `Extension` entry to the `registry` in `internal/openjd/extension.go`
   (vendor names prefixed `SQI_`).
2. Implement its parse/validate behavior in `internal/openjd` and any worker-side
   behavior under `internal/worker/`.
3. Add a contribution doc under `docs/openjd-extensions/<name>.md` using the
   template below.

### Contribution-doc template

```
# <EXTENSION_NAME>

- Origin: official | vendor
- Status: supported
- Summary: <one line>

## Motivation
Why the extension exists; what base-spec OpenJD cannot express.

## Schema
The template fields it adds or the directives it introduces, with types.

## Validation
The rules sqi enforces and the error messages/pointers produced.

## Worker behavior
What the worker does at runtime, with file references.
```

## Supported extensions

- [`TASK_CHUNKING`](openjd-extensions/task-chunking.md) — official.
- [`REDACTED_ENV_VARS`](openjd-extensions/redacted-env-vars.md) — official.
- [`SQI_PATH_TRANSLATION`](openjd-extensions/path-translation.md) — vendor.
- [`SQI_CHUNK_BOUNDS`](openjd-extensions/sqi-chunk-bounds.md) — vendor.
