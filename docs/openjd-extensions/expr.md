<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->

# EXPR

- Origin: official
- Status: supported
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

## Worked examples

`presets/sqi/ffmpeg-segment-transcode-*.yaml` are the shipped worked
examples of this extension: three variants of the same segmented ffmpeg
transcode, all three of which declare `extensions: [EXPR]`
(`presets/sqi/ffmpeg-sequence-encode.yaml` declares it too, more modestly).
One, `ffmpeg-segment-transcode-expr` ("Portable"), builds its ffmpeg concat
file list from an EXPR comprehension instead of a join-time shell script, so it
needs no shell on the worker at all — not even `bash`, which the bash-joined
variant needs but is likewise not gated to any one OS. That
list-building expression references only job parameters, so it is fully
resolved at **submission** (phase 2, not phase 3) and its cost is charged
against the *server's* [`openjd.expr_operation_limit`](../configuration.md#openjdexpr_operation_limit)
(default 10,000) — the worker's far roomier `expr.operation_limit` never
enters into it. Measured cost is linear at ~24.15 operations per slice, so
the preset documents a ceiling of 400 slices under stock server limits; see
that file's `description`, [`docs/preset-library.md`](../preset-library.md#transcoding-reference-presets),
and `internal/product/exprpresetcost_test.go`, which pins the ceiling.

## Current status

**EXPR is a supported extension.** `internal/openjd/extension.go`'s registry
entry carries `Status: StatusSupported`, so `internal/openjd/validate.go`
accepts `extensions: [EXPR]` and every EXPR template is submittable through
`POST /api/v1/jobs`, judged on its own merits like any other template. EXPR
jobs are dispatched to workers and executed, and `test/integration` and
`scripts/smoke.sh` each run one through the real binaries end to end,
asserting on a value the **worker** resolves at phase 3.

