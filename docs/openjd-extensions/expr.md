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
  individual evaluations and one task's or one environment's retained
  bindings — and, since EXPR sub-project E4c, a cumulative budget bounds the
  WHOLE template and the WHOLE assignment on top of them; see "Template-wide
  expression budget" below.
- **The scope model and phase-2 checker** — `internal/openjd/scope.go` and
  `internal/openjd/exprcheck.go` type-check every format string and `let:`
  block against the template's declared parameters at submission time, so a
  template that would fail at runtime is rejected up front instead.
- **Phase-3 (worker-side) evaluation** — described below. This is what
  EXPR sub-project E4a added: evaluating expressions for real, against a
  real task's concrete values, on the worker that executes it.
- **Task-parameter range field extensions (section 1.3.12)** — described in
  the next section. This is what EXPR sub-project E4b added: a submission-
  time (phase-2/resolve) feature, not a worker one, so it sits before the
  wire protocol below rather than inside "Worker behavior".
- **A template-wide, cumulative expression budget** — described in
  "Template-wide expression budget" below. This is what EXPR sub-project
  E4c added: closing the "Bounded evaluation" bullet's own gap, after three
  separate sub-projects (E2, E3, E4b) each independently found — and each
  fixed only locally — an unbounded cumulative cost none of the per-`Eval`
  bounds above ever reached.

## Task-parameter range field extensions (section 1.3.12)

Base-spec `range` (Template Schemas §3.4.1, `<TaskParameterDefinition>` and
its four type-specific variants) is either a literal list of values or, for
INT/CHUNK[INT] only, the succinct `<IntRangeExpr>` string syntax
(`"1-100:2"`). Section 1.3.12 of the expression language spec
(`wiki/2026-02-Expression-Language.md:1109`) extends what that field may
contain, per declared type — quoting its own table:

| Parameter Type | Original `range` Type | Extended `range` Type |
| --- | --- | --- |
| INT | `list[int \| FormatString] \| RangeString` | (unchanged, but see `RangeString` note below) |
| FLOAT | `list[Decimal \| FormatString]` | `list[Decimal \| FormatString] \| ListExpressionString` |
| STRING | `list[FormatString]` | `list[FormatString] \| ListExpressionString` |
| PATH | `list[FormatString]` | `list[FormatString] \| ListExpressionString` |

Two distinct shapes fall out of that table, handled by different code and
producing different results.

**The whole-field target, per declared type.** A whole-field `range` that is
*exactly one* `{{ ... }}` reference (`fmtstring.LoneRef` — no surrounding
text) is evaluated against the type section 1.3.12 gives that field:

| Declared type | Whole-field target |
| --- | --- |
| INT, CHUNK[INT] | `int \| string \| range_expr \| list[int]` (match-first) |
| FLOAT | `list[float]` |
| STRING | `list[string]` |
| PATH | `list[path]` |

**One function produces that target for both layers.** The checker
(`checkParameterSpaceExpressions`, `internal/openjd/exprcheck.go`) and the
resolver (`evalRangeExprField`, `internal/openjd/resolve.go`) both call
`rangeExprFieldType`, so their accept/reject verdicts *and* their rejection
messages are identical by construction. An earlier revision of this document
described two different `expr.Type` values "provably equivalent for every
accept/reject verdict"; they were equivalent only because both were equally
wrong — see the note below.

**FLOAT/STRING/PATH: `<ListExpressionString>`.** These three rows extend the
field with a format string containing an expression that evaluates to a
**list** — e.g. `range: "{{ [Param.Scale * 2, Param.Scale + 0.5] }}"` for a
FLOAT parameter (the spec's own worked example, and one of the three vendored
`expr1.3.11--*-range-expression.yaml` conformance fixtures — named for the
section's number *before* it was renumbered to 1.3.12, not a citation to a
section that no longer exists). Each resulting list element becomes one
`RangeList` entry, rendered to text with `Value.String()`, and `RangeExpr` is
cleared.

**INT/CHUNK[INT]: four members, two outcomes.** Only the INT row carries
section 1.3.12's `RangeString` note, because only INT/CHUNK[INT] ever had the
`<IntRangeExpr>` text form for an expression to be "**in addition to**". So
the whole-field result is dispatched on which member it landed in, and the
order is load-bearing:

