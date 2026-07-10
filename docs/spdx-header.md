# SPDX License Header Template

Add the SPDX license identifier at the top of every source file, **before** any `package`, `import`, or module declarations. Use the comment syntax for the file's language.

## Go (`.go`), TypeScript / JavaScript (`.ts`, `.tsx`, `.js`, `.jsx`)

```go
// SPDX-License-Identifier: AGPL-3.0-or-later
```

## YAML (`.yaml`, `.yml`), Python (`.py`), Shell (`.sh`), TOML (`.toml`)

```yaml
# SPDX-License-Identifier: AGPL-3.0-or-later
```

## Notes

- The single `SPDX-License-Identifier` line above is the required form and is what every source file in the repo carries.
- An optional copyright line may precede it — `SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>` — but it is not required and most files omit it.
- Use `AGPL-3.0-or-later` (not `AGPL-3.0-only`) so future relicensing to a newer AGPL version is permissible.
- If you add the copyright line, the year should reflect the year the file was first created, not last modified.
- Third-party vendored files retain their original SPDX headers; do not overwrite them.