Conformance against the vendored fixtures, on the ordinary template path that
scores every other suite: **`EXPR/job_templates` 206/209 with 3 baselined**,
alongside `base` 449/450 and `TASK_CHUNKING` 11/11. The three `EXPR`
shortfalls are not EXPR shortfalls — see [Known gaps](#known-gaps).

The extension is implemented and tested as follows:

- **Expression core** — the full type system, coercion, operators,
  comprehensions, call syntax — `internal/openjd/expr` (package doc:
  `internal/openjd/expr/doc.go`).
- **The standard library** — conversions, math, string, regex, path, and
  `apply_path_mapping`: **80 registry entries**, being 74 callable functions
  and the six path properties (`__property_name__` and friends), which are
  registered in the same table — `internal/openjd/expr`'s `functionShapes`,
  whose count `funcs_internal_test.go` pins at 80.
- **`apply_path_mapping` is host-context-only.** It is registered flat in
  `functionShapes` — the evaluator itself imposes no scope restriction — and
  the rule is enforced one layer up, by the phase-2 checker: `hostOnlyFunctions`
  (`internal/openjd/exprcheck.go`) walks `Expression.CalledFunctions()` at every
  position whose `Scope.IsHostContext()` is false and rejects the call there.
  `IsHostContext` is a positive list of exactly three scopes —
  `ScopeJobEnvironment`, `ScopeStepEnvironment`, `ScopeStepScript`
  (`internal/openjd/scope.go`) — so the function is refused in the job `name`
  field, in `hostRequirements`, in `parameterSpace` and in a
  `<StepTemplate>.let` block, all of which are evaluated at submission before
  any session exists. The list is positive rather than a negation so that a
  scope added later fails closed.
- **Bounded evaluation** — per-`Eval` memory and operation limits
  (`internal/openjd/expr/limits.go`, `meter.go`), plus a per-symbol-table
  retained-bytes bound on the worker's `let:` evaluator
  (`internal/worker/fmtres`, configured by `expr.let_retained_bytes`). These
  bound individual evaluations and one task's or one environment's retained
  bindings; a cumulative budget bounds the whole template and the whole
  assignment on top of them (see "Template-wide expression budget" below). Two
  further bounds cover what no counter can see: a **wall-clock deadline**
  (`openjd.expr_submission_deadline`, default 5s, checked inside
  `meter.charge`), because §1.3.10 prices 256 bytes at one operation and so
  an operation budget cannot bound seconds; and **`maxTasksPerJob`**
  (`internal/openjd/expand.go`), an always-on cap on the tasks one job may
  produce summed across steps. A deadline breach is a **503**, never a 4xx —
  it is not a verdict on the template. The only bound that applies *before*
  evaluation is **`maxSourceBytes`** (`internal/openjd/expr/limits.go`), an
  always-on cap of **10,000 bytes on the source of one expression**, checked
  at the top of `Parse` before the source is tokenized. Everything else above
  is metered from inside an evaluation, so without it a single 4 MB expression
  parses successfully — measured at 427.6 MB of live heap and 544 ms, with
  every configured budget and even an already-expired deadline untouched. It
  is not configurable, and an oversized expression is a **422**: too long is a
  deterministic property of the template.
- **Operator configuration of all nine limits** — every one of the bounds
  above is a config key rather than a compiled constant, on both binaries,
  with a validated range and a cross-binary dispatch gate; see "Operator
  configuration" below. The submission deadline is a tenth key on the same
  pattern, and `maxTasksPerJob` is a compiled constant, like its per-step
  sibling.
- **The scope model and phase-2 checker** — `internal/openjd/scope.go` and
  `internal/openjd/exprcheck.go` type-check every format string and `let:`
  block against the template's declared parameters at submission time, so a
  template that would fail at runtime is rejected up front instead.
- **Phase-3 (worker-side) evaluation** — described below: expressions are
  evaluated against a real task's concrete values, on the worker that
  executes it.
- **Task-parameter range field extensions (section 1.3.12)** — described in
  the next section. This is a submission-time (phase-2/resolve) feature, not
  a worker one, so it sits before the wire protocol below rather than inside
  "Worker behavior".
- **A template-wide, cumulative expression budget** — described in
  "Template-wide expression budget" below. It bounds the cumulative cost that
  none of the per-`Eval` bounds above reach.

## Extended parameter types

RFC 0007 extends the job-parameter type system, and sqi implements it.

**Case-insensitive type names.** Every type name, job and task, including
compound ones: `int`, `Path`, `List[Int]`, `list[list[int]]`. Gated on
`extensions: [EXPR]` — a base-spec template writing `type: int` is rejected,
because RFC 0007 is an extension specification and accepting lowercase
unconditionally would widen what the base spec admits.

**Eight new job-parameter types.**

| Type | Default | Notes |
| --- | --- | --- |
| `BOOL` | `true`, `1`, `1.0`, `"yes"`, `"on"` and negatives | `allowedValues` is **rejected**: restricting a two-valued type to one value constrains nothing (the RFC says so explicitly). Its control is `CHECK_BOX`, without the base spec's two-`allowedValues` requirement. |
| `RANGE_EXPR` | an `<IntRangeExpr>` string | `minLength`/`maxLength` bound the expression's **string length**, default max 1024. |
| `LIST[STRING]`, `LIST[PATH]`, `LIST[INT]`, `LIST[FLOAT]`, `LIST[BOOL]` | a YAML sequence | Stored as canonical JSON in `JobParameter.Default`. `LIST[PATH]` also carries `objectType`, `dataFlow`, `fileFilters` and `fileFilterDefault`. |
| `LIST[LIST[INT]]` | a sequence of sequences | One level of nesting, the RFC's own limit. No control but `HIDDEN` — its use case is programmatic. |

Element constraints use a nested `item:` block mirroring the type nesting, so
each level reuses the scalar names (`minLength`/`maxLength` for string-like,
`minValue`/`maxValue` for numeric). Element type checking is by JSON **type**,
which is what the RFC's `<string>` / `<integer>` schemas constrain — `LIST[BOOL]`
is the deliberate exception, because the RFC gives its items the same
accepted-values table as a scalar `BOOL`.

**`RANGE_EXPR` uses the specification's permissive range policy**, not
`internal/openjd`'s stricter one. The two differ on purpose: `internal/openjd`
rejects `start > end` and a negative step for literal base-spec range text. A
`RANGE_EXPR` **parameter** does not, because the
conformance fixture declares `"10-1:-1"` and `"-1--10:-1"`, and because a lone
whole-field `range: "{{Param.FrameRange}}"` evaluates as a *value* through
`range_expr`'s own coercion — which uses the permissive parse anyway. Literal
range text keeps the strict policy.

**The `<ArgString>` control-character amendment.** When `EXPR` is declared, CR,
LF and TAB become legal anywhere in an argument, "to support multi-line
expressions in YAML literal block scalars" (Expression Language, "When EXPR is
enabled", item 3). Every other Cc character is still rejected, and
`<CommandString>` — a separate type in the same schema (Template Schemas §5.1
and §5.2) — is **not** amended, so a command keeps the base-spec rule.

**Both evaluation phases resolve the new types.** `expr.JobParamTypes` maps
them and `expr.ValueFromText` decodes their values. That function lives in the
leaf package both phases import, so phase 2 and phase 3 resolve the types the
same way — `TestPhase2Phase3Agreement` covers all eight.

**List values at run time.** A `LIST[*]` value is canonical JSON
(`internal/openjd/paramjson.go`), held in the same `map[string]string` as
scalar values, which matters in two runtime paths:

- **`loc://` resolution is per element.** `resolveLocURIsInParamValue`
  (`internal/scheduler/assign.go`) decodes the list, resolves `loc://` element
  by element, and re-encodes through the same codec that produced it.
  Whole-string substring replacement would splice the resolved path *inside*
  a JSON string literal, which survives for a POSIX path but corrupts a
  Windows one (backslashes are not legal JSON escapes) and leaves the value
  undecodable.
- **`LIST[PATH]` elements are path-mapped.** `paramValueForBinding`
  (`internal/worker/fmtres/exprsyms.go`) runs each element of a `LIST[PATH]`
  job parameter through `mapListPathParamValue`, which applies the same
  `mapPathParamValue` the scalar case uses, as RFC 0007 requires:
  `Param.<name>` for a list of paths "Returns a `list[path]` type value with
  path mapping applied."

**Web form.** `web/src/api/types.ts` mirrors the eight RFC 0007 types and the
six `*_LIST` `userInterface` controls, and the product form renders each list
type with a `list` widget — see `docs/web-development.md`'s "Product
parameter widgets" section.

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
messages are identical by construction.

**FLOAT/STRING/PATH: `<ListExpressionString>`.** These three rows extend the
field with a format string containing an expression that evaluates to a
**list** — e.g. `range: "{{ [Param.Scale * 2, Param.Scale + 0.5] }}"` for a
FLOAT parameter (the spec's own worked example, and one of the three vendored
`expr1.3.11--*-range-expression.yaml` conformance fixtures, named for the
section's earlier number). Each resulting list element becomes one
`RangeList` entry, rendered to text with `Value.String()`, and `RangeExpr` is
cleared.

**INT/CHUNK[INT]: four members, two outcomes.** Only the INT row carries
section 1.3.12's `RangeString` note, because only INT/CHUNK[INT] ever had the
`<IntRangeExpr>` text form for an expression to be "**in addition to**". So
the whole-field result is dispatched on which member it landed in, and the
order matters:

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

> **Why `int` and `string` are in the target.** With a target of only
> `range_expr | list[int]`, `range: "{{Param.Frames}}"` with a `STRING`
> `Frames` would be **rejected at template upload** while the identical
> template *without* `extensions: [EXPR]` expands correctly, so declaring the
> extension would remove base-spec capability at this field. All six of this
> repo's reference render presets (`presets/sqi/*.yaml`) use that shape. The
> required target is stated verbatim by the conformance suite
> (`EXPR/jobs/expr1.2.3--union-target-type.test.yaml:12`), which exercises all
> four members.

**Symbol table: a step's own `let:` names are in scope here.** Section 3.6.2
row 1 makes the names a step template's `let:` block binds visible in
`parameterSpace`, and both layers build that table from the same shared pair
of helpers (`stepLetSymbols` and `rangeScopeSymbols`, `exprcheck.go`) — the
`ScopeJob` fixed and family symbols plus the step's let names, and nothing
else. `ResolveParameterSpaceParams` therefore takes the `*StepTemplate`
alongside the `*JobTemplate`. (If only the checker saw those names, a
step-level `let:` plus a range that referenced it would validate at upload and
then fail at submit with `unknown symbol`.)

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
whether it is the field's sole content. This is intended — see
`resolveRangeExprField`'s own doc comment (`resolve.go`) for the reasoning —
and is pinned by
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
`invalid integer "abc"`); the other two differ, as measured:

| shape | base-spec literal | EXPR path |
|---|---|---|
| lone text `{{ 'abc' }}` | `range expression "abc": invalid integer "abc"` | identical |
| non-lone `["x{{ 2.5 }}"]` | `value "x2.5" is not a valid integer` (validate) | `invalid integer "x2.5"` (expand) |
| lone empty `{{ [] }}` | `required` (validate) — never reaches `range list is empty` | `range list is empty` (expand) |

All three are pinned by
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
the trailing `.0` — a task receives `"5"`. This re-canonicalization is not
part of section 1.3.12 (it applies to a literal, non-EXPR FLOAT range entry
too); it is noted here so `"5.0"` is not mistaken for the value a job's
command line will see.

**Conformance cannot see this feature.** The `EXPR/job_templates`
conformance suite (`make test-conformance`) only parses and validates a
template (`openjd.ValidateWithOptions`) — it never expands a parameter
space, so it cannot observe what a range resolves *to*, only whether the
template is accepted. The three fixtures for this feature
(`expr1.3.11--float-range-expression.yaml`,
`expr1.3.11--string-range-expression.yaml`,
`expr1.3.11--path-range-expression.yaml`) pass on acceptance alone. What
exercises resolution and expansion end to end is
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
`protocol.ProtocolVersion` is `"2"`; version `"2"` carries the fields EXPR
phase-3 evaluation needs. All nine are `omitempty`, so they add nothing to a
base-spec assignment's wire bytes; only the envelope's own `"version"` value
differs from version `"1"`, which is what the worker's receiver-side check
keys off:

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
`LogChunkMsg`) is unaffected by the added fields. Version enforcement is
**both-directional**: the worker checks the `AssignMsg` it receives, and the
server checks the `RegisterMsg`, `HeartbeatMsg` and `TaskStatusMsg` it
receives (`scheduler.discardOnVersionMismatch`), discarding a mismatched
message rather than half-reading it. `LogChunkMsg` and `DeregisterMsg` are
deliberately not gated — a mismatched worker is already fenced out by the
registration gate, so neither can carry work-affecting state on its own.

