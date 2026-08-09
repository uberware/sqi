<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->

# EXPR

- Origin: official
- Status: in-progress
- Summary: Python-subset expression language in format strings.

## Motivation

Base-spec OpenJD format strings (`{{Task.Param.Name}}`) only substitute a
symbol's text verbatim. EXPR (RFC 0005/0006) extends that to a small
Python-subset expression language: arithmetic, comparisons, string/list
operations, comprehensions, `let:` bindings, path manipulation
(`Session.WorkingDirectory / 'frames'`), path mapping
(`apply_path_mapping(...)`), and an ~80-function standard library, all
evaluatable inside `{{ ... }}`. A template declares it with
`extensions: [EXPR]`.

## Current status

**EXPR is registered but not yet accepted.** `internal/openjd/extension.go`'s
registry entry carries `Status: StatusInProgress`, and
`internal/openjd/validate.go` rejects `extensions: [EXPR]` at submission —
`TestConformance_EXPRNotSupported` fails the build the instant that flips.
The reason is section 1.2.2: the extension's own contract includes the
`BOOL`, `RANGE_EXPR` and `LIST[*]` job-parameter types, which are not
implemented yet (tracked separately), so accepting an EXPR template today
would ship a partial implementation the extension's own contract forbids.
Flipping the status to `StatusSupported` is a distinct, later unit of work
(sequenced in this program's tracker) — this document describes what is
built, not what is reachable from a submitted job today.

Despite that gate, most of the extension is implemented and tested:

- **Expression core** — the full type system, coercion, operators,
  comprehensions, call syntax — `internal/openjd/expr` (package doc:
  `internal/openjd/expr/doc.go`).
- **The ~100-function/property standard library** — conversions, math,
  string, regex, path, and `apply_path_mapping` — registered in
  `internal/openjd/expr`'s function tables.
- **Bounded evaluation** — per-`Eval` memory and operation limits
  (`internal/openjd/expr/limits.go`, `meter.go`), so an untrusted expression
  cannot exhaust server or worker resources.
- **The scope model and phase-2 checker** — `internal/openjd/scope.go` and
  `internal/openjd/exprcheck.go` type-check every format string and `let:`
  block against the template's declared parameters at submission time, so a
  template that would fail at runtime is rejected up front instead.
- **Phase-3 (worker-side) evaluation** — described below. This is what
  EXPR sub-project E4a added: evaluating expressions for real, against a
  real task's concrete values, on the worker that executes it.

## The wire protocol

The server → worker `AssignMsg` (`internal/worker/protocol/protocol.go`)
carries an envelope `Version` field, checked by the worker's lease loop
(`internal/worker/lease.decodeAssignment`) before the message is dispatched:
a version mismatch is rejected outright rather than silently decoded with
unrecognized fields dropped, so a partially upgraded farm fails loudly
instead of quietly running a task with the wrong command line.
`protocol.ProtocolVersion` is currently `"2"`; version `"2"` added the fields
EXPR phase-3 evaluation needs, all `omitempty` so a base-spec assignment's
wire bytes are byte-for-byte unchanged:

| Field | Purpose |
| --- | --- |
| `AssignMsg.EXPR` | Whether the template that produced this task declared the EXPR extension. Selects the worker's resolution path — see below. |
| `AssignMsg.JobName` / `StepName` | The template's own declared `Job.Name`/`Step.Name` (section 1.2.2 symbols), not derivable from any other field on the message. |
| `AssignMsg.ParameterTypes` / `JobParameterTypes` | Each task/job parameter's declared OpenJD type (`STRING`, `PATH`, `INT`, `FLOAT`, sqi's `CHUNK[INT]`), so `Task.Param.Count + 1` adds rather than concatenates — a type that must never be inferred from the value text, since `"1"` is a valid `STRING` parameter. |
| `AssignMsg.StepTemplateLet` / `StepScriptLet` | The step template's and step script's `let:` blocks (Template Schemas §3.6), as raw, unparsed `"name = expression"` source in declaration order, re-evaluated on the worker rather than shipped as resolved values — see below. |
| `AssignEnvironment.Let` / `StepEnvironment` | The same `let:` transport and scope tagging, per environment. |

**Only workers that build assignments through `internal/worker/protocol`
gain these fields**, and only for a template that declares EXPR — every
other message (`RegisterMsg`, `HeartbeatMsg`, `TaskStatusMsg`,
`LogChunkMsg`) is unaffected, and version enforcement is currently
**receiver-side on the worker only**: the server does not yet check
`Version` on the messages it receives.

## Worker behavior

