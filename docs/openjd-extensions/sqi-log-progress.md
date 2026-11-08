<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->

# SQI_LOG_PROGRESS

- Origin: vendor
- Status: supported
- Summary: Step-level log progress: report task progress and status text from log lines.

## Motivation
A task reports progress to sqi by printing `openjd_progress: <percent>` and
`openjd_status: <text>` lines. A preset that runs its own script inside an
application can print them; a preset that runs an application's command line
cannot, because the application decides what it prints. Most renderers and
encoders already say where they are (the frame they are on, a percentage, a
sample count), just not in OpenJD's words.

This extension lets a step say how to read those lines: patterns whose named
groups declare what a match means, and patterns that capture status text. The
worker already reads every output line of a task process for the log stream,
for `openjd_*` directives and for `SQI_LOG_CHECKS`, so the matching is a small
addition there, and a preset stays a single file.

## Schema
Declare the extension, and put a `SQI_LOG_PROGRESS` block on the step, as a
sibling of `script`:

    extensions: [TASK_CHUNKING, SQI_LOG_PROGRESS]
    steps:
      - name: Render
        parameterSpace:
          taskParameterDefinitions:
            - name: Frame
              type: CHUNK[INT]
              range: "1-100"
              chunks:
                defaultTaskCount: 10
                rangeConstraint: CONTIGUOUS
        SQI_LOG_PROGRESS:
          frameParameter: Frame
          progress:
            - '^Fra:(?P<frame>-?\d+) '
            - 'Sample (?P<framedone>\d+)/(?P<frametotal>\d+)'
          status:
            - '\| (?P<status>[^|]+)$'
        script:
          actions:
            onRun:
              command: render
              args: ["{{Param.SceneFile}}", "{{Task.Param.Frame}}"]

This is a **step-level** block, like `SQI_LOG_CHECKS`. A `SQI_LOG_PROGRESS` key
at the top level of the template, or anywhere inside a step other than its own
slot, is rejected whether or not the extension is declared.

