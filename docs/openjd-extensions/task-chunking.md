<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->

# TASK_CHUNKING

- Origin: official
- Status: supported
- Summary: Chunked integer task parameters (CHUNK[INT]).

## Motivation
Lets a step expand an integer range into chunks instead of one task per value,
so a large frame range becomes a manageable number of tasks.

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
`chunks.rangeConstraint` is required. A `CONTIGUOUS` chunk never spans a gap in
the range: the values are split into runs of consecutive integers and
each run is chunked separately, so RFC 0001's example, `1,10-12,18-50` at 10,
is `1-1`, `10-12`, `18-26`, `27-34`, `35-42`, `43-50`. A chunk is always
spelled `<start>-<end>`, a single frame included (`5-5`), so a template that
reads `{{Task.Param.<name>}}` as one frame number receives `5-5` at a chunk
size of 1. A stepped range yields one task per frame, since no two of its
frames are consecutive; declare `NONCONTIGUOUS` to keep such a step in fewer
chunks.

A `NONCONTIGUOUS` chunk may contain gaps and is spelled as the compact
`<IntRangeExpr>` the reference implementation produces: a run of three or more
values at one positive step is written as a range (`1-5`, `1-9:2`) and anything
shorter is listed, so `1-8` at 3 is `1-3`, `4-6`, `7,8`. Values keep sqi's
first-seen expansion order, so a chunk of `10-15:2,1-5` can read `10-14:2,1`;
a descending pair is never written as a range. The TASK_CHUNKING conformance
fixtures pin this spelling (`test/conformance/chunkingjobs_test.go`).

`defaultTaskCount` is a maximum. Each run (`CONTIGUOUS`) or the whole range
(`NONCONTIGUOUS`) of `n` values is cut into the fewest chunks that respect it,
`k = ceil(n / defaultTaskCount)`, with sizes balanced: chunk `i` ends after
`ceil((i+1)·n / k)` values. Chunk sizes differ by at most one, so no chunk is
much smaller than the rest: `1-12` at 5 is `1-4`, `5-8`, `9-12`, and `1-10` at
3 is `1-3`, `4-5`, `6-8`, `9-10`. This matches the reference implementation
and the conformance fixture `contiguous-uneven.test.yaml`. RFC 0001's own
illustration fills each chunk to `defaultTaskCount` and leaves the remainder
last, which gives the same number of chunks with different boundaries.

## Worker behavior
Chunked expansion is implemented in `internal/openjd/expand.go`. Each task
receives its chunk's spelling as the task parameter value. The
`SQI_CHUNK_BOUNDS` extension also exposes a `CONTIGUOUS` chunk's smallest and
largest value as `{{Task.Param.<name>.Start}}` and `.End`; see
[`sqi-chunk-bounds.md`](sqi-chunk-bounds.md).
