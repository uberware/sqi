<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->

# SQI_LOG_CHECKS

- Origin: vendor
- Status: supported
- Summary: Step-level log checks: fail a task on a log pattern or a missing success line.

## Motivation
OpenJD decides whether an action succeeded from its exit status alone. Some
renderers and DCC applications exit 0 after printing that the render failed, and
an application that quits early, before an asynchronous render finishes (an
Unreal editor, a VRED that lost its licence), also exits 0 without having
produced anything. A farm that believes the exit status reports a finished job
with no frames.

This extension lets a step say what a failure looks like in the process's own
output: a pattern that must never appear (`failOn`) and a pattern that must
appear at least once (`requireLine`). The worker already reads every output line
of a task process to forward logs and to catch `openjd_fail`, so the check is a
small addition there, and one shared checker keeps every preset a single file.

## Schema
Declare the extension, and put a `SQI_LOG_CHECKS` block on the step, as a sibling
of `script`:

    extensions: [SQI_LOG_CHECKS]
    steps:
      - name: Render
        SQI_LOG_CHECKS:
          failOn:
            - 'ERROR +\|'
          requireLine:
            - 'SQI_RENDER: OK'
        script:
          actions:
            onRun:
              command: render
              args: ["{{Param.SceneFile}}"]

This is a **step-level** block. `SQI_PATH_TRANSLATION`, by contrast, is a
top-level block. A `SQI_LOG_CHECKS` key at the top level of the template is
rejected whether or not the extension is declared.

- `failOn`: a list of RE2 patterns. If any stdout or stderr line of the step's
  `onRun` process matches any of them, the task fails.
- `requireLine`: a list of RE2 patterns. Each pattern must match at least one
  line of the process's output, or the task fails.
- At least one of the two lists must be present and non-empty.
- The patterns are **literal text, not format strings**: `{{ ... }}` in a pattern
  is regular-expression syntax, not a substitution. They are validated once at
  submission and cannot vary per task.
- Matching is **unanchored** (a pattern matches anywhere in the line) and
  **case-sensitive**; use `(?i)` to fold case, and `^`/`$` to anchor.
- The block applies to the step's `onRun` action only, not to environment
  `onEnter`/`onExit` actions.

## Validation
Structural, enforced at submission (`internal/openjd/logchecks.go`,
`validateLogChecks`). Every error carries a JSON pointer.