- `progress`: a list of RE2 patterns. Each pattern's **named groups** say what
  a match reports (see [Progress forms](#progress-forms)).
- `status`: a list of RE2 patterns that set the task's status text.
- `frameParameter`: optional; the step's `INT` or `CHUNK[INT]` task parameter
  the frame forms are measured against.
- At least one of `progress` and `status` must hold a pattern.
- The patterns are **literal text, not format strings**: `{{ ... }}` in a
  pattern is regular-expression syntax, not a substitution. They are validated
  once at submission and cannot vary per task.
- Matching is **unanchored** and **case-sensitive**; use `(?i)` to fold case,
  and `^`/`$` to anchor.
- The block applies to the step's `onRun` action only, not to environment
  `onEnter`/`onExit` actions.

### Progress forms
A progress pattern must have exactly one of these sets of named groups (unnamed
groups are ignored). Here `index` is the frame's position in the task's own
frame list and `n` is that list's length.

| Groups | Meaning | Progress |
|---|---|---|
| `percent` | percent through the whole task | `percent` |
| `done`, `total` | counts through the whole task | `done / total × 100` |
| `frame` | the task has started this frame | `index / n × 100` |
| `framedone` | this frame has finished | `(index + 1) / n × 100` |
| `framepercent` | percent through the current frame | `(index + framepercent / 100) / n × 100` |
| `framedone`, `frametotal` | counts through the current frame | `(index + framedone / frametotal) / n × 100` |

- Progress is an integer percent, rounded down and capped at 100.
- A value that is not a finite number, a `percent` outside 0 to 100, a `total`
  of zero or less, or a `done` outside 0 to `total` is ignored, as is a `frame`
  or `framedone` that is not a whole number or not in the task's frame list.
- The two **within-frame** forms (`framepercent`, and `framedone` with
  `frametotal`) are measured from the last frame a `frame` or `framedone`
  pattern named, so the block must hold such a pattern. Until one has matched,
  a within-frame match is ignored.
- The patterns are tried in order, and the first that yields a valid value sets
  progress for the line.

### `frameParameter`
The frame forms need the task's frames. The worker reads them from the task's
value of the named parameter (`"5"`, `"1-10"`, `"1-19:2"`, `"1-3,7,8"` for a
chunk). When `frameParameter` is left out, the step's only `INT` or
`CHUNK[INT]` task parameter is used. It is an error when a frame form is used
and the step has no such parameter, or more than one and none is named, and
when `frameParameter` names a parameter that does not exist or is of another
type. The server resolves the name before it sends the assignment
(`AssignMsg.LogProgress.FrameParameter`), and sends it empty when no pattern
uses frames.

### Status text
A status pattern may name one group, `status`. The status text is that group,
or the whole line when the pattern has no group. An empty capture sets nothing,
and the next status pattern is tried. The text is cut to **200 bytes**, never
inside a UTF-8 character, and then cleaned as `openjd_status` text is: control
characters are dropped, tabs and line breaks become spaces, and surrounding
space is trimmed.

## Validation
Structural, enforced at submission (`internal/openjd/logprogress.go`,
`validateLogProgress`). Every error carries a JSON pointer.

- The extension and at least one block must both be present: a block on a step
  without the declaration is an error, and the declaration with no block on any
  step is an error (`/extensions`).
- The block holds only `frameParameter`, `progress` and `status`; any other
  field is an error. Each list must be a list of strings.
- The pattern limits are those of `SQI_LOG_CHECKS`: at most **32** patterns
  per list, **1024** bytes per pattern, no empty pattern, RE2 syntax, and a
  compiled program of at most **2000** instructions
  (`logpattern.MaxProgramInsts`). The template's `SQI_LOG_PROGRESS` patterns
  have their own **128,000**-instruction total, separate from
  `SQI_LOG_CHECKS`'s, so a step carrying both full blocks is valid.
- A progress pattern with any other set of named groups is an error that lists
  the accepted sets. A status pattern naming any group other than `status` is
  an error.
- A within-frame pattern in a block with no `frame` or `framedone` pattern is an
  error.
- `frameParameter` is checked as described [above](#frameparameter).

## Worker behavior
The scheduler copies the step's block into the task assignment
(`AssignMsg.LogProgress`, `internal/scheduler/assign.go`). The executor compiles
it with `logprogress.Compile` against the task's frame-parameter value
(`installLogProgress`, `internal/worker/executor/run.go`) and hands the matcher
to the OpenJD interceptor (`SetLogProgress`, `internal/worker/openjd/interceptor.go`)
before the process starts.

**Which lines.** The matcher sees every stdout and stderr line of the `onRun`
process that the interceptor forwards to the task log: not the `openjd_*`
directive lines it consumes, and never an `openjd_redacted_env:` directive line,
which carries its secret in the clear. Lines are matched **after redaction**
(`ScrubRedacted`), so status text never shows a redacted value. A line longer
than 64 KiB arrives in pieces, and each piece is matched on its own.

**Snapshots.** A match updates the attempt's progress and status, the same
values the directives set, and a change is published as a `running` snapshot
under the same throttle: at most one every **2 seconds**, with a trailing send
for the last change. A match that changes nothing sends nothing.

**Precedence.** Per field, an explicit directive wins. Once the attempt has
printed a valid `openjd_progress` line, the progress patterns no longer set
progress; once it has printed an `openjd_status` line, the status patterns no
longer set status. A script that knows better than the patterns is believed. The
flags last for the attempt; a retry starts afresh.

**With `SQI_LOG_CHECKS`.** The two blocks act independently on the same line: a
line can set progress and fail the task.

**A compile failure never fails the task.** A pattern that does not compile on
the worker, or frames that cannot be read (both impossible for a validated
template with a well-formed assignment), disable progress for that attempt with
one warning in the worker's diagnostic log (`executor: SQI_LOG_PROGRESS
disabled for this attempt`), and the task runs as if the step had no block.

### Not worker-enforced
Unlike `SQI_LOG_CHECKS`, this extension is registered with `StepBlock: true`
and is **not** worker-enforced, so the scheduler does not gate on it. Progress
is advisory: a worker that does not know `AssignMsg.LogProgress` (an additive
`omitempty` field) drops the key and runs the task correctly, with a bar that
moves only as tasks finish. Gating would make a preset's jobs wait for a worker
upgrade for the sake of a progress bar. `ProtocolVersion` is not bumped.

### Applications that redraw one line
The worker splits output on newlines. An application that redraws its progress
line in place with carriage returns (ffmpeg's default statistics, many
`\r`-driven progress bars) never ends a line, so the worker sees nothing until
the line finally ends, or a piece of 64 KiB fills. A preset for such an
application must ask it for line-per-update output instead (for ffmpeg,
`-progress pipe:1 -nostats`).

## Shipped presets
Every pattern is confirmed against the real application's output before it
ships.

| Preset | Forms used | Confirmed against |
|---|---|---|
| `blender-batch-render` | `frame` (`Rendering frame N`); `framedone` + `frametotal` (Cycles `Sample N/M`, tiled Cycles `Rendered N/M Tiles`, EEVEE `Rendering N / M samples`); status after the `Mem:` field, or after `Fra:` for EEVEE | Blender 5.2.2 (Cycles whole-frame and tiled, EEVEE); not Blender 4.x |
| `blender-view-layer-render` | the same block as `blender-batch-render` | Blender 5.2.2 (Cycles whole-frame and tiled, EEVEE); not Blender 4.x |
| `nuke-write-render` | `framedone` (`Frame N (i of n)`, printed once a frame is written); the same line as status | Nuke 17.1v2 Non-commercial on macOS; no other version or license |
| `nuke-script-render` | the same block as `nuke-write-render`; Nuke prints a `Writing ... took` line per Write node but `Frame N (i of n)` once per frame, after every node has written it, so the bar moves once per frame, not per node | Nuke 17.1v2 Non-commercial on macOS, rendering two Write nodes; no other version or license |
| `houdini-rop-render` | no block: the embedded hython driver prints `openjd_progress` and `openjd_status: rendered frame F (k of n)` itself, from one render-event callback on each `PostFrame` event | Houdini 21.0.631 Apprentice on macOS, Geometry ROP; not a render ROP (Mantra, Karma, OpenGL) |
| `keyshot-render`, `keyshot-studio-render` | no block: the embedded `render.py` prints `openjd_status: rendering frame F (k of n)` before each frame and `openjd_progress` (frames done / total x 100) after the frame's image is checked | the stub `lux` only; no KeyShot has run it |
| `modo-render`, `modo-pass-group-render` | no block: the embedded `render.py` prints one `openjd_status: rendering frames S–E` before the blocking render command and no `openjd_progress` | the stub `modo` only; no Modo has run it |
| `vred-render`, `vred-viewpoint-render` | no block: the embedded `render.py` prints one `openjd_status: rendering frames S–E` before the blocking `startRenderToFile` and no `openjd_progress` | the stub VRED only; no VRED has run it |
| `fusion-saver-render` | no block: the embedded `render.lua` prints one `openjd_status: rendering frames S–E` before the blocking `Render` and no `openjd_progress` | the stub Fusion only, under a stock Lua; no Fusion has run it |
| `unreal-mrq-job-render`, `unreal-mrq-queue-render` | no block: the embedded script prints `openjd_status: finished job NAME` and `openjd_progress` (jobs done / jobs x 100) as each job's `on_individual_job_work_finished_delegate` fires; a failed job reports none. Progress within a job (`MoviePipelineLibrary.get_completion_percentage`) is unconfirmed: no Unreal was available | the stub `unreal` module only; no Unreal has run it |
| `unreal-mrq-shot-render` | no block: the embedded script prints one `openjd_status: rendering SHOT` when the task's render starts and no `openjd_progress` | the stub `unreal` module only; no Unreal has run it |
| `husk-usd-render` | `percent` (`ALF_PROGRESS N%`, which `-V 2a`'s `a` turns on: the percent through the whole run, which is the task's chunk, moving within each frame); status is the image name and count from `>>> Render <image> (i of n)` | husk in Houdini 21.0.631 Apprentice on macOS, Karma CPU; not Karma XPU or another delegate, and not Houdini 22 |
| `usdrecord-render` | `frame` (`Recording time code: F.000000`, printed before each frame); the same line as status. usdrecord has no verbosity option and prints nothing after the last frame, so the bar reaches the last frame's start, not 100 | Houdini 21.0.631's usdrecord under hython on macOS, Storm and Karma CPU; not Houdini 22 |
| `oiiotool-process` | `framedone` (`Wrote frame N`); the same line as status. oiiotool prints nothing by default and `-v` names files, not frames, so the preset adds `--echo "Wrote frame {FRAME_NUMBER}"` after `-o`, which only prints | oiiotool 3.1.15.0 (Homebrew, macOS) and Houdini 21.0.631's hoiiotool (OIIO 2.5.18.0); not Ubuntu's openimageio-tools |

### Candidates, not shipped
These presets have no progress or status yet: the application is not available
to confirm its output, and a pattern is only shipped after it has been
confirmed against the real application.

| Preset | Status |
|---|---|
| `maya-layer-render`, `maya-scene-render` | application not available to confirm output |
| `arnold-kick-render`, `arnold-kick-tile-render` | application not available to confirm output |
| `after-effects-render`, `after-effects-queue-render` | application not available to confirm output |
| `vray-standalone-render`, `redshift-standalone-render`, `katana-render` | application not available to confirm output |
| `cinema4d-render`, `cinema4d-take-render`, `3dsmax-render` | application not available to confirm output |
| `mistika-boutique-render`, `mistika-vr-render`, `mistika-workflows-render` | application not available to confirm output |
| `silhouette-render`, `silhouette-render-all` | application not available to confirm output |
| `mantra-render` | not in Houdini 21; application not available to confirm output |
| `prman-render`, `3delight-render` | application not available to confirm output |

## Compatibility
`SQI_LOG_PROGRESS` is an sqi extension. Other OpenJD runtimes reject the
unknown extension name, as they do `SQI_LOG_CHECKS`, so a template that declares
it runs only on sqi.