## Worker behavior

### Choosing the resolution path

`msg.EXPR` selects between two resolution families at both places the
worker renders format strings — `internal/worker/executor/run.go`'s
`resolveAssignment` (the task's `onRun` action and step embedded files; the
environment variables it returns are `sess.StaticEnv()`, already resolved at
enter time, on both branches) and `internal/worker/session/session.go`'s
environment-entry and environment-exit resolution:

- `EXPR` false (the zero value, every base-spec assignment): the
  `fmtstring.Resolve`-backed plain-substitution path, byte for byte the
  base-spec behavior.
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
environment-scope builder, which conversely binds no `Task.*` family at all.
Every path value is bound under `expr.PathNative` — the worker runs ON the host a task executes on, so a
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
limits, by the worker's own `expr.operation_limit` and `expr.memory_limit`
([Worker configuration → `expr`](../worker-configuration.md#expr--expr-expression-limits)).
Those are **per-`Eval`** budgets, which is enough for every position that
renders a result and drops it — but not for `let:`, the one construct that
*retains* values; see below.

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
`expr.memory_limit` retain 50 times it. `expr.let_retained_bytes` (default
10 MB) bounds the total section 1.3.9 size **one symbol table** holds live,
measured across every block evaluated into it — the step template's and the
step script's for a task, the step template's and the environment's own for
an environment entry. A block that would push the table past it stops there
with an error rather than continuing to spend a fresh evaluation budget per
remaining binding. This is the worker-local, per-table bound only; the
cumulative, assignment-wide one is `expr.assignment_retained_bytes`, and the
template-wide one is the server's — see "Template-wide expression budget".

### Deliberate phase-2/phase-3 divergences

Three differences between the two phases are intended:

1. **`Session.PathMappingRulesFile` is bound conditionally at phase 3.** It
   appears only when the session has rules (`hasPathMap`), matching the
   base-spec `addPathMappingKeys` rule, so a reference to it with no rules
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

## Template-wide expression budget

Sections 1.3.9 and 1.3.10 of the expression language spec
(`wiki/2026-02-Expression-Language.md:1060` and `:1071`) each bound a
*single* evaluation: "the evaluator tracks the memory size of live values"
and "maintains a running operation count," reset for every `Eval` call.
Every `Eval` in sqi enforces both, independently, at every phase —
`openjd.expr_operation_limit`/`openjd.expr_memory_limit` (phases 1/2,
`internal/openjd/exprcheck.go`) and `expr.operation_limit`/`expr.memory_limit`
(phase 3, `internal/worker/fmtres/expres.go`) — and phase 3 additionally
bounds one symbol table's own `let:` retention (`expr.let_retained_bytes`,
default 10 MB, `internal/worker/fmtres/exprsyms.go`).

**None of that bounds the whole walk**, and the spec does not ask it to —
§1.3.9/§1.3.10 only obligate a per-evaluation bound. Without a cumulative
bound, a template with thousands of cheap positions, or many
individually-compliant `let:` blocks, pays no aggregate cost. Four
constructions show the gap:

| Construction | Cost | Local bound |
| --- | --- | --- |
| an unmetered expression walk over a submitted template | ~9 minutes of server CPU per request | the per-`Eval` limits |
| a `let:` block whose 50-binding cap is reported but not enforced | a 183 KB template body reached 6.9 GB in 1.45 s (`internal/worker/fmtres/exprsyms.go:464`) | `checkLetBindings`'s per-block cap, a different mechanism from this budget |
| one worker symbol table's `let:` retention, unbounded across the table | 472 MB retained in one table | `expr.let_retained_bytes`, per table, not cumulative |
| 16 task-parameter definitions × 1024 `RangeList` entries, one step | 96 s in the resolver alone, with every per-`Eval` budget respected the entire time (`internal/openjd/exprcheck_budget_test.go:17`) | none |

Each local bound covers one *table* or one *position*; none sums across a
whole *walk*. The cumulative budget does: it is charged once per position and
once per retained `let:` binding, summed across the entire walk rather than
reset per position or per table. It is an sqi-specific mechanism layered on
top of the spec's own per-evaluation bounds, enforced independently at three
separate points — because a budget shared across two walks that charge the
same positions halves the effective cap for each (item 2, below) — never
across phases (a fresh budget every call, so phase 2's verdict never depends
on what phase 1 already spent):

1. **Server, phases 1 and 2 — the checker.** `checkTemplateExpressions`'s own
   `templateBudget` (`internal/openjd/exprcheck.go`) caps:
   - `openjd.expr_template_positions`, default **10,000** — one charge per
     format-string/`let:`-binding position actually walked.
   - `openjd.expr_template_retained_bytes`, default **10,000,000** (10 MB) —
     the summed `expr.SizeOf` of every value a `let:` block adds to its
     table, the only construct in this walk that retains a value across
     positions. The default equals `expr.let_retained_bytes`' default
     (below) by choice. Configuration can separate them, and nothing
     compares these two dimensions across the binaries (the registration gate
     below pairs the template-wide byte budget with
     `expr.assignment_retained_bytes` instead).
   - Fresh per call: `ValidateWithOptions` (phase 1, unresolved parameters)
     and `checkExpressionsAtSubmit` (phase 2, concrete parameters) each get
     their own allowance.

2. **Server, phase 2 — the resolver.** `ResolveParameterSpaceParams`
   (`internal/openjd/resolve.go`), threaded from `submit.go`'s
   `prepareTemplate`/`Submit`, spends its *own* `templateBudget`
   (`resolverBudget`), shared across every step's resolver call for one
   submission but **not** the checker's budget (`checkerBudget`). The
   resolver re-charges the identical range positions and `let:` bytes the
   checker already charged for the same submission, so one shared budget
   would halve the effective cap for those classes: a template
   `ValidateWithOptions` (phase 1) accepted could be rejected by `Submit`
   purely because phase 2's two walks drew from one 10,000-position/10 MB
   pool instead of two.

3. **Worker, phase 3 — the assignment.** `AssignmentBudget`
   (`internal/worker/fmtres/assignmentbudget.go`), one per *session*
   (created once in `session.Manager.Create`, before any environment is
   entered, and shared by the task's own symbol table and every
   environment's) caps:
   - `expr.assignment_positions`, default **10,000**, so that it is never the
     tighter of the two position caps (an assignment's positions are a subset
     of its template's, so a lower value here is reachable by a template the
     server accepted and fails every task in the job after submission).
   - `expr.assignment_retained_bytes`, default **20,000,000** (20 MB) —
     2× `expr.let_retained_bytes`' default, sized for the common shape of a
     task plus one environment near its own per-table ceiling.
   - **Charged only at environment ENTRY, never at teardown.**
     `ExitEnvironments`'s own re-evaluation of an environment's `let:` block
     (`session.go`'s `resolveEnvAction`) does not charge `s.exprBudget`: it
     takes a **fresh budget per evaluation** (`Session.teardownBudget`,
     carrying the operator's configured limits). The table
     `resolveEnvAction` builds is rebuilt and discarded whether or not the
     charge is accepted — the allocation happens either way — and
     `ExitEnvironments` treats a resolve error as a warning and continues past
     it, so a budget that could trip at teardown would skip that
     environment's `onExit` (license check-ins, daemon shutdowns, unmounts)
     without preventing any memory use. Per-table (`expr.let_retained_bytes`)
     and per-`Eval` (`expr.operation_limit`/`expr.memory_limit`) bounds still
     apply to the teardown table unconditionally, at the operator's
     configured values — only the cross-table, assignment-wide ledger is
     exempt. The per-evaluation budget is fresh for the same reason: one
     shared teardown ledger would skip `onExit` the same way, one level down.

**What this budget does not measure:**

- **Operations are derived, not measured.** No operation counter crosses
  into `internal/openjd/expr` for either the template-wide or the
  assignment-wide budget — both count POSITIONS only. The server-side
  operation ceiling is a derived upper bound:
  `openjd.expr_template_positions × openjd.expr_operation_limit` =
  10,000 × 10,000 = **100,000,000** operations at the defaults, per walk, per
  phase. Because this ceiling is a *product*, raising either factor raises it
  proportionally — both are operator configuration, so raising either
  multiplies the ceiling and raising both multiplies it twice (10¹⁰ at the
  two maxima, 100× the default), without touching a single line of
  operation-counting code. Nothing in this mechanism adds up to 100,000,000;
  it is a bound on what the per-`Eval` operation limit, applied that many
  times, could in principle cost. The same product exists on the worker
  (`expr.assignment_positions × expr.operation_limit`, 10¹⁰ at the defaults
  and 10¹² at the maxima — also 100×, since the worker's wider operation
  range is offset by an identical position ceiling).
- **Neither budget bounds WALL-CLOCK TIME, and no value of either could.**
  An operation's real cost is not uniform: §1.3.10 rule 3
  (`wiki/2026-02-Expression-Language.md:1090`) prices a string operation at
  the value's length divided by 256, so byte-heavy work is charged almost
  nothing. Measured on the ~900 KB string `openjd.expr_memory_limit` permits
  to be live at its default:

  | Expression | Operations charged | CPU | per operation |
  |---|---|---|---|
  | `("x" * 900000).upper()` | 7,034 | ~6 ms | 0.9 µs |
  | `("x" * 900000).title()` | 7,034 | ~58 ms | 8.2 µs |
  | `re_findall("x", "x" * 900000)` | 3,519 | ~51 ms | 14.5 µs |
  | `max([len(re_findall("x", "x" * 900000)) for i in range(2)])` | 7,048 | ~103 ms | 14.6 µs |

  All four sit inside one position's default 10,000-operation budget; the
  first two are charged **identically** while differing by 9× in time, and
  the third is charged **half** as much as either while costing nearly as
  much time as the slowest — a ~17× spread in time per operation among these,
  and four orders of magnitude against scalar arithmetic.
  A ~650 KB template body built entirely from the **fourth** row costs
  **roughly 1,030 seconds** (~17 minutes) of server CPU on the synchronous
  validate/submit request path, with every budget respected throughout; a
  position spending its whole allowance at that rate puts the same template
  near 24 minutes, and at the two configurable maxima the equivalent
  construction reaches ~40 hours (100× the 24-minute ceiling: the worst
  per-operation rate measured, ~14.6 µs, is the same at both settings).
  **Operators running a publicly-reachable, unauthenticated
  `POST /api/v1/jobs` should size request timeouts and concurrency against
  that**, not against the position cap.

  The same template built from `.title()` costs 571 seconds: `.title()`
  leaves a third of the operation budget unspent and costs little more than
  half as much per operation as the fourth row.

  The figure is the maximum over an enumerated, measured set of payloads, so
  a construction outside that set can cost more. Two constructions that would
  each cost an order of magnitude more
  (`{{ len(range_expr("1-5000000,6000000-9000000")) }}` at ~107 minutes and
  `{{ [1] == range_expr("1-5000000,6000000-9000000") }}` at ~110 minutes) are
  bounded arithmetically and cost milliseconds; the fourth table row above is
  rejected by no bound, because nothing about it is out of budget. The table
  and the enumeration live in `defaultTemplatePositions`' doc comment
  (`internal/openjd/exprcheck.go`) and in
  `internal/openjd/expr/reservework_internal_test.go`. **Treat the number as
  a floor on the worst case, not a proof of it.**

  The wall-clock deadline (see [Known gaps](#known-gaps)) is what bounds
  time; the position and byte budgets above bound *memory* and *count*, and
  that is all they claim.
- **The byte dimension measures cumulative allocation, not peak live
  retention.** Both `openjd.expr_template_retained_bytes` and
  `expr.assignment_retained_bytes` sum every `let:` block's own charge across
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
  openjd.expr_memory_limit` = 50 × 1,000,000 = **50,000,000 bytes (50 MB)**
  at the defaults before this counter rejects it. That is the true
  single-block ceiling this budget enforces, not 10 MB — and it moves with
  `openjd.expr_memory_limit`, not with `openjd.expr_template_retained_bytes`.
  The worker's equivalent is `50 × expr.memory_limit`, 1 GB at that key's
  default; that product, not the key itself, is the number to size a host's
  RAM against.

**`maxSteps` = 100** (`internal/openjd/validate.go`) caps the number of
`<StepTemplate>` entries *any* job template may declare. Unlike every other
bound on this page, which engages only once a template declares
`extensions: [EXPR]`, it applies to **every** template, including base-spec
ones declaring no `extensions:` at all. It is not a spec transcription:
OpenJD's own "Constraints" list for `<JobTemplate>` gives
`parameterDefinitions` an explicit min/max
(`wiki/2023-09-Template-Schemas.md:62`–`64`, item 6) but states none at all
for `steps` (`wiki/2023-09-Template-Schemas.md:71`, item 8) — 100 is an sqi
product decision, gated by `openjd.enforce_limits`
(`docs/configuration.md`) like the package's other bare structural-count
caps, not one of the package's always-on resource-exhaustion guards
(`maxRangeValues`, `maxTasksPerStep`). It also sizes the position budget's
own denominator: `openjd.expr_template_positions`' default was chosen
against a worked, per-step position count times `maxSteps` — see
`defaultTemplatePositions`' own doc comment
(`internal/openjd/exprcheck.go`) for the full arithmetic. `maxSteps` is a
compiled constant, unlike the nine expression limits: it is a **policy** cap
already escapable via `openjd.enforce_limits`, not a resource-exhaustion
guard.

**Task count is a different dimension from expression cost.** These budgets
bound what *expressions* may cost to evaluate, not how many *tasks* a
parameter space may expand to. A template whose steps each declare a large
`range` (e.g. `"1-1000000"`) validates cheaply and quickly and then
multiplies: `maxSteps` (100) × `maxTasksPerStep` (1,000,000) is 10⁸ task rows
from one `POST` at the production default (`enforce_limits: true`), and with
`openjd.enforce_limits: false` the step count is unbounded and the product
has no ceiling at all.

`maxTasksPerJob` (`internal/openjd/expand.go`) bounds that product: it caps
the tasks a whole job may produce, summed across steps, at **1,000,000** —
the same figure as `maxTasksPerStep`, on the principle that a job may not
produce more tasks than a single step may. It cuts the worst case from 10⁸
rows to 10⁶.

Three properties matter:

- **It is always-on**, not gated by `openjd.enforce_limits`: with the flag
  off the step count is unbounded, so a gated cap would leave the worse case
  uncovered. A caller that relaxes quantitative limits does not thereby
  acquire the right to insert a hundred million rows.
- **It applies to every template, not only EXPR ones.** A job of two
  600,000-task steps is rejected whether or not it declares
  `extensions: [EXPR]`. It is a compiled constant, like its sibling — tune it
  if legitimate jobs need larger totals.
- **A breach writes nothing.** The cap is charged per step during expansion,
  which runs to completion in memory before the single submission write, so a
  two-step job of 600,000 + 600,000 is rejected with no rows persisted.

See `maxTasksPerJob`'s doc comment (`internal/openjd/expand.go`) for the
mechanism.

## Operator configuration

All nine limits above are **operator configuration**, not compiled
constants. The full reference, with each key's range and what happens at the
bound, is in the two configuration guides — this section is the map:

| Bounds | Server key | Worker key |
|---|---|---|
| operations, one evaluation | [`openjd.expr_operation_limit`](../configuration.md#openjdexpr_operation_limit) | [`expr.operation_limit`](../worker-configuration.md#exproperation_limit) |
| live bytes, one evaluation | [`openjd.expr_memory_limit`](../configuration.md#openjdexpr_memory_limit) | [`expr.memory_limit`](../worker-configuration.md#exprmemory_limit) |
| positions, one walk / one assignment | [`openjd.expr_template_positions`](../configuration.md#openjdexpr_template_positions) | [`expr.assignment_positions`](../worker-configuration.md#exprassignment_positions) |
| retained bytes, one walk / one assignment | [`openjd.expr_template_retained_bytes`](../configuration.md#openjdexpr_template_retained_bytes) | [`expr.assignment_retained_bytes`](../worker-configuration.md#exprassignment_retained_bytes) |
| retained bytes, one symbol table | [`openjd.expr_template_retained_bytes`](../configuration.md#openjdexpr_template_retained_bytes) *(the walk-wide bound that upper-bounds one table)* | [`expr.let_retained_bytes`](../worker-configuration.md#exprlet_retained_bytes) |

A tenth key is not in that table:
[`openjd.expr_submission_deadline`](../configuration.md#openjdexpr_submission_deadline)
is a wall-clock backstop, not a budget. It decides only whether this server
keeps working on a request (`503`), never whether a template is valid, so
none of the three properties below applies to it — it has no worker
counterpart, and no cross-binary gate. See "Known gaps" for what it does and
does not close.

Three properties of that surface:

1. **`0` never means "unlimited".** Each key has a validated floor and
   ceiling; an out-of-range value fails startup with a message naming the
   bound, rather than being clamped. The two sides' floors are sized
   differently on purpose. On the **server**, tightening only rejects work at
   submit, on the one request that can report it, so the floors are sized
   against this repository's own reference presets (`presets/sqi/*.yaml`) —
   whose worst case costs 31 positions template-wide, and 390 live bytes with
   15 operations in any single evaluation — those three all set by
   `ffmpeg-segment-transcode-expr`, and re-measured on every run by
   `TestExprLimits_FloorsAcceptReferencePresets`. Retained bytes are
   effectively zero for all fourteen (the binary search floors that dimension
   at 1, so a reported "1" means "nothing measurable"). Headroom is wide
   throughout (8x to 65536x). On the
   **worker**, tightening rejects work *after* the job was accepted, so the
   floors are sized well above what a preset happens to cost: the server's own
   **defaults** for the two per-evaluation dimensions, and 2,000 positions
   against a worked figure of 1,841 for a generous session.

   **A worker at its floors cannot be assumed to run everything this server
   accepts, and the floors do not claim it.** They are fixed numbers sized
   against the server's *defaults*; the server's actual limits are themselves
   configurable, up to 10x those defaults, and no worker reads them. Two of
   the five are further apart still: `expr.let_retained_bytes`' floor is a
   *tenth* of `openjd.expr_template_retained_bytes`' default. Setting a worker
   to its floors and raising a server key is a supported thing to do, and what
   happens then is property 3 — the work is **withheld**, visibly — not a
   per-task failure after acceptance.
2. **The ceilings are not all the same kind of bound.** The ones whose
   absence produced measured multi-minute requests and multi-gigabyte heaps
   are **catastrophe** ceilings — one order of magnitude above the default.
   The rest are **policy** ceilings, wide but finite, sized against what a
   legitimate template plausibly needs.
3. **The server↔worker relation is enforced at runtime, not at load.** Each
   worker advertises all five of its caps in its registration message; the
   server persists them and the scheduler **withholds EXPR work** from any
   worker whose caps undercut the limits this server accepts templates under.
   That is a per-job refusal, not a per-host one: a tightened worker keeps
   running every base-spec job. Neither binary can validate the relation on
   its own — the worker does not read the server's configuration, and by
   design does not receive it.

   The fifth dimension, per-table retained bytes, has no server-side
   counterpart, because the server meters no per-table scope. The
   template-wide retained-bytes budget is compared instead, as a valid
   **upper bound** on any one table: without that comparison a worker at its
   `expr.let_retained_bytes` floor would accept, and then fail once per task,
   a `let:` block the server had accepted. Comparing against the
   per-*evaluation* memory limit instead would be unsound (a table
   accumulates: eight 1 MB bindings are accepted by a default server and
   rejected by a 1 MB table), and comparing against the sufficient `50 x` form
   would exceed the worker key's own legal maximum.
   `internal/scheduler/exprcaps.go` carries the arithmetic.

**The gate identifies an EXPR job by reading the extension list persisted on
the job row** (`jobUsesEXPR`, `internal/scheduler/exprcaps.go`). Migration
`00027` added `jobs.declared_extensions`, which `internal/openjd` fills at
submission from the parsed and registry-validated declaration, so the test is
exact — including for a declaration spelled with an escape (`"EXPR"`),
which decodes to `EXPR`, is accepted, and is stored verbatim in the raw
template. Reading it costs no extra query: the column is on the same `SELECT`
the lease path already issues for each candidate task.

The column is `TEXT NOT NULL DEFAULT ''`, and `''` (**not recorded**) is a
different value from `'[]'` (**recorded, declares nothing**). Every job row
written before the migration holds `''`, and for those alone the gate falls
back to a byte scan (`jobMayUseEXPR`): the raw template must contain both the
bytes `EXPR` and the bytes `extensions`. That fallback is a deliberate
superset — a template declaring some *other* extension and mentioning `EXPR`
elsewhere still matches, and an obfuscated declaration still escapes it — but
it applies only to the jobs a deployment already held when it upgraded.
Defaulting the column to `'[]'` instead would read every one of those rows as
declaring nothing and ungate them.

## Known gaps

- **End-to-end coverage is one job per harness.**
  `test/integration/expr_realworker_test.go` (`TestEXPRJobEndToEnd`) submits
  an `extensions: [EXPR]` job to a real server and runs it on a real worker,
  and `scripts/smoke.sh` submits a second one against the shipped binaries.
  Both assert on a value only the **worker** can produce — the integration
  job on `{{ upper(Param.Shot) + '.' + zfill(Task.Param.Frame * 3, 4) }}`,
  whose `Task.Param.Frame` the server cannot resolve, plus an
  `is_absolute(Session.WorkingDirectory)` that forces the path engine to run
  against the worker's own session directory. Neither is coverage in the
  broad sense; each is one job, proving the whole path works against real
  binaries. Submitted list-parameter values are covered by
  `test/integration/expr_paramtypes_test.go`, which calls
  `openjd.Parse`/`openjd.BindJobParameters` directly.
- **The 141 `EXPR/jobs/` conformance fixtures are scored by nothing.**
  `test/conformance/suite_test.go` walks only `job_templates` and
  `env_templates`, and returns early for a job test — so the runtime half of
  the extension (submitted parameter values, `Param.<name>[i]`,
  `list(Param.FrameRange)`) has no external scoreboard. The template half is
  measured; the runtime half is measured only by sqi's own tests.
- **The worker cannot report an over-cap `let:` block.** It has no
  `validateLetElementCounts` equivalent; it truncates at 50 bindings without
  reporting it, relying on phase 2 having already rejected the template.
- **Wall-clock time is bounded by a deadline, not by the counters.**
  `openjd.expr_submission_deadline` (default 5s, range 1s–60s) gives one
  submission a fixed wall-clock allowance for its expression evaluation and
  stops it when that passes, on **every route that validates a template**:
  the two job routes, the two product create/update routes, and the two
  preset routes.
  ([`docs/configuration.md`](../configuration.md#openjdexpr_submission_deadline)
  enumerates them and what each answers for an invalid template.) It bounds
  elapsed time *directly*, so it holds whatever the operation pricing
  permits — which is the property none of the counters have.

  **It is not co-sized with the counters, and the headroom against real work
  is ~6x.** Measured with a generated multi-layer VFX shot-render job (12
  `let:` bindings per step, comprehensions over real frame ranges,
  `RANGE_EXPR`/`LIST[STRING]`/`PATH` parameters) driven through the real
  `Submitter.Submit`, at the default limits:

  | Shape | Wall clock | Positions | Operations | Verdict |
  |---|---|---|---|---|
  | 40 steps / 1,000 frames | 253 ms | 1,973 | 540,621 | accepted |
  | 100 steps / 1,000 frames | **831 ms** | 5,013 | 1,661,261 | accepted — worst |
  | 100 steps / 2,000 frames | 1.108 s | — | 2,120,268 | `422`, retained bytes |
  | 100 steps / 60 args | 702 ms | 10,001 | 1,109,586 | `422`, positions |

  So **0.83 s is the worst legitimate job this server accepts, ~6x under the
  5s default**. A production host 2–3x slower puts that job at 1.7–2.5 s.
  **At the defaults the counters always bind first and the clock never
  does**, and the binding dimension is *retained bytes*, not positions. At
  the *legal maximum* limits it inverts: 100 steps × 10,000 frames takes
  7.5 s and correctly produces `503 … deadline exceeded`, which is why
  raising the four limits without raising this key converts legitimate
  acceptances into 503s
  ([`docs/configuration.md`](../configuration.md#2-none-of-these-limits-bounds-wall-clock-time)).

  Phase 2 is ~98% of that cost (15 ms phase 1 against a 727 ms phase-2
  checker): `Param.*` is unresolved at phase 1, so the identical template
  charges 4,519 operations there against 363,467 at phase 2. Measuring only
  phase 1 understates the figure ~50x.

  A breach is reported as **`503`, never `4xx`**. The counters are
  deterministic, so a template that breaches one breaches it everywhere and
  the submitter is told the template is invalid; a wall-clock stop is
  load-dependent, the same body would be accepted on an idle host, and
  reporting it as an invalid template would make acceptance depend on machine
  load. It is matched structurally on `expr.ErrDeadlineExceeded`, never by
  reading a message. `internal/api`'s deadline tests drive a real
  `Submitter` through the handler and assert a real `503` on all three
  template-accepting routes.

  **The counters cannot bound time on their own.** §1.3.10 prices 256 bytes
  at one operation, so byte-heavy work is nearly free in budget terms; the
  wall-clock worst case at the default limits is measured at roughly
  **17–24 minutes** of server CPU for one request with every budget
  respected, and about **40 hours** at the two configurable maxima. Treat
  that figure as a floor on the worst case, not a proof of it; the deadline
  does not depend on that measurement being right. A request refused at the
  deadline still *spent* that time, and concurrent requests each get their
  own allowance: the deadline bounds one request, not a host.

  **The ~17-minute figure and the ~1.1 s one describe different template
  shapes; neither supersedes the other.** An adversarial template does
  op-cheap, byte-heavy work across many positions and **discards** every
  result, so it never approaches the retained-bytes cap and this deadline is
  the only thing bounding it. A legitimate template leans on `let:`, which
  retains, so it hits retained bytes first — at ~1.1 s. **~1.1 s is the
  ceiling for realistic `let:`-using templates; the adversarial ceiling is
  far higher, which is why the wall-clock backstop exists.**
- **The cross-binary gate is necessary, not sufficient.** A worker at parity
  with the server can still exhaust its own budget on a concrete value phase
  2 only had a placeholder for, and the per-table dimension is compared
  against a walk-wide bound that measures a different scope in both
  directions (it ignores the worker's whole-table accounting of job
  parameters, and over-counts a template that spreads its budget across many
  steps). That residual is what the worker's generous defaults exist for; do
  not read the gate as a guarantee that an accepted job runs. The byte scan
  is not how an EXPR job is identified; it survives only as the fallback for
  job rows written before migration `00027`.
- **A worker's advertised caps are not surfaced in the API or the web UI.**
  They are persisted on the worker row, but an operator diagnosing "why is
  this host getting no EXPR work" reads the registration warning or the
  task's unschedulable reason instead.
- **Workers must be upgraded after the server, never before.** The worker's
  lease loop rejects any `AssignMsg` whose `Version` does not match its own,
  so a `"2"` worker talking to a `"1"` server rejects every assignment it is
  offered and the tasks churn through reclaim until the server is upgraded.
  The reverse order is the safe one: a `"1"` worker against a `"2"` server is
  refused at registration, so it is never offered work at all — it does
  not appear in the workers list, and the server logs the mismatch with both
  versions. Nothing enforces the ordering, but neither direction is silent.
- **The gate covers four of six channels.** `AssignMsg` (worker-side, since
  `"2"`), plus `RegisterMsg`, `HeartbeatMsg` and `TaskStatusMsg`
  (server-side). A discarded task status is the one that costs something:
  the server never learns the task finished, so it is reclaimed and retried.
  That is deliberate — it is the message carrying the outcome, and a wrong
  outcome written to a task that actually succeeded is worse than a re-run.
  `LogChunkMsg` and `DeregisterMsg` remain ungated by design.
- **Three `EXPR/job_templates` fixtures are baselined, and none of them is an
  EXPR shortfall.** `3.6--let-bindings.yaml`,
  `3.6--let-host-context-symbols.yaml` and
  `7.3.1--job-step-name-in-step-let.yaml` are valid templates that sqi
  rejects, but all three declare **`FEATURE_BUNDLE_1`** (RFC 0004) alongside
  `EXPR` and use that extension's `bash:` `SimpleAction` in place of
  `script:`. sqi does not register `FEATURE_BUNDLE_1`, so `validateExtensions`
  refuses the template at its *other* extension and their `let:` subject
  matter is never reached. Until sqi registers RFC 0004, these three stay
  baselined with that reason recorded in `test/conformance/baseline.txt`.
