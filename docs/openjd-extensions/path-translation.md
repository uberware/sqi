<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
# SQI_PATH_TRANSLATION

- Origin: vendor
- Status: supported
- Summary: Per-product checklist of path-delivery mechanisms.

## Motivation
Path translation in sqi is otherwise automatic and all-at-once. This extension
lets a product/template author choose, per job, how concrete paths reach the
application — and adds command-flag, environment-variable, and local-staging
deliveries that base-spec OpenJD does not provide.

## Relationship to native OpenJD
- `translation_file` IS the native OpenJD `pathmapping-1.0` file plus the
  `{{Session.PathMappingRulesFile}}` format string. Prefer it (and the format
  string) whenever your app can read a mapping file.
- `command_flags` / `environment` deliver the mapping as individual `src`/`dest`
  pairs, which the native rules-file format string cannot express. Use them only
  for apps that need per-pair flags/env.
- `swap_in_place` (string substitution) and `stage_locally` are beyond the OpenJD
  spec — sqi conveniences for path-mapping-unaware apps and worker-local staging.

## Schema
Declare the extension and a top-level `SQI_PATH_TRANSLATION` block:

    extensions: [ SQI_PATH_TRANSLATION ]
    SQI_PATH_TRANSLATION:
      deliveries:
        - swap_in_place
        - translation_file
        - command_flags: { pattern: "--remap {src}={dest}" }
        - environment:   { variable: "PROJECT_ROOT" }
        - stage_locally

A delivery is a bare string (no settings) or a single-key map (with settings).

## Validation
- The block and the extension must both be present, or neither.
- `deliveries` must be non-empty when the extension is declared.
- `command_flags.pattern` must be non-empty and contain both `{src}` and `{dest}`.
- `environment.variable` must be non-empty.
- Unknown delivery names are rejected.

## Worker behavior
Deliveries run in fixed order: stage_locally → swap_in_place → translation_file →
command_flags → environment. Staging copies job-level PATH parameters
(`dataFlow` IN/INOUT) to worker-local scratch before the run and copies OUT/INOUT
back after, via `staging.sync_command` when the operator has configured one.
The other deliveries advertise the effective (post-staging) mappings.
`command_flags` appends `pattern` rendered per pair; `environment` sets
`variable` to `src=dest` pairs joined by the OS path-list separator. When the
extension is absent, the default is `swap_in_place` + `translation_file`
(today's behavior, unchanged).

`stage_locally` now works with **no worker configuration**: an unconfigured
worker falls back to a TEMP scratch directory
(`<os.TempDir()>/sqi-staging`) and sqi's own built-in copy in place of a shell
`sync_command`, logging a one-time WARN the first time it does so
(`staging.defaults`, on by default — see
[Worker configuration → `staging`](../worker-configuration.md#staging-local-path-staging-stage_locally-delivery)).
Set `staging.defaults: false` to restore the previous fail-hard behavior for
an unconfigured worker.

The built-in copy only moves bytes the worker can already reach (local disk,
or a filesystem already shared/mounted on that worker) — it is a local/dev
convenience, not a substitute for real remote transfer. `stage_locally` is
the primary mechanism for accessing S3-backed storage on ephemeral cloud
workers and, more generally, for a farm whose workers span multiple compute
locations: that setup needs an explicit `staging.sync_command`
(`rsync`/`aws-cli`/etc.), not the built-in copy fallback. See
[`docs/storage-s3.md`](../storage-s3.md) for setup instructions, per-provider
`sync_command` recipes (AWS, MinIO, R2, B2), and the mounted-vs-staged
decision guide.