- The extension and at least one block must both be present: a block on a step
  without the declaration is an error (`/steps/N/SQI_LOG_CHECKS`: "block requires
  declaring the SQI_LOG_CHECKS extension in extensions"), and the declaration with
  no block on any step is an error (`/extensions`).
- A step key starting `SQI_` must name a registered, supported extension that is
  defined as a step-level block and that the template declares. Anything else
  (for example a misspelt `SQI_LOG_CHECK`) is an error naming the key, because
  the step decoder would otherwise drop it and the step would run with its checks
  off. Other unknown, non-`SQI_` step keys are still silently dropped, as they
  were before this extension; that is a pre-existing divergence, not changed here.
- The block holds only `failOn` and `requireLine`; any other field is an error.
- Each list must be a list of strings (an unquoted YAML `404` is rejected as
  "must be a string" rather than coerced).
- A list holds at most **32** patterns; a pattern is at most **1024** bytes; an
  empty pattern is rejected (an empty `failOn` pattern would fail every task).
- Every pattern must compile as RE2 (Go `regexp`). RE2 never backtracks, so
  matching is linear in the length of the line, but the cost per byte also
  scales with the size of the compiled program, and a short pattern can compile
  to a huge one: Go bounds nested repetition but not concatenation, so the
  905-byte `.{0,999}` repeated 113 times, followed by `x`, compiles to about
  226,000 instructions and takes seconds to match a single 4 KiB line. The
  compiled program is therefore capped at **2000** instructions
  (`logpattern.MaxProgramInsts`, `internal/openjd/logpattern`); realistic
  patterns compile to 10 to 100. A larger pattern is rejected at its own pointer
  (`/steps/N/SQI_LOG_CHECKS/failOn/I`): "pattern compiles to <n> instructions; at
  most 2000 are allowed". The worker enforces the same bound from the same
  constant when it builds the checker.

## Worker behavior
The scheduler copies the step's block into the task assignment
(`AssignMsg.LogChecks`, `internal/scheduler/assign.go`); the worker enforces it in
`internal/worker/executor/logchecks.go`, wired into `runTask`.

**Which lines.** A checker wraps the output handler that `scanOutput` feeds, so it
sees every stdout and stderr line of the `onRun` process. Lines are matched
**after redaction** (`ScrubRedacted`), the same text the task log shows, so a
failure reason never leaks a redacted environment value. The
`openjd_redacted_env:` directive line itself is never scanned, because it carries
the secret in the clear before the value is registered for scrubbing.

**Pieces of a long line.** The worker delivers a line longer than 64 KiB in
pieces (P5b-0). Each piece is checked on its own, as if it were a line, so a
pattern that would match across a split point does not match, and a marker that
a split cuts in two is not seen.

**`failOn` does not kill the process.** Applications print error lines and carry
on cleaning up, and killing early could leave partial outputs worse than letting
the process finish. The first matching line is remembered and the task is failed
once the process exits. A process that hangs is the step timeout's job.

**Outcome order.** The executor's existing decisions are unchanged and come
first: a process that could not start, an `openjd_fail`, a cancel, a worker
shutdown, a timeout, and a non-zero exit all decide the task exactly as before,
even when the output also matched a `failOn` pattern. Only a task that would
otherwise have **succeeded** is then judged by the checks: the first `failOn`
match wins, then the first `requireLine` pattern no line matched. A non-zero
exit still fails the task even when every check passed. A task failed by the
checks stages no outputs.

**Failure reasons.** The reason lands in the task's `failure_reason` and the task
log, in exactly these forms:

    log line matched failOn pattern "<pattern>": "<line>"
    the task never printed a line matching requireLine pattern "<pattern>"

The quoted line is cut at 200 bytes (never inside a UTF-8 character) and a cut is
marked with an ellipsis. A pattern that fails to compile on the worker, or
compiles to more than 2000 instructions (neither can happen for a validated
template), fails the task before it starts, with
`invalid SQI_LOG_CHECKS pattern: <error>`.

### Scheduling gate
A worker built before this extension would run a checked task and report its exit
status alone, which is exactly the outcome the author declared the extension to
prevent. So a job that declares `SQI_LOG_CHECKS` is only ever given to a worker
that enforces it:

- A worker advertises the worker-enforced extensions it supports in its
  registration (`RegisterMsg.Extensions`); the server persists them on the worker
  row (migration `00039`, `workers.extensions`).
- The scheduler's lease gate (`internal/scheduler/extcaps.go`,
  `workerExtensionBlock`) withholds a job that declares a worker-enforced
  extension from a worker that does not list it.
- When no online worker enforces it, the unschedulable sweep writes the reason
  onto the task: `no eligible online worker: worker does not enforce
  SQI_LOG_CHECKS; upgrade sqi-worker`. The sweep only runs when
  `scheduler.unschedulable_grace` is greater than zero; with it disabled the job
  waits silently.
- A job row that records no declared-extension list (written before migration
  `00027`) is not gated: the extension did not exist then.

`ProtocolVersion` is not bumped: `AssignMsg.LogChecks` and
`RegisterMsg.Extensions` are additive `omitempty` fields, and the gate keeps
checked jobs away from workers that do not know them.

### Host scripts: the success markers
Presets that render by running an embedded script inside the application (Unreal,
VRED, KeyShot, Modo) report the script's own outcome with two fixed markers:

- `SQI_RENDER: OK`, the last thing the script does, after the render call
  returned without error and, where it can check, the outputs exist;
- `SQI_RENDER: FAILED: <reason>` on a caught error.

Presets pair them with `requireLine: ['SQI_RENDER: OK']` and
`failOn: ['SQI_RENDER: FAILED:']`; the matched `FAILED` line, with its reason,
becomes the task's failure reason, and a missing `OK` (the application quit before
rendering, for example after a licence failure) fails the task.

An application's Python `print` does not reliably reach the process's stdout as a
bare line (Unreal routes it through its log as `[...][  0]LogPython: ...`), so a
script writes each marker straight to file descriptor 1 with `os.write(1, ...)`,
bypassing the host's `sys.stdout` redirection. The patterns are nevertheless
matched **unanchored**, so a host that prefixes even descriptor-1 output still
matches.

## Compatibility
`SQI_LOG_CHECKS` is an sqi extension. Other OpenJD runtimes reject the unknown
extension name, as they do `SQI_CHUNK_BOUNDS`, so a template that declares it
runs only on sqi.
