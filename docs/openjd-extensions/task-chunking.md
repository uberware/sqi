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

## Validation
- Declaring `CHUNK[INT]` without `TASK_CHUNKING` in `extensions` is rejected
  (`/steps/{i}/parameterSpace`).
- `TASK_CHUNKING` may be declared without using `CHUNK[INT]`.

See `internal/openjd/validate.go` (`validateExtensions`).

## Worker behavior
Chunked expansion is implemented in `internal/openjd/expand.go`; tasks carry the
chunk's range to the worker.
