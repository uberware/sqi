# SPDX License Header Template

Add the following comment block at the top of every source file, **before** any `package`, `import`, or module declarations.

## Go (`.go`)

```go
// SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
// SPDX-License-Identifier: AGPL-3.0-or-later
```

## TypeScript / JavaScript (`.ts`, `.tsx`, `.js`, `.jsx`)

```ts
// SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
// SPDX-License-Identifier: AGPL-3.0-or-later
```

## YAML (`.yaml`, `.yml`)

```yaml
# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
```

## Python (`.py`)

```python
# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
```

## Shell (`.sh`)

```sh
# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
```

## Notes

- Use `AGPL-3.0-or-later` (not `AGPL-3.0-only`) so future relicensing to a newer AGPL version is permissible.
- The copyright year should reflect the year the file was first created, not last modified.
- Third-party vendored files retain their original SPDX headers; do not overwrite them.
- `golangci-lint` can enforce header presence via the `goheader` linter — wire this up in `.golangci.yml`.
