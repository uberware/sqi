<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->

# SQI_CHUNK_BOUNDS

- Origin: vendor
- Status: supported
- Summary: Exposes a CHUNK[INT] chunk's first/last integer as Task.Param.<name>.Start/.End.

## Motivation
Some renderers take the start and end of a frame range as separate integer
arguments (e.g. `-s START -e END`). OpenJD's `CHUNK[INT]` parameter exposes a
chunk only as a single combined range string (`{{Task.Param.Frame}}` → `1-10`),
so those renderers otherwise need one task per frame or a wrapper script to split
the range. This extension derives the bounds directly, mirroring Smedge's
`$(SubRange.Start)` / `$(SubRange.End)`.

## Schema
Declare both extensions:

    extensions: [TASK_CHUNKING, SQI_CHUNK_BOUNDS]

When declared, every `CHUNK[INT]` task parameter named `X` gains two derived task
parameters per task, referenced as `{{Task.Param.X.Start}}` and
`{{Task.Param.X.End}}`. `Start` is the smallest integer in the chunk, `End` the
largest. For a single-frame chunk, `Start == End`.

Example:

    parameterSpace:
      taskParameterDefinitions:
        - name: Frame
          type: CHUNK[INT]
          range: "{{Param.Frames}}"
          chunks: { defaultTaskCount: 10, rangeConstraint: CONTIGUOUS }
    script:
      actions:
        onRun:
          command: render
          args: ["-s", "{{Task.Param.Frame.Start}}", "-e", "{{Task.Param.Frame.End}}"]

## Validation
- `SQI_CHUNK_BOUNDS` requires `TASK_CHUNKING` to also be declared (`/extensions`).
- Every `CHUNK[INT]` parameter must be `CONTIGUOUS` (the default). A
  `NONCONTIGUOUS` chunk is rejected, because `.Start`/`.End` are undefined across
  the gaps of a non-contiguous set.
See `internal/openjd/validate.go` (`validateChunkBounds`).

Note: `.Start`/`.End` describe the enclosing span of the chunk, so a stepped
range (e.g. `1-10:2` → frames 1,3,5,7,9) yields `Start=1`, `End=9`. A renderer
invoked with `-s 1 -e 9` will render the full contiguous span, including the
frames the step skipped — start/end renderers cannot express a step.

## Worker behavior
The derived keys are added to each task's parameters during submission
(`internal/openjd/expand.go` `DeriveChunkBounds`, wired in `submit.go`). They are
ordinary task parameters, resolved on the worker through the standard
`Task.Param.*` scope; no worker-side change is required.
