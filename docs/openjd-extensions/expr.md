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
(`apply_path_mapping(...)`), and an 80-name standard library — 74 callable
functions plus the six path properties — all evaluatable inside `{{ ... }}`.
A template declares it with `extensions: [EXPR]`.

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
- **The standard library** — conversions, math, string, regex, path, and
  `apply_path_mapping`: **80 registry entries**, being 74 callable functions
  and the six path properties (`__property_name__` and friends), which are
  registered in the same table — `internal/openjd/expr`'s `functionShapes`,
  whose count `funcs_internal_test.go` pins at 80.
- **Bounded evaluation** — per-`Eval` memory and operation limits
  (`internal/openjd/expr/limits.go`, `meter.go`), plus a per-symbol-table
  retained-bytes bound on the worker's `let:` evaluator
  (`internal/worker/fmtres`'s `workerLetRetainedLimit`). These bound
  individual evaluations and one task's or environment's retained bindings;
  they are **not** a cumulative, template-wide budget — see Known gaps.
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
EXPR phase-3 evaluation needs. All nine are `omitempty`, so the **added
fields** leave a base-spec assignment's wire bytes byte-for-byte unchanged —
the envelope's own `"version":"1"` → `"2"` is of course changed by the bump
itself, which is what the worker's receiver-side check keys off:

| Field | Purpose |
| --- | --- |
| `AssignMsg.EXPR` | Whether the template that produced this task declared the EXPR extension. Selects the worker's resolution path — see below. |
| `AssignMsg.JobName` / `StepName` | The template's own declared `Job.Name`/`Step.Name` (section 1.2.2 symbols), not derivable from any other field on the message. |
| `AssignMsg.ParameterTypes` / `JobParameterTypes` | Each task/job parameter's declared OpenJD type (`STRING`, `PATH`, `INT`, `FLOAT`, sqi's `CHUNK[INT]`), so `Task.Param.Count + 1` adds rather than concatenates — a type that must never be inferred from the value text, since `"1"` is a valid `STRING` parameter. |
| `AssignMsg.StepTemplateLet` / `StepScriptLet` | The step template's and step script's `let:` blocks (Template Schemas §3.6), as raw, unparsed `"name = expression"` source in declaration order, re-evaluated on the worker rather than shipped as resolved values — see below. |
| `AssignEnvironment.Let` / `StepEnvironment` | The same `let:` transport and scope tagging, per environment. |

**The server builds every assignment** (`internal/scheduler/assign.go`), and
populates these fields only for a template that declares EXPR — every
other message (`RegisterMsg`, `HeartbeatMsg`, `TaskStatusMsg`,
`LogChunkMsg`) is unaffected, and version enforcement is currently
**receiver-side on the worker only**: the server does not yet check
`Version` on the messages it receives.

## Worker behavior

### Choosing the resolution path

`msg.EXPR` selects between two resolution families at both places the
worker renders format strings — `internal/worker/executor/run.go`'s
`resolveAssignment` (the task's `onRun` action and step embedded files; the
environment variables it returns are `sess.StaticEnv()`, already resolved at
enter time, on both branches) and `internal/worker/session/session.go`'s
environment-entry and environment-exit resolution:

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
bound from the session, and `Task.File.*` is bound from materialized
embedded-file paths. `Env.File.*` belongs to `EnvSymbols` alone — the
environment-scope builder, which conversely binds no `Task.*` family at all. Every path value is bound under
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
`workerOperationLimit` and `workerMemoryLimit`. Those are **per-`Eval`**
budgets, which is enough for every position that renders a result and drops
it — but not for `let:`, the one construct that *retains* values; see below.

### `let:` re-evaluation

`AssignMsg.StepTemplateLet` and `StepScriptLet` (and each
`AssignEnvironment.Let`) ship as **raw, unparsed source**, not
pre-resolved values, and the worker re-evaluates them itself
(`internal/worker/fmtres/exprsyms.go`'s `ApplyTaskLet`/`ApplyEnvLet`, applied
from `internal/worker/executor/run.go`'s `resolveAssignmentExpr` for the two
step-level blocks and from `session.go` for an environment's) in the same
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

A `let:` block is the only phase-3 position that keeps its results, so a
per-`Eval` budget does not bound it: 50 bindings each within
`workerMemoryLimit` retain 50 times it. `workerLetRetainedLimit` (10 MB)
bounds the total section 1.3.9 size **one symbol table** holds live, measured
across every block evaluated into it — the step template's and the step
script's for a task, the step template's and the environment's own for an
environment entry. A block that would push the table past it stops there with
an error rather than continuing to spend a fresh evaluation budget per
remaining binding. This is the worker-local bound only; the cumulative,
template-wide budget is still open (see Known gaps).

### Deliberate phase-2/phase-3 divergences

Three differences between the two phases are correct and intended. They are
recorded here so they are not re-derived as findings:

1. **`Session.PathMappingRulesFile` is bound conditionally at phase 3.** It
   appears only when the session actually has rules (`hasPathMap`), matching
   the pre-EXPR `addPathMappingKeys` rule, so a reference to it with no rules
   fails as an unknown symbol rather than resolving to an empty path.
2. **`PathPOSIX` at phase 2, `PathNative` at phase 3.** Expression-Language
   §1.2.1: "In TEMPLATE scope contexts, POSIX semantics are used for
   consistency. In host contexts (SESSION and TASK scopes), the semantics
   match the host's operating system." Phase 3 *is* the host context.
3. **Path mapping is applied to `Param.<PATH>`/`Task.Param.<PATH>` only at
   phase 3.** §1.2.2 requires `Param.<name>` to carry the value "with path
   mapping rules applied"; phase 2 has no session, and therefore no rules, to
   apply.

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
  single `Eval` call's memory and operation count, and phase 3 additionally
  bounds what one symbol table's `let:` bindings retain
  (`workerLetRetainedLimit`) — but nothing yet bounds the total across every
  expression a whole template or task evaluates, nor across the tables a
  worker holds concurrently. The worker also has no
  `validateLetElementCounts`-equivalent to *report* an over-cap `let:` block;
  it silently truncates at 50, relying on phase 2 having already rejected the
  template. This is later work.
- **Workers must be upgraded after the server, never before.** The worker's
  lease loop rejects any `AssignMsg` whose `Version` does not match its own,
  so a `"2"` worker talking to a `"1"` server rejects every assignment it is
  offered and the tasks churn through reclaim until the server is upgraded.
  The reverse order is the safe one: a `"1"` worker against a `"2"` server
  also rejects, but the server can still hand that work to upgraded workers.
  Nothing enforces or detects the ordering today.
- **Only the worker's lease-loop receiver enforces `Version`.** The other
  worker→server message types decode without a version check today, so a
  version mismatch on those channels is silent. `heartbeat` and
  `registration` decode into local server-side structs with no `Version`
  field at all — a mismatch drops unrecognized fields regardless of
  version; `TaskStatusMsg` decodes into the real wire type but the
  consumer never reads `.Version`. Also later work.
- **`EXPR`'s registry `Status` stays `StatusInProgress`** until the gaps
  above (and the job-parameter types section 1.2.2 requires) are closed.
