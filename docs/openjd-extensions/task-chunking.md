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
`1,10-12,18-50` at 10 is `1-1`, `10-12`, `18-26`, `27-34`, `35-42`, `43-50`
(the example in RFC 0001, balanced as described below). It is always spelled `<start>-<end>`, a single
frame included (`5-5`). A `NONCONTIGUOUS` chunk holds up to
`defaultTaskCount` values, gaps allowed, spelled as the compact
`<IntRangeExpr>` the reference implementation produces: a run of three or more
values at one positive step is a range (`1-5`, `1-9:2`), and anything shorter
is listed, so `1-8` at 3 is `1-3`, `4-6`, `7,8`. The TASK_CHUNKING conformance
fixtures pin this spelling (`test/conformance/chunkingjobs_test.go`). Values
keep sqi's first-seen expansion order, so a chunk of `10-15:2,1-5` can read
`10-14:2,1`; a descent is never written as a range.

`defaultTaskCount` is a maximum, not an exact size. Under either constraint,
each run (CONTIGUOUS) or the whole range (NONCONTIGUOUS) is cut into the
fewest chunks of at most `defaultTaskCount` values, `k = ceil(n /
defaultTaskCount)`, with the values spread evenly: chunk `i` ends after
`ceil((i+1)·n / k)` values. So `1-12` at 5 is `1-4`, `5-8`, `9-12`, and `1-10`
at 3 is `1-3`, `4-5`, `6-8`, `9-10`, never a short last chunk. The task count
is the same as filling each chunk to `defaultTaskCount` would give; only the
sizes even out, so the largest task is never larger, which shortens a job's
finish when workers are free. This is how the reference implementation chunks,
and the conformance fixture `contiguous-uneven.test.yaml` pins it. Tools that
fill chunks exactly and leave the remainder last (as RFC 0001's own "likely
division" does) give different boundaries.

A template that reads `{{Task.Param.<name>}}` as one frame number therefore
receives `5-5` at a chunk size of 1. A stepped range
yields one task per frame, since no two of its frames are consecutive; declare
`NONCONTIGUOUS` to keep a step in one chunk.

## Worker behavior
Chunked expansion is implemented in `internal/openjd/expand.go`; tasks carry the
chunk's range to the worker.