### Choosing the resolution path

`msg.EXPR` selects between two resolution families at both places the
worker renders format strings — `internal/worker/executor/run.go`'s
`resolveAssignment` (task actions, embedded files, environment variables)
and `internal/worker/session/session.go`'s environment-entry resolution:

- `EXPR` false (the zero value, every base-spec assignment): the existing
  `fmtstring.Resolve`-backed plain-substitution path, unchanged byte for
  byte from before this extension existed.
- `EXPR` true: the phase-3 EXPR-aware evaluator described below.

### The phase-3 symbol table

`internal/worker/fmtres.TaskSymbols` builds the worker-side counterpart to
`internal/openjd/exprcheck.go`'s phase-2 symbol table, from one task
assignment: `Task.Param.*`/`Task.RawParam.*` and `Param.*`/`RawParam.*` are
bound as concrete `expr.Value`s (typed via `AssignMsg.ParameterTypes`/
`JobParameterTypes`, concretized from `AssignMsg.Parameters`/
`JobParameters`), `Session.WorkingDirectory` and the path-mapping symbols are
bound from the session, and `Task.File.*`/`Env.File.*` are bound from
materialized embedded-file paths. Every path value is bound under
`expr.PathNative` — the worker runs ON the host a task executes on, so a
Windows worker must evaluate its own native-flavored paths, not a
POSIX-flavored default.

`internal/worker/fmtres.ResolveActionExpr` /
`ResolveEmbeddedFilesExpr` / `ResolveVarsExpr` then render each format
string through `expr.Eval` against that table — including
`apply_path_mapping()`, threaded via `expr.WithPathMapping` from the
assignment's `PathMap` rules, and the same phase-2/phase-3 target-type
inheritance rule (section 1.3.2): a format string that is exactly one
reference evaluates against the *field's* own target type, so a path-typed
value coerces to text via the field's target; an embedded reference
evaluates unconstrained and stringifies its natural result.

Every phase-3 evaluation is metered independently of phase 2's submission-time
limits, via named constants in `internal/worker/fmtres`:
`workerOperationLimit` and `workerMemoryLimit`.

### `let:` re-evaluation

`AssignMsg.StepTemplateLet` and `StepScriptLet` (and each
`AssignEnvironment.Let`) ship as **raw, unparsed source**, not
pre-resolved values, and the worker re-evaluates them itself
(`internal/worker/session/session.go`'s let-binding path) in the same
order the template declares — step-template bindings first, then the step
script's, each new binding visible to the ones after it — building on
Template Schemas §3.6's ordering rule. A step template's own `let:` block
*could* be resolved server-side (its scope exposes only `Param.`,
`RawParam.`, `Job.Name` and `Step.Name` — all concrete by phase 2), but it
ships raw anyway: its bindings are the same table the step script's `let:`
block evaluates against, so splitting one ordered evaluation across two
machines would fragment it, and re-evaluating every block the same way on
the worker host keeps phase 3 "the same walk with a different table" rather
than a second mechanism.

### Phase-2/phase-3 agreement

Because phase 2 (`internal/openjd/exprcheck.go`, submission-time
type-checking) and phase 3 (this package, worker-side evaluation) build
their symbol tables independently from the same rules, sqi tests that they
**agree** — the same declared parameter type produces the same expression
type on both sides — for every job/task-parameter type EXPR supports
(`STRING`, `PATH`, `INT`, `FLOAT`, `CHUNK[INT]`, plus the derived
`Session.*`/`Task.File.*` path types). See
`internal/worker/fmtres/exprsyms_test.go`'s `TestPhase2Phase3Agreement` and
`internal/openjd/expr/paramtypes_internal_test.go`.

## Known gaps

- **No cumulative, template-wide budget.** Both phase 2 and phase 3 bound a
  single `Eval` call's memory and operation count, but nothing yet bounds
  the total across every expression a template or task evaluates. The
  worker has no `validateLetElementCounts`-equivalent to reject an
  over-cap `let:` block before evaluating it, unlike phase 2's checker.
  This is later work.
- **Only the worker's lease-loop receiver enforces `Version`.** The other
  worker→server message types decode without a version check today, so a
  version mismatch on those channels is silent. `heartbeat` and
  `registration` decode into local server-side structs with no `Version`
  field at all — a mismatch drops unrecognized fields regardless of
  version; `TaskStatusMsg` decodes into the real wire type but the
  consumer never reads `.Version`. Also later work.
- **`EXPR`'s registry `Status` stays `StatusInProgress`** until the gaps
  above (and the job-parameter types section 1.2.2 requires) are closed.
