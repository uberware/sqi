<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->

# TASK_CHUNKING

- Origin: official
- Status: supported
- Summary: Chunked integer task parameters (CHUNK[INT]).

## Motivation
Lets a step expand an integer range into chunks rather than one task per value,
so large frame ranges become a manageable number of tasks.

## Schema
A task parameter may use the `CHUNK[INT]` type. The template must declare
`TASK_CHUNKING` in its top-level `extensions` list.

`chunks.defaultTaskCount` and `chunks.targetRuntimeSeconds` are each an
integer or a format string (`defaultTaskCount: "{{Param.ChunkSize}}"`),
resolved against the job's parameters when the job is created. The resolved
`defaultTaskCount` must be at least 1 and `targetRuntimeSeconds` at least 0;
`0` means "ignore and use `defaultTaskCount`". sqi parses
`targetRuntimeSeconds` but does not act on it, as the specification permits.

## Validation
- Declaring `CHUNK[INT]` without `TASK_CHUNKING` in `extensions` is rejected
  (`/steps/{i}/parameterSpace`).
- `TASK_CHUNKING` may be declared without using `CHUNK[INT]`.

See `internal/openjd/validate.go` (`validateExtensions`).

## Chunking
A `CONTIGUOUS` chunk never spans a gap in the range: the values are split
into runs of consecutive integers and each run is chunked separately, so
`1,10-12,18-50` at 10 is `1-1`, `10-12`, `18-27`, `28-37`, `38-47`, `48-50`
(RFC 0001's own example). It is always spelled `<start>-<end>`, a single
frame included (`5-5`). A `NONCONTIGUOUS` chunk is a comma-separated list of
up to `defaultTaskCount` values, gaps allowed.

## Worker behavior
Chunked expansion is implemented in `internal/openjd/expand.go`; tasks carry the
chunk's range to the worker.