- a **`list[int]`** result, or a **`range_expr`** result (e.g. from
  `range_expr(...)`, converted with section 1.2.3's own `range_expr →
  list[int]` rule via `expr.Coerce`, with no detour through range-expression
  text), becomes the range's **values** in `RangeList`, with `RangeExpr`
  cleared;
- an **`int`** or a **`string`** result is range **text**: it is rendered with
  `Value.String()` into `RangeExpr` and read by `parseIntRangeExpr` under
  `internal/openjd`'s own base-spec policy, exactly as a hand-typed
  `"1-100:2"` is. So `range: "{{Param.Frames}}"` with a `STRING` `Frames` of
  `"1-100:2"` yields the same 50 tasks with or without `extensions: [EXPR]`,
  and `range: "{{Param.N + 1}}"` with `N = 7` yields the single task `8`.

Recognising the two list-shaped members **before** the text fallback is what
keeps a lone `{{ range_expr(...) }}` out of `parseIntRangeExpr` — see "Why the
whole-field/embedded distinction matters" below.

> **Corrected during EXPR sub-project E4b's whole-branch review.** Both layers
> originally targeted only `range_expr | list[int]`, omitting `int` and
> `string`. Measured at that HEAD, `range: "{{Param.Frames}}"` with a `STRING`
> `Frames` was **rejected at template upload** — while the identical template
> *without* `extensions: [EXPR]` expanded correctly, so declaring the
> extension *removed* base-spec capability at this field. All six of this
> repo's reference render presets (`presets/sqi/*.yaml`) use that exact shape.
> The required target is stated verbatim by the conformance suite
> (`EXPR/jobs/expr1.2.3--union-target-type.test.yaml:12`), which exercises all
> four members.

**Symbol table: a step's own `let:` names are in scope here.** Section 3.6.2
row 1 makes the names a step template's `let:` block binds visible in
`parameterSpace`, and both layers build that table from the same shared pair
of helpers (`stepLetSymbols` and `rangeScopeSymbols`, `exprcheck.go`) — the
`ScopeJob` fixed and family symbols plus the step's let names, and nothing
else. `ResolveParameterSpaceParams` therefore takes the `*StepTemplate`
alongside the `*JobTemplate`. (Before the same review, only the checker saw
those names, so a step-level `let:` plus a range that referenced it validated
at upload and then failed at submit with `unknown symbol`.)

**Every other RangeList entry or RangeExpr shape — literal text, or a
`{{...}}` reference embedded in surrounding text — is not a whole-field list
expression.** For INT/CHUNK[INT], section 1.3.12 says the `RangeString`
field "is also extended" because, being a format string, it "can now contain
an expression that evaluates to either a range expression string or a
`list[int]`" — e.g. `range: "1-{{ Param.End }}"`. For every type, an
individual RangeList entry was already a format string in the base
specification — `<TaskParameterStringValue>` (Template Schemas §3.4.2) is
itself defined as a Format String, and every type's `range` list is built
from it (INT/FLOAT via `<IntRangeList>`/`<FloatRangeList>`, §3.4.1.1/§3.4.1.2;
STRING/PATH directly, §3.4.1.3/§3.4.1.4, `# @fmtstring`) — under EXPR, its
embedded reference is now evaluated as an expression (section 1.3.2's
general embedded-reference rule: unconstrained `expr.TAny`, rendered with
`Value.String()`) rather than looked up as a bare dotted identifier. Either
way, the result is **text**,
resolved by `resolve.go`'s `resolveRangeExprField`/`resolveRangeListEntry`
exactly as any other embedded reference resolves — never a value handed
directly to `evalRangeExprField`'s list-producing path.

**Why the whole-field/embedded distinction matters for INT/CHUNK[INT]
specifically:** `internal/openjd`'s own `<IntRangeExpr>` reader
(`parseIntRangeExpr`, via `internal/openjd/intrange`, configured with
`intrange.Policy{PositiveStepOnly, AscendingOnly}`) applies a policy that
deliberately diverges from the expression language's (`internal/openjd/expr`
passes the zero `Policy` and follows the spec) in three ways this repo
preserves on purpose — it rejects `start > end`, rejects a negative step, and
expands in first-seen rather than ascending order. A **lone** whole-field
range expression is consumed directly as a value and never re-enters that
reader, so it keeps the expression language's own semantics:
`range_expr("5-1")` resolves to the single value `["5"]` (section 3.4.1.1.1's
`x-y` formula, for `x > y`, reduces to the set `{x}` — the same rule that
makes `"1 - -1"` the single value `[1]` in the spec's own worked table),
where the same text as base-spec literal `<IntRangeExpr>` syntax is rejected
outright by `internal/openjd`'s own stricter `start > end` check, and
`range_expr("10-15:2,1-5")` comes out ascending — `[1,2,3,4,5,10,12,14]`, the
spec's own worked example for that text. An **embedded** occurrence
of the same call — e.g. `"{{ range_expr(\"10-15:2,1-5\") }},7"` — renders to
its own `<IntRangeExpr>` text (`Value.String()` on a `range_expr` value
round-trips the text it was constructed from, unmodified) and the *composed*
result is ordinary base-spec `<RangeString>` text, parsed by
`internal/openjd`'s own stricter/differently-ordered reader like any other
literal range string — so the identical `range_expr(...)` call can legally
produce different orderings, or accept-vs-reject outcomes, depending only on
whether it is the field's sole content. This is a deliberate ruling, not a
defect — see `resolveRangeExprField`'s own doc comment (`resolve.go`) for the
reasoning — and is pinned by
`TestResolveParameterSpaceParams_NonLoneRangeExprEmbeddedRangeExprValue`.

**What the checker does *not* judge, and where those errors surface instead.**
The checker judges an expression's **type**. It never judges the syntax,
length or value of the range text or range list that expression produces —
`checkFormatString` discards the evaluated value by design, and at phase 1 a
symbol-dependent expression has no value to inspect. Three shapes therefore
type-check and then fail at expansion, all three reported to the submitter as
a `SubmitValidationError` and none of them able to reach a task:

- a **non-lone** composition whose *composed* text is not valid range syntax
  (`range: ["x{{ 2.5 }}"]` → `invalid integer "x2.5"`);
- a **lone text-arm** result that is not valid `<IntRangeExpr>` syntax
  (`range: "{{ 'abc' }}"` → `invalid integer "abc"`);
- a **lone empty list** at any type (`range: "{{ [] }}"` → `range list is
  empty`, or `range produces no values` for CHUNK[INT]) — an empty list is
  perfectly well *typed*; its length is what is wrong, and length is not a
  type.

Each is base-spec-*rejecting*: the same text written as a literal `range` on a
non-EXPR template is refused too, so what differs is the layer and the JSON
pointer, never whether a malformed range is caught. The **message** matches only
for the lone-text arm (`{{ 'abc' }}` and a literal `abc` both give
`invalid integer "abc"`); the other two differ, and the difference was measured
rather than assumed:

| shape | base-spec literal | EXPR path |
|---|---|---|
| lone text `{{ 'abc' }}` | `range expression "abc": invalid integer "abc"` | identical |
| non-lone `["x{{ 2.5 }}"]` | `value "x2.5" is not a valid integer` (validate) | `invalid integer "x2.5"` (expand) |
| lone empty `{{ [] }}` | `required` (validate) — never reaches `range list is empty` | `range list is empty` (expand) |

An earlier revision of this paragraph claimed all three share a message. That
was true of one. All three are pinned by
`TestRangeCheckerResolverAgreement_KnownNonLoneDivergences`, whose doc comment
carries the full ruling.

**A computed FLOAT's rendered text is not always what a task receives.**
Section 1.3.4 ("Float Value Pass-Through",
`wiki/2026-02-Expression-Language.md:981`) gives computed floats their
shortest round-tripping decimal string — `evalRangeExprField` and
`resolveRangeListEntry` follow that rule via `Value.String()`, so
`Param.Scale * 2` with `Scale = 2.5` renders `"5.0"`. But for a FLOAT task
parameter, that text is not the final value: `internal/openjd/expand.go`'s
`validateFloatList` re-canonicalizes every FLOAT `RangeList` entry with
`strconv.FormatFloat(f, 'g', -1, 64)` before a task row is built, which drops
the trailing `.0` — a task actually receives `"5"`. This re-canonicalization
is pre-existing and not itself part of section 1.3.12 (it already applied to
a literal, non-EXPR FLOAT range entry); it is noted here only so `"5.0"` is
not mistaken for the value a job's command line will see.

**Conformance cannot see this feature.** The `EXPR/job_templates`
conformance suite (`make test-conformance`) only parses and validates a
template (`openjd.ValidateWithOptions`) — it never expands a parameter
space, so it cannot observe what a range resolves *to*, only whether the
template is accepted. The three fixtures for this feature
(`expr1.3.11--float-range-expression.yaml`,
`expr1.3.11--string-range-expression.yaml`,
`expr1.3.11--path-range-expression.yaml`) already passed before this support
existed. What actually exercises resolution and expansion end to end is
`internal/openjd/resolve_agreement_test.go`, which runs the real checker and
the real resolver+expander against the same templates and asserts they
agree on every verdict.

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

## Template-wide expression budget (EXPR sub-project E4c)

Sections 1.3.9 and 1.3.10 of the expression language spec
(`wiki/2026-02-Expression-Language.md:1060` and `:1071`) each bound a
*single* evaluation: "the evaluator tracks the memory size of live values"
and "maintains a running operation count," reset for every `Eval` call.
Every `Eval` in sqi already enforces both, independently, at every phase —
`submissionLimits()` (phases 1/2, `internal/openjd/exprcheck.go`) and
`workerOperationLimit`/`workerMemoryLimit` (phase 3,
`internal/worker/fmtres/expres.go`) — and phase 3 additionally bounds one
symbol table's own `let:` retention (`workerLetRetainedLimit`, 10 MB,
`internal/worker/fmtres/exprsyms.go`).

**None of that bounds the whole walk**, and the spec does not ask it to —
§1.3.9/§1.3.10 only obligate a per-evaluation bound. A template with
thousands of cheap positions, or many individually-compliant `let:` blocks,
paid no aggregate cost before this wave. Three sub-projects independently
found that gap and each fixed it only locally, never closing the underlying
pattern — the four constructions this wave closes together:

| Sub-project | Construction | Cost | What bounded it before E4c |
| --- | --- | --- | --- |
| E2 | an unmetered expression walk over a submitted template | ~9 minutes of server CPU per request | nothing — this is what forced the per-`Eval` limits to exist at all |
| E3 | a `let:` block whose reported 50-binding cap was never enforced | a 183 KB template body reached 6.9 GB in 1.45 s (`internal/worker/fmtres/exprsyms.go:464`) | nothing — fixed by `checkLetBindings`'s per-block cap, a DIFFERENT mechanism from this wave's budget |
| E4a | one worker symbol table's `let:` retention, unbounded across the table | 472 MB retained in one table | nothing — fixed by `workerLetRetainedLimit`, per-TABLE, not cumulative |
| E4b | 16 task-parameter definitions × 1024 `RangeList` entries, one step | 96 s in the resolver alone, with every per-`Eval` budget respected the entire time (`internal/openjd/exprcheck_budget_test.go:17`) | nothing — this wave |

Each prior fix bounded one *table* or one *position*; none summed across a
whole *walk*. E4c adds exactly that: a cumulative cap, charged once per
position and once per retained `let:` binding, summed across the entire
walk rather than reset per position or per table. It is an sqi-specific
mechanism layered on top of the spec's own per-evaluation bounds, enforced
independently at three separate points — because a budget shared across two
walks that charge the same positions silently halves the effective cap for
each (item 2, below) — never across phases (a fresh budget every call, so
phase 2's verdict never depends on what phase 1 already spent):

1. **Server, phases 1 and 2 — the checker.** `checkTemplateExpressions`'s own
   `templateBudget` (`internal/openjd/exprcheck.go`) caps:
   - `maxTemplateExprPositions` = **10,000** — one charge per
     format-string/`let:`-binding position actually walked.
   - `maxTemplateExprRetainedBytes` = **10,000,000** (10 MB) — the summed
     `expr.SizeOf` of every value a `let:` block adds to its table, the only
     construct in this walk that retains a value across positions.
     Deliberately equal to `workerLetRetainedLimit` (below), not a
     coincidence — the two are chosen to agree.
   - Fresh per call: `ValidateWithOptions` (phase 1, unresolved parameters)
     and `checkExpressionsAtSubmit` (phase 2, concrete parameters) each get
     their own allowance.

2. **Server, phase 2 — the resolver.** `ResolveParameterSpaceParams`
   (`internal/openjd/resolve.go`), threaded from `submit.go`'s
   `prepareTemplate`/`Submit`, spends its *own* `templateBudget`
   (`resolverBudget`), shared across every step's resolver call for one
   submission but deliberately **not** the checker's own budget
   (`checkerBudget`). The resolver re-charges the identical range positions
   and `let:` bytes the checker already charged for the same submission — a
   post-implementation review found that an earlier version shared one
   budget between the two, which silently halved the effective cap for
   those classes: a template `ValidateWithOptions` (phase 1) accepted could
   be rejected by `Submit` purely because phase 2's two walks were drawing
   from one 10,000-position/10 MB pool instead of two. Two independent
   budgets per phase-2 submission — one per walk — is the fix.

3. **Worker, phase 3 — the assignment.** `AssignmentBudget`
   (`internal/worker/fmtres/assignmentbudget.go`), one per *session*
   (created once in `session.Manager.Create`, before any environment is
   entered, and shared by the task's own symbol table and every
   environment's) caps:
   - `assignmentMaxPositions` = **5,000**.
   - `assignmentMaxRetainedBytes` = **20,000,000** (20 MB) — exactly 2×
     `workerLetRetainedLimit`, sized for the common shape of a task plus one
     environment near its own per-table ceiling.
   - **Charged only at environment ENTRY, never at teardown.**
     `ExitEnvironments`'s own re-evaluation of an environment's `let:` block
     (`session.go`'s `resolveEnvAction`) deliberately does not pass
     `s.exprBudget` to any of its three phase-3 calls. This is intentional,
     not an oversight: the table `resolveEnvAction` builds is rebuilt and
     discarded regardless of whether the charge is accepted — the
     allocation happens either way — and `ExitEnvironments` treats a
     resolve error as a warning and continues past it, so a budget that
     could still trip at teardown would silently **skip** that
     environment's `onExit` (license check-ins, daemon shutdowns, unmounts)
     rather than prevent any memory use. A budget that cannot avert the
     cost it is charging for must not be allowed to block cleanup it cannot
     prevent. Per-table (`workerLetRetainedLimit`) and per-`Eval`
     (`workerOperationLimit`/`workerMemoryLimit`) bounds still apply to the
     teardown table unconditionally — only the cross-table,
     assignment-wide ledger is exempt.

**What this budget does not measure, stated plainly:**

- **Operations are derived, not measured.** No operation counter crosses
  into `internal/openjd/expr` for either the template-wide or the
  assignment-wide budget — both count POSITIONS only. The server-side
  operation ceiling is a derived upper bound:
  `maxTemplateExprPositions × submissionOperationLimit` = 10,000 × 10,000 =
  **100,000,000** operations, per walk, per phase. Because this ceiling is a
  *product*, raising either factor raises it proportionally — an operator
  who later raises the position cap (or the per-position operation cap)
  also raises the operation ceiling by the same factor, without touching a
  single line of operation-counting code. This is not obvious from reading
  either constant in isolation and must be stated: nothing in this
  mechanism ever adds up to 100,000,000; it is a bound on what the existing
  per-`Eval` operation limit, applied 10,000 times, could in principle cost.
- **The byte dimension measures cumulative allocation, not peak live
  retention.** Both `maxTemplateExprRetainedBytes` and
  `assignmentMaxRetainedBytes` sum every `let:` block's own charge across
  the whole walk, in evaluation order — they do not model when Go's garbage
  collector might reclaim an earlier, now-unreferenced block's values. A
  template whose `let:` blocks never hold more than a few MB live *at any
  one instant*, but which declares many such blocks in sequence, is charged
  for their SUM and rejected once that sum crosses the limit, exactly as a
  template that held all of it live simultaneously would be — a
  construction that never holds more than, say, 2.7 MB live at once can
  still be charged 10.8 MB in total if it retains that much across four
  sequential `let:` blocks.
- **A single `let:` block can transiently exceed the server-side 10 MB
  figure before the budget ever sees it.** The charge lands once, after
  `checkLetBindings` finishes evaluating a whole block — not per binding
  within it — so one block can retain up to `maxLetBindings ×
  submissionMemoryLimit` = 50 × 1,000,000 = **50,000,000 bytes (50 MB)**
  before this counter rejects it. That is the true single-block ceiling
  this budget enforces, not 10 MB.

**`maxSteps` = 100** (`internal/openjd/validate.go`) caps the number of
`<StepTemplate>` entries *any* job template may declare — the one limit
this wave adds that applies to **every** template, including base-spec ones
declaring no `extensions:` at all, unlike every other bound on this page,
which engages only once a template declares `extensions: [EXPR]`. It is not
a spec transcription: OpenJD's own "Constraints" list for `<JobTemplate>`
gives `parameterDefinitions` an explicit min/max
(`wiki/2023-09-Template-Schemas.md:62`–`64`, item 6) but states none at all
for `steps` (`wiki/2023-09-Template-Schemas.md:71`, item 8) — 100 is an sqi
product decision, gated by `openjd.enforce_limits`
(`docs/configuration.md`) like the package's other bare structural-count
caps, not one of the package's always-on resource-exhaustion guards
(`maxRangeValues`, `maxTasksPerStep`). It also sizes the position budget's
own denominator: `maxTemplateExprPositions` was chosen against a worked,
per-step position count times `maxSteps` — see that constant's own doc
comment (`internal/openjd/exprcheck.go`) for the full arithmetic.

**What this wave deliberately does not reach.** Task *count* is a different
dimension from expression *cost*: these budgets bound what *expressions* may
cost to evaluate, not how many *tasks* a parameter space may expand to. A
template whose steps each declare a large `range` (e.g. `"1-1000000"`)
validates cheaply and quickly and then multiplies.

**At the production default (`enforce_limits: true`) this is already 10⁸
`CreateTask` inserts from one `POST`** — `maxSteps` (100) × `maxTasksPerStep`
(1,000,000) — and **no operator opt-out is required to reach it**. An
earlier revision of this page said the gap "requires a deliberate operator
opt-out (`enforce_limits: false`)" in the same sentence as its own
parenthetical conceding that `maxSteps` "does not close it even when
`enforce_limits` is true". Both cannot hold; the opt-out framing was the
wrong one and is withdrawn here.

What the opt-out changes is only how much *further* it goes: `maxSteps` is
gated by `openjd.enforce_limits` like the package's other bare
structural-count caps, so with `openjd.enforce_limits: false` the step count
is unbounded and the product has no ceiling at all. See `maxSteps`'s own
"RESIDUAL, PRE-EXISTING, NOT INTRODUCED HERE" paragraph
(`internal/openjd/validate.go`) for the exact mechanism.

The gap is pre-existing — it predates `maxSteps` — and is not attempted by
this wave. Closing it needs an always-on, catastrophically-generous bound on
**total tasks per job**, analogous to `maxTasksPerStep`
(`internal/openjd/expand.go`) but summed across steps — tracked as later
work.

## Known gaps

- **A cumulative, template-wide budget now exists** (EXPR sub-project E4c;
  see "Template-wide expression budget" above) — bounding what one
  submission's checker walk, one submission's resolver walk, and one
  worker's assignment may cost in total, on top of the per-`Eval` bounds
  this section used to describe as the only ones. What it still does not
  reach: the worker has no `validateLetElementCounts`-equivalent to
  *report* an over-cap `let:` block; it silently truncates at 50, relying
  on phase 2 having already rejected the template. Also open: an always-on,
  catastrophically-generous bound on total tasks per job (a distinct,
  task-count dimension — see "Template-wide expression budget" above).
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
