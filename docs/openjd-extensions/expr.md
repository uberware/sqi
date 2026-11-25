<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->

# EXPR

- Origin: official
- Status: supported
- Summary: Python-subset expression language in format strings.

## Motivation

Base-spec OpenJD format strings (`{{Task.Param.Name}}`) substitute a symbol's
text verbatim. EXPR (RFC 0005/0006) extends that to a small Python-subset
expression language: arithmetic, comparisons, string/list operations,
comprehensions, `let:` bindings, path manipulation
(`Session.WorkingDirectory / 'frames'`), path mapping
(`apply_path_mapping(...)`), and an 80-name standard library (74 callable
functions plus the six path properties), all evaluatable inside `{{ ... }}`.
A template declares it with `extensions: [EXPR]`.

## Worked examples

`presets/sqi/ffmpeg-segment-transcode-*.yaml` are the shipped worked
examples of this extension: three variants of the same segmented ffmpeg
transcode, all of which declare `extensions: [EXPR]`
(`presets/sqi/ffmpeg-sequence-encode.yaml` declares it too, for less).
`ffmpeg-segment-transcode-expr` ("Portable") builds its ffmpeg concat file
list from an EXPR comprehension instead of a join-time shell script, so it
needs no shell on the worker, not even `bash` (which the bash-joined variant
needs, though neither is gated to one OS). That list-building expression
references only job parameters, so it is fully resolved at submission
(phase 2, not phase 3) and its cost is charged against the server's
[`openjd.expr_operation_limit`](../configuration.md#openjdexpr_operation_limit)
(default 10,000), not the worker's `expr.operation_limit`. It costs about 24
operations per slice, so the preset documents a ceiling of 400 slices under
stock server limits; see that file's `description`,
[`docs/preset-library.md`](../preset-library.md#transcoding-reference-presets),
and `internal/product/exprpresetcost_test.go`, which pins the ceiling.

## Status

EXPR is a supported extension. `internal/openjd/extension.go`'s registry
entry carries `Status: StatusSupported`, so `internal/openjd/validate.go`
accepts `extensions: [EXPR]` and every EXPR template is submittable through
`POST /api/v1/jobs`, judged on its own merits like any other template. EXPR
jobs are dispatched to workers and executed, and `test/integration` and
`scripts/smoke.sh` each run one through the real binaries end to end,
asserting on a value the worker resolves at phase 3.

Conformance against the vendored fixtures, on the same template path that
scores every other suite: `EXPR/job_templates` 230/233 with 3 baselined,
alongside `base/job_templates` 459/459 and `TASK_CHUNKING/job_templates`
12/12. The three `EXPR` shortfalls are not EXPR shortfalls; see
[Known gaps](#known-gaps) and [OpenJD conformance](../openjd-conformance.md).

The extension is implemented and tested as follows:

- **Expression core**: the full type system, coercion, operators,
  comprehensions and call syntax, in `internal/openjd/expr` (package doc:
  `internal/openjd/expr/doc.go`).
- **The standard library**: conversions, math, string, regex, path, and
  `apply_path_mapping`. There are 80 registry entries, 74 callable functions
  and the six path properties (`__property_name__` and friends), registered
  in the same table, `internal/openjd/expr`'s `functionShapes`, whose count
  `funcs_internal_test.go` pins at 80.
- **`apply_path_mapping` is host-context-only.** It is registered flat in
  `functionShapes` (the evaluator imposes no scope restriction), and the rule
  is enforced one layer up, by the phase-2 checker: `hostOnlyFunctions`
  (`internal/openjd/exprcheck.go`) walks `Expression.CalledFunctions()` at every
  position whose `Scope.IsHostContext()` is false and rejects the call there.
  `IsHostContext` is a positive list of exactly three scopes,
  `ScopeJobEnvironment`, `ScopeStepEnvironment` and `ScopeStepScript`
  (`internal/openjd/scope.go`), so the function is refused in the job `name`
  field, in `hostRequirements`, in `parameterSpace` and in a
  `<StepTemplate>.let` block, all of which are evaluated at submission before
  any session exists. Because the list is positive, a new scope is not a host
  context until it is added to it.
- **Bounded evaluation**: per-`Eval` memory and operation limits
  (`internal/openjd/expr/limits.go`, `meter.go`), plus a per-symbol-table
  retained-bytes bound on the worker's `let:` evaluator
  (`internal/worker/fmtres`, configured by `expr.let_retained_bytes`). These
  bound individual evaluations and one task's or one environment's retained
  bindings; a cumulative budget bounds the whole template and the whole
  assignment on top of them (see [Template-wide expression
  budget](#template-wide-expression-budget)). Two further bounds cover what no
  counter can: a wall-clock deadline (`openjd.expr_submission_deadline`,
  default 5s, checked inside `meter.charge`), needed because §1.3.10 prices
  256 bytes at one operation so an operation budget cannot bound seconds; and
  `maxTasksPerJob` (`internal/openjd/expand.go`), an always-on cap on the
  tasks one job may produce, summed across steps. A deadline breach is a
  `503`, never a 4xx, because it is not a verdict on the template. The only
  bound that applies before evaluation is `maxSourceBytes`
  (`internal/openjd/expr/limits.go`), an always-on cap of 10,000 bytes on the
  source of one expression, checked at the top of `Parse` before the source
  is tokenized; every other bound is metered from inside an evaluation, so
  without it a very large expression would be parsed in full. It is not
  configurable, and an oversized expression is a `422`, since length is a
  deterministic property of the template.
- **Operator configuration of all nine limits**: every one of the bounds
  above is a config key on one of the two binaries, with a validated range
  and a cross-binary dispatch gate; see [Operator
  configuration](#operator-configuration). The submission deadline is a tenth
  key on the same pattern, and `maxTasksPerJob` is a compiled constant, like
  its per-step sibling.
- **The scope model and phase-2 checker**: `internal/openjd/scope.go` and
  `internal/openjd/exprcheck.go` type-check every format string and `let:`
  block against the template's declared parameters at submission time, so a
  template that would fail at runtime is rejected up front.
- **Phase-3 (worker-side) evaluation**, described below: expressions are
  evaluated against a real task's concrete values, on the worker that
  executes it.
- **Task-parameter range field extensions (section 1.3.12)**, described in
  the next section. This is a submission-time (phase-2/resolve) feature, not
  a worker one.
- **A template-wide, cumulative expression budget**, described in
  [Template-wide expression budget](#template-wide-expression-budget). It
  bounds the cumulative cost that the per-`Eval` bounds do not reach.

## Extended parameter types

RFC 0007 extends the job-parameter type system, and sqi implements it.

**Case-insensitive type names.** Every type name, job and task, including
compound ones: `int`, `Path`, `List[Int]`, `list[list[int]]`. Gated on
`extensions: [EXPR]`: a base-spec template writing `type: int` is rejected,
because RFC 0007 is an extension specification and accepting lowercase
unconditionally would widen what the base spec admits.

**Eight new job-parameter types.**

| Type | Default | Notes |
| --- | --- | --- |
| `BOOL` | `true`, `1`, `1.0`, `"yes"`, `"on"` and negatives | `allowedValues` is rejected, as the RFC requires: restricting a two-valued type to one value constrains nothing. Its control is `CHECK_BOX`, without the base spec's two-`allowedValues` requirement. |
| `RANGE_EXPR` | an `<IntRangeExpr>` string | `minLength`/`maxLength` bound the expression's string length, default max 1024. |
| `LIST[STRING]`, `LIST[PATH]`, `LIST[INT]`, `LIST[FLOAT]`, `LIST[BOOL]` | a YAML sequence | Stored as canonical JSON in `JobParameter.Default`. `LIST[PATH]` also carries `objectType`, `dataFlow`, `fileFilters` and `fileFilterDefault`. |
| `LIST[LIST[INT]]` | a sequence of sequences | One level of nesting, the RFC's own limit. No control but `HIDDEN`; its use case is programmatic. |

Element constraints use a nested `item:` block mirroring the type nesting, so
each level reuses the scalar names (`minLength`/`maxLength` for string-like,
`minValue`/`maxValue` for numeric). Element type checking is by JSON type,
which is what the RFC's `<string>` / `<integer>` schemas constrain. `LIST[BOOL]`
is the exception: the RFC gives its items the same accepted-values table as a
scalar `BOOL`.

**`RANGE_EXPR` uses the specification's permissive range policy**, not
`internal/openjd`'s stricter one, which rejects `start > end` and a negative
step in literal base-spec range text. A `RANGE_EXPR` parameter accepts both,
because the conformance fixture declares `"10-1:-1"` and `"-1--10:-1"`, and
because a lone whole-field `range: "{{Param.FrameRange}}"` evaluates as a
value through `range_expr`'s own coercion, which uses the permissive parse.
Literal range text keeps the strict policy.

**The `<ArgString>` control-character amendment.** When `EXPR` is declared, CR,
LF and TAB become legal anywhere in an argument, "to support multi-line
expressions in YAML literal block scalars" (Expression Language, "When EXPR is
enabled", item 3). Every other Cc character is still rejected, and
`<CommandString>`, a separate type in the same schema (Template Schemas §5.1
and §5.2), is not amended, so a command keeps the base-spec rule.

**Both evaluation phases resolve the new types.** `expr.JobParamTypes` maps
them and `expr.ValueFromText` decodes their values. That function lives in the
leaf package both phases import, so phase 2 and phase 3 resolve the types the
same way; `TestPhase2Phase3Agreement` covers all eight.

**List values at run time.** A `LIST[*]` value is canonical JSON
(`internal/openjd/paramjson.go`), held in the same `map[string]string` as
scalar values, which matters in two runtime paths:

- **`loc://` resolution is per element.** `resolveLocURIsInParamValue`
  (`internal/scheduler/assign.go`) decodes the list, resolves `loc://` element
  by element, and re-encodes through the same codec that produced it.
  Substring replacement on the whole string would splice the resolved path
  inside a JSON string literal, which corrupts a Windows path (backslashes are
  not legal JSON escapes) and leaves the value undecodable.
- **`LIST[PATH]` elements are path-mapped.** `paramValueForBinding`
  (`internal/worker/fmtres/exprsyms.go`) runs each element of a `LIST[PATH]`
  job parameter through `mapListPathParamValue`, which applies the same
  `mapPathParamValue` the scalar case uses, as RFC 0007 requires:
  `Param.<name>` for a list of paths "Returns a `list[path]` type value with
  path mapping applied."

**Web form.** `web/src/api/types.ts` mirrors the eight RFC 0007 types and the
six `*_LIST` `userInterface` controls, and the product form renders each list
type with a `list` widget; see `docs/web-development.md`'s "Product
parameter widgets" section.

## Task-parameter range field extensions (section 1.3.12)

Base-spec `range` (Template Schemas §3.4.1, `<TaskParameterDefinition>` and
its four type-specific variants) is either a literal list of values or, for
INT/CHUNK[INT] only, the succinct `<IntRangeExpr>` string syntax
(`"1-100:2"`). Section 1.3.12 of the expression language spec
(`wiki/2026-02-Expression-Language.md:1109`) extends what that field may
contain, per declared type. Its table:

| Parameter Type | Original `range` Type | Extended `range` Type |
| --- | --- | --- |
| INT | `list[int \| FormatString] \| RangeString` | (unchanged, but see `RangeString` note below) |
| FLOAT | `list[Decimal \| FormatString]` | `list[Decimal \| FormatString] \| ListExpressionString` |
| STRING | `list[FormatString]` | `list[FormatString] \| ListExpressionString` |
| PATH | `list[FormatString]` | `list[FormatString] \| ListExpressionString` |

Two distinct shapes follow from that table, handled by different code and
producing different results.

**The whole-field target, per declared type.** A whole-field `range` that is
exactly one `{{ ... }}` reference (`fmtstring.LoneRef`, no surrounding text) is
evaluated against the type section 1.3.12 gives that field:

| Declared type | Whole-field target |
| --- | --- |
| INT, CHUNK[INT] | `int \| string \| range_expr \| list[int]` (match-first) |
| FLOAT | `list[float]` |
| STRING | `list[string]` |
| PATH | `list[path]` |

**One function produces that target for both layers.** The checker
(`checkParameterSpaceExpressions`, `internal/openjd/exprcheck.go`) and the
resolver (`evalRangeExprField`, `internal/openjd/resolve.go`) both call
`rangeExprFieldType`, so their accept/reject verdicts and their rejection
messages are identical.

**FLOAT/STRING/PATH: `<ListExpressionString>`.** These three rows extend the
field with a format string containing an expression that evaluates to a
list, e.g. `range: "{{ [Param.Scale * 2, Param.Scale + 0.5] }}"` for a
FLOAT parameter (the spec's own worked example, and one of the three vendored
`expr1.3.11--*-range-expression.yaml` conformance fixtures, named for the
section's earlier number). Each resulting list element becomes one
`RangeList` entry, rendered to text with `Value.String()`, and `RangeExpr` is
cleared.

**INT/CHUNK[INT]: four members, two outcomes.** Only the INT row carries
section 1.3.12's `RangeString` note, because only INT/CHUNK[INT] has the
`<IntRangeExpr>` text form for an expression to be "in addition to". The
whole-field result is dispatched on which member it landed in, in this order:

- a `list[int]` result, or a `range_expr` result (e.g. from
  `range_expr(...)`, converted with section 1.2.3's own `range_expr →
  list[int]` rule via `expr.Coerce`, with no detour through range-expression
  text), becomes the range's values in `RangeList`, with `RangeExpr`
  cleared;
- an `int` or a `string` result is range text: it is rendered with
  `Value.String()` into `RangeExpr` and read by `parseIntRangeExpr` under
  `internal/openjd`'s own base-spec policy, exactly as a hand-typed
  `"1-100:2"` is. So `range: "{{Param.Frames}}"` with a `STRING` `Frames` of
  `"1-100:2"` yields the same 50 tasks with or without `extensions: [EXPR]`,
  and `range: "{{Param.N + 1}}"` with `N = 7` yields the single task `8`.

The two list-shaped members are recognised before the text fallback, which
keeps a lone `{{ range_expr(...) }}` out of `parseIntRangeExpr`; see the
whole-field/embedded distinction below.

> **The target must include `int` and `string`.** With a target of only
> `range_expr | list[int]`, `range: "{{Param.Frames}}"` with a `STRING`
> `Frames` would be rejected at template upload, while the same template
> without `extensions: [EXPR]` expands, so declaring the extension would
> remove base-spec capability at this field. Presets in `presets/sqi/` use
> that shape. The conformance suite states the required target
> (`EXPR/jobs/expr1.2.3--union-target-type.test.yaml:12`) and exercises all
> four members.

**Symbol table: a step's own `let:` names are in scope here.** Section 3.6.2
row 1 makes the names a step template's `let:` block binds visible in
`parameterSpace`, and both layers build that table from the same pair of
helpers (`stepLetSymbols` and `rangeScopeSymbols`, `exprcheck.go`): the
`ScopeJob` fixed and family symbols plus the step's let names, and nothing
else. `ResolveParameterSpaceParams` therefore takes the `*StepTemplate`
alongside the `*JobTemplate`; if only the checker saw those names, a
step-level `let:` plus a range that referenced it would validate at upload and
then fail at submit with `unknown symbol`.

**A RangeList entry that is exactly `{{<expr>}}` is a list item** (section
1.3.12, as section 1.3.2 defines list items). Its target is `T? | list[T]`,
where `T` is `int` for INT and CHUNK[INT] and `string` or `path` otherwise;
for FLOAT it is `float? | int | list[float]`, and an `int` result is kept as
written. A `null` result drops the entry and a list result is flattened into
one entry per member.
`rangeListItemType` (`resolve.go`) is the one definition the checker and the
resolver share. A list that flattening empties is an error, and so is one that
would carry the list past the step's task limit.

**Every other RangeList entry or RangeExpr shape (literal text, or a
`{{...}}` reference embedded in surrounding text) is not a whole-field list
expression.** For INT/CHUNK[INT], section 1.3.12 says the `RangeString`
field "is also extended" because, being a format string, it "can now contain
an expression that evaluates to either a range expression string or a
`list[int]`", e.g. `range: "1-{{ Param.End }}"`. For every type, an
individual RangeList entry is a format string in the base specification:
`<TaskParameterStringValue>` (Template Schemas §3.4.2) is defined as a Format
String, and every type's `range` list is built from it (INT/FLOAT via
`<IntRangeList>`/`<FloatRangeList>`, §3.4.1.1/§3.4.1.2; STRING/PATH directly,
§3.4.1.3/§3.4.1.4, `# @fmtstring`). Under EXPR, its embedded reference is
evaluated as an expression (section 1.3.2's general embedded-reference rule:
unconstrained `expr.TAny`, rendered with `Value.String()`) rather than looked
up as a bare dotted identifier. Either way, the result is text, resolved by
`resolve.go`'s `resolveRangeExprField`/`resolveRangeListEntry` like any other
embedded reference, and never a value handed to `evalRangeExprField`'s
list-producing path.

**The whole-field/embedded distinction for INT/CHUNK[INT].**
`internal/openjd`'s own `<IntRangeExpr>` reader (`parseIntRangeExpr`, via
`internal/openjd/intrange`, configured with
`intrange.Policy{PositiveStepOnly, AscendingOnly}`) differs from the
expression language's (`internal/openjd/expr` passes the zero `Policy` and
follows the spec) in three ways: it rejects `start > end`, rejects a negative
step, and expands in first-seen rather than ascending order. A lone
whole-field range expression is consumed directly as a value and never
re-enters that reader, so it keeps the expression language's semantics:
`range_expr("5-1")` resolves to the single value `["5"]` (section 3.4.1.1.1's
`x-y` formula, for `x > y`, reduces to the set `{x}`, the same rule that
makes `"1 - -1"` the single value `[1]` in the spec's own worked table),
while the same text as literal `<IntRangeExpr>` syntax is rejected by
`internal/openjd`'s `start > end` check; and `range_expr("10-15:2,1-5")` comes
out ascending, `[1,2,3,4,5,10,12,14]`, the spec's own worked example for that
text. An embedded occurrence of the same call, e.g.
`"{{ range_expr(\"10-15:2,1-5\") }},7"`, renders to its canonical form
(`Value.String()` on a `range_expr` value writes the canonical form of
Template Schemas §3.4.1.1.1: ascending, positive steps, runs of three or more
collapsed, so `"1-5,10-14:2"` here), and the composed result is ordinary
base-spec `<RangeString>` text, parsed by `internal/openjd`'s first-seen reader
like any other literal range string. The same `range_expr(...)` call can
therefore produce a different ordering depending on whether it is the field's
sole content: `[1,2,3,4,5,10,12,14]` as a lone expression,
`[1,2,3,4,5,10,12,14,7]` here, with the 7 after the values that precede it in
the text. `resolveRangeExprField`'s doc comment (`resolve.go`) explains this,
and `TestResolveParameterSpaceParams_NonLoneRangeExprEmbeddedRangeExprValue`
pins it.

The canonical form is computed from the elements' arithmetic, never by listing
their values, so a range of a billion values costs what a range of three does.
The one exception is a range expression whose elements overlap: their union is
settled by listing the values, up to 65,536 of them, and a larger overlapping
expression keeps the text it was written in. A CHUNK[INT] value keeps its own
form, `<N>-<M>` for a CONTIGUOUS chunk even with one or two values (section
1.2.2); a `range_expr` computed from it is canonical. A RANGE_EXPR job
parameter's value is canonicalized when it is bound, at submission.

**What the checker does not judge, and where those errors surface instead.**
The checker judges an expression's type. It never judges the syntax, length or
value of the range text or range list that expression produces:
`checkFormatString` discards the evaluated value, and at phase 1 a
symbol-dependent expression has no value to inspect. Three shapes therefore
type-check and then fail at expansion, all three reported to the submitter as
a `SubmitValidationError` and none of them able to reach a task:

- a non-lone composition whose composed text is not valid range syntax
  (`range: ["x{{ 2.5 }}"]` → `invalid integer "x2.5"`);
- a lone text-arm result that is not valid `<IntRangeExpr>` syntax
  (`range: "{{ 'abc' }}"` → `invalid integer "abc"`);
- a lone empty list at any type (`range: "{{ [] }}"` → `range list is
  empty`, or `range produces no values` for CHUNK[INT]): an empty list is
  well typed; its length is what is wrong, and length is not a type.

The same text written as a literal `range` on a non-EXPR template is refused
too, so what differs is the layer and the JSON pointer, never whether a
malformed range is caught. The message matches only for the lone-text arm
(`{{ 'abc' }}` and a literal `abc` both give `invalid integer "abc"`); the
other two differ:

| shape | base-spec literal | EXPR path |
|---|---|---|
| lone text `{{ 'abc' }}` | `range expression "abc": invalid integer "abc"` | identical |
| non-lone `["x{{ 2.5 }}"]` | `value "x2.5" is not a valid integer` (validate) | `invalid integer "x2.5"` (expand) |
| lone empty `{{ [] }}` | `required` (validate), never reaching `range list is empty` | `range list is empty` (expand) |

All three are pinned by
`TestRangeCheckerResolverAgreement_KnownNonLoneDivergences`.

**A computed FLOAT's rendered text is not always what a task receives.**
Section 1.3.4 ("Float Value Pass-Through",
`wiki/2026-02-Expression-Language.md:981`) gives computed floats their
shortest round-tripping decimal string; `evalRangeExprField` and
`resolveRangeListEntry` follow that rule via `Value.String()`, so
`Param.Scale * 2` with `Scale = 2.5` renders `"5.0"`. For a FLOAT task
parameter that text is not the final value: `internal/openjd/expand.go`'s
`validateFloatList` re-canonicalizes every FLOAT `RangeList` entry with
`strconv.FormatFloat(f, 'g', -1, 64)` before a task row is built, which drops
the trailing `.0`, so a task receives `"5"`. This re-canonicalization is not
part of section 1.3.12 (it applies to a literal, non-EXPR FLOAT range entry
too).

**Conformance does not cover range resolution.** The `EXPR/job_templates`
conformance suite (`make test-conformance`) parses and validates a template
(`openjd.ValidateWithOptions`) and never expands a parameter space, so it
observes only whether the template is accepted. The three fixtures for this
feature (`expr1.3.11--float-range-expression.yaml`,
`expr1.3.11--string-range-expression.yaml`,
`expr1.3.11--path-range-expression.yaml`) pass on acceptance alone.
`internal/openjd/resolve_agreement_test.go` covers resolution and expansion:
it runs the real checker and the real resolver and expander against the same
templates and asserts they agree on every verdict.

## The wire protocol

The server → worker `AssignMsg` (`internal/worker/protocol/protocol.go`)
carries an envelope `Version` field, checked by the worker's lease loop
(`internal/worker/lease.decodeAssignment`) before the message is dispatched:
a version mismatch is rejected rather than decoded with unrecognized fields
dropped, so a mismatched worker cannot run a task with the wrong command line.
These fields carry what EXPR phase-3
evaluation needs; all nine are `omitempty`, so a base-spec assignment carries
none of them:

| Field | Purpose |
| --- | --- |
| `AssignMsg.EXPR` | Whether the template that produced this task declared the EXPR extension. Selects the worker's resolution path; see below. |
| `AssignMsg.JobName` / `StepName` | The template's own declared `Job.Name`/`Step.Name` (section 1.2.2 symbols), not derivable from any other field on the message. |
| `AssignMsg.ParameterTypes` / `JobParameterTypes` | Each task/job parameter's declared OpenJD type (`STRING`, `PATH`, `INT`, `FLOAT`, sqi's `CHUNK[INT]`), so `Task.Param.Count + 1` adds rather than concatenates. The type cannot be inferred from the value text, since `"1"` is a valid `STRING` parameter. |
| `AssignMsg.StepTemplateLet` / `StepScriptLet` | The step template's and step script's `let:` blocks (Template Schemas §3.6), as raw, unparsed `"name = expression"` source in declaration order, re-evaluated on the worker rather than shipped as resolved values; see below. |
| `AssignEnvironment.Let` / `StepEnvironment` | The same `let:` transport and scope tagging, per environment. |

The server builds every assignment (`internal/scheduler/assign.go`) and
populates these fields only for a template that declares EXPR. Version
enforcement runs in both directions: the worker checks the `AssignMsg` it
receives, and the server checks the `RegisterMsg`, `HeartbeatMsg` and
`TaskStatusMsg` it receives (`scheduler.discardOnVersionMismatch`), discarding
a mismatched message rather than half-reading it. `LogChunkMsg` and
`DeregisterMsg` are not checked: a mismatched worker is already refused at
registration, so neither can carry work-affecting state on its own.

## Worker behavior

### Choosing the resolution path

`msg.EXPR` selects between two resolution families at both places the
worker renders format strings: `internal/worker/executor/run.go`'s
`resolveAssignment` (the task's `onRun` action and step embedded files; the
environment variables it returns are `sess.StaticEnv()`, resolved at enter
time, on both branches) and `internal/worker/session/session.go`'s
environment-entry and environment-exit resolution.

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
embedded-file paths. `Env.File.*` belongs to `EnvSymbols` alone, the
environment-scope builder, which binds no `Task.*` family. Every path value is
bound under `expr.PathNative`: the worker runs on the host a task executes on,
so a Windows worker evaluates native Windows paths.

`internal/worker/fmtres.ResolveActionExpr` /
`ResolveEmbeddedFilesExpr` / `ResolveVarsExpr` then render each format
string through `expr.Eval` against that table, including
`apply_path_mapping()`, threaded via `expr.WithPathMapping` from the
assignment's `PathMap` rules, and the same phase-2/phase-3 target-type
inheritance rule (section 1.3.2): a format string that is exactly one
reference evaluates against the field's own target type, so a path-typed
value coerces to text via the field's target; an embedded reference
evaluates unconstrained and stringifies its natural result.

Every phase-3 evaluation is metered independently of phase 2's submission-time
limits, by the worker's own `expr.operation_limit` and `expr.memory_limit`
([Worker configuration → `expr`](../worker-configuration.md#expr--expr-expression-limits)).
Those are per-`Eval` budgets, which suffice for every position that renders a
result and drops it, but not for `let:`, the one construct that retains
values; see below.

### `let:` re-evaluation

`AssignMsg.StepTemplateLet` and `StepScriptLet` (and each
`AssignEnvironment.Let`) ship as raw, unparsed source, not pre-resolved
values, and the worker re-evaluates them itself
(`internal/worker/fmtres/exprsyms.go`'s `ApplyTaskLet`/`ApplyEnvLet`, applied
from `internal/worker/executor/run.go`'s `resolveAssignmentExpr` for the two
step-level blocks and from `session.go` for an environment's) in the order the
template declares: step-template bindings first, then the step script's, each
new binding visible to the ones after it, following Template Schemas §3.6's
ordering rule. A step template's own `let:` block could be resolved
server-side (its scope exposes only `Param.`, `RawParam.`, `Job.Name` and
`Step.Name`, all concrete at phase 2), but it ships raw too: its bindings are
the same table the step script's `let:` block evaluates against, so the worker
evaluates the whole ordered sequence in one place.

A `let:` block is the only phase-3 position that keeps its results, so a
per-`Eval` budget does not bound it: 50 bindings each within
`expr.memory_limit` retain 50 times it. `expr.let_retained_bytes` (default
10 MB) bounds the total section 1.3.9 size one symbol table holds live,
measured across every block evaluated into it: the step template's and the
step script's for a task, the step template's and the environment's own for
an environment entry. A block that would push the table past it stops there
with an error. This is the worker-local, per-table bound; the cumulative,
assignment-wide one is `expr.assignment_retained_bytes`, and the
template-wide one is the server's; see [Template-wide expression
budget](#template-wide-expression-budget).

### Phase-2/phase-3 differences

Three differences between the two phases are intended:

1. **`Session.PathMappingRulesFile` is bound conditionally at phase 3.** It
   appears only when the session has rules (`hasPathMap`), matching the
   base-spec `addPathMappingKeys` rule, so a reference to it with no rules
   fails as an unknown symbol rather than resolving to an empty path.
2. **`PathPOSIX` at phase 2, `PathNative` at phase 3.** Expression-Language
   §1.2.1: "In TEMPLATE scope contexts, POSIX semantics are used for
   consistency. In host contexts (SESSION and TASK scopes), the semantics
   match the host's operating system." Phase 3 is the host context.
3. **Path mapping is applied to `Param.<PATH>`/`Task.Param.<PATH>` only at
   phase 3.** §1.2.2 requires `Param.<name>` to carry the value "with path
   mapping rules applied"; phase 2 has no session, and therefore no rules, to
   apply.

### Phase-2/phase-3 agreement

Phase 2 (`internal/openjd/exprcheck.go`, submission-time type-checking) and
phase 3 (`internal/worker/fmtres`, worker-side evaluation) build their symbol
tables independently from the same rules, so sqi tests that they agree (the
same declared parameter type produces the same expression type on both
sides) for every job/task-parameter type EXPR supports (`STRING`, `PATH`,
`INT`, `FLOAT`, `CHUNK[INT]`, plus the derived `Session.*`/`Task.File.*` path
types). See `internal/worker/fmtres/exprsyms_test.go`'s
`TestPhase2Phase3Agreement` and
`internal/openjd/expr/paramtypes_internal_test.go`.

## Template-wide expression budget

Sections 1.3.9 and 1.3.10 of the expression language spec
(`wiki/2026-02-Expression-Language.md:1060` and `:1071`) each bound a single
evaluation: "the evaluator tracks the memory size of live values" and
"maintains a running operation count," reset for every `Eval` call. Every
`Eval` in sqi enforces both, independently, at every phase:
`openjd.expr_operation_limit`/`openjd.expr_memory_limit` (phases 1/2,
`internal/openjd/exprcheck.go`) and `expr.operation_limit`/`expr.memory_limit`
(phase 3, `internal/worker/fmtres/expres.go`). Phase 3 also bounds one symbol
table's own `let:` retention (`expr.let_retained_bytes`, default 10 MB,
`internal/worker/fmtres/exprsyms.go`).

None of that bounds the whole walk, and the spec asks only for a
per-evaluation bound. Without a cumulative bound, a template with thousands
of cheap positions, or many individually compliant `let:` blocks, has no
aggregate limit. The cumulative budget is charged once per position and once
per retained `let:` binding, summed across the entire walk rather than reset
per position or per table. It is an sqi mechanism on top of the spec's
per-evaluation bounds, enforced independently at three points, never shared
across phases (each call gets a fresh budget, so phase 2's verdict never
depends on what phase 1 spent):

1. **Server, phases 1 and 2: the checker.** `checkTemplateExpressions`'s own
   `templateBudget` (`internal/openjd/exprcheck.go`) caps:
   - `openjd.expr_template_positions`, default 10,000: one charge per
     format-string/`let:`-binding position walked.
   - `openjd.expr_template_retained_bytes`, default 10,000,000 (10 MB): the
     summed `expr.SizeOf` of every value a `let:` block adds to its table, the
     only construct in this walk that retains a value across positions. The
     default equals `expr.let_retained_bytes`' default (below). Configuration
     can separate them, and nothing compares these two dimensions across the
     binaries (the registration gate below pairs the template-wide byte budget
     with `expr.assignment_retained_bytes` instead).
   - Fresh per call: `ValidateWithOptions` (phase 1, unresolved parameters)
     and `checkExpressionsAtSubmit` (phase 2, concrete parameters) each get
     their own allowance.

2. **Server, phase 2: the resolver.** `ResolveParameterSpaceParams`
   (`internal/openjd/resolve.go`), threaded from `submit.go`'s
   `prepareTemplate`/`Submit`, spends its own `templateBudget`
   (`resolverBudget`), shared across every step's resolver call for one
   submission but separate from the checker's budget (`checkerBudget`). The
   resolver re-charges the same range positions and `let:` bytes the checker
   charged for the same submission, so one shared budget would halve the
   effective cap: a template `ValidateWithOptions` (phase 1) accepted could be
   rejected by `Submit` only because phase 2's two walks drew from one pool.

3. **Worker, phase 3: the assignment.** `AssignmentBudget`
   (`internal/worker/fmtres/assignmentbudget.go`), one per session (created
   once in `session.Manager.Create`, before any environment is entered, and
   shared by the task's own symbol table and every environment's) caps:
   - `expr.assignment_positions`, default 10,000, so that it is never the
     tighter of the two position caps (an assignment's positions are a subset
     of its template's, so a lower value here can fail every task of a job the
     server accepted).
   - `expr.assignment_retained_bytes`, default 20,000,000 (20 MB), twice
     `expr.let_retained_bytes`' default, sized for a task plus one environment
     near its own per-table ceiling.
   - **Charged only at environment entry, never at teardown.**
     `ExitEnvironments`'s re-evaluation of an environment's `let:` block
     (`session.go`'s `resolveEnvAction`) does not charge `s.exprBudget`: it
     takes a fresh budget per evaluation (`Session.teardownBudget`, carrying
     the operator's configured limits). The table `resolveEnvAction` builds is
     rebuilt and discarded whether or not the charge is accepted, and
     `ExitEnvironments` treats a resolve error as a warning and continues, so
     a budget that could trip at teardown would skip that environment's
     `onExit` (license check-ins, daemon shutdowns, unmounts) without
     preventing any memory use. Per-table (`expr.let_retained_bytes`) and
     per-`Eval` (`expr.operation_limit`/`expr.memory_limit`) bounds still apply
     to the teardown table at the operator's configured values; only the
     cross-table, assignment-wide ledger is exempt. The per-evaluation budget
     is fresh for the same reason: one shared teardown ledger would skip
     `onExit` the same way.

**What this budget does not measure:**

- **Operations are derived, not counted.** No operation counter crosses into
  `internal/openjd/expr` for either the template-wide or the assignment-wide
  budget; both count positions only. The server-side operation ceiling is a
  derived upper bound: `openjd.expr_template_positions ×
  openjd.expr_operation_limit` = 10,000 × 10,000 = 100,000,000 operations at
  the defaults, per walk, per phase. Because the ceiling is a product, raising
  either factor raises it proportionally (10¹⁰ at the two maxima, 100× the
  default). The same product exists on the worker
  (`expr.assignment_positions × expr.operation_limit`, 10¹⁰ at the defaults
  and 10¹² at the maxima).
- **Neither budget bounds wall-clock time.** An operation's real cost is not
  uniform: §1.3.10 rule 3 (`wiki/2026-02-Expression-Language.md:1090`) prices
  a string operation at the value's length divided by 256, so byte-heavy work
  is charged little, and two expressions charged the same number of
  operations can differ by an order of magnitude in CPU time. A template that
  respects every budget can still cost minutes of server CPU to evaluate. The
  wall-clock deadline (see [Known gaps](#known-gaps)) bounds time; the
  position and byte budgets bound memory and count. Each request may use up
  to `openjd.expr_submission_deadline` of CPU, and concurrent requests each
  get their own allowance, so an operator exposing an unauthenticated
  `POST /api/v1/jobs` should size request concurrency against the deadline,
  not against the position cap. `defaultTemplatePositions`' doc comment
  (`internal/openjd/exprcheck.go`) and
  `internal/openjd/expr/reservework_internal_test.go` list the costliest
  constructions known.
- **The byte dimension measures cumulative allocation, not peak live
  retention.** Both `openjd.expr_template_retained_bytes` and
  `expr.assignment_retained_bytes` sum every `let:` block's own charge across
  the whole walk, in evaluation order, without modelling when Go's garbage
  collector might reclaim an earlier block's values. A template whose `let:`
  blocks never hold more than a few MB live at once, but which declares many
  such blocks in sequence, is charged for their sum: four sequential blocks
  that each retain 2.7 MB are charged 10.8 MB.
- **A single `let:` block can transiently exceed the server-side 10 MB
  figure before the budget sees it.** The charge lands once, after
  `checkLetBindings` finishes evaluating a whole block, not per binding
  within it, so one block can retain up to `maxLetBindings ×
  openjd.expr_memory_limit` = 50 × 1,000,000 = 50,000,000 bytes (50 MB)
  at the defaults before this counter rejects it. That single-block ceiling
  moves with `openjd.expr_memory_limit`, not with
  `openjd.expr_template_retained_bytes`. The worker's equivalent is
  `50 × expr.memory_limit`, 1 GB at that key's default; that product, not the
  key itself, is the number to size a host's RAM against.

**`maxSteps` = 100** (`internal/openjd/validate.go`) caps the number of
`<StepTemplate>` entries any job template may declare, including base-spec
templates with no `extensions:`. It is not from the spec: OpenJD's
"Constraints" list for `<JobTemplate>` gives `parameterDefinitions` an
explicit min/max (`wiki/2023-09-Template-Schemas.md:62`–`64`, item 6) but
states none for `steps` (`wiki/2023-09-Template-Schemas.md:71`, item 8). It is
gated by `openjd.enforce_limits` (`docs/configuration.md`) like the package's
other structural-count caps, unlike the always-on resource-exhaustion guards
(`maxRangeValues`, `maxTasksPerStep`), and it is a compiled constant.
`openjd.expr_template_positions`' default is sized against a per-step position
count times `maxSteps`; `defaultTemplatePositions`' doc comment
(`internal/openjd/exprcheck.go`) has the arithmetic.

**Task count is a different dimension from expression cost.** These budgets
bound what expressions may cost to evaluate, not how many tasks a parameter
space may expand to. A template whose steps each declare a large `range`
(e.g. `"1-1000000"`) validates cheaply and then multiplies: `maxSteps` (100) ×
`maxTasksPerStep` (1,000,000) is 10⁸ task rows, and with
`openjd.enforce_limits: false` the step count is unbounded.

`maxTasksPerJob` (`internal/openjd/expand.go`) bounds that product: it caps
the tasks a whole job may produce, summed across steps, at 1,000,000, the
same figure as `maxTasksPerStep`, so a job may not produce more tasks than a
single step may.

- **It is always on**, not gated by `openjd.enforce_limits`: with the flag
  off the step count is unbounded, so a gated cap would leave that case
  uncovered.
- **It applies to every template, not only EXPR ones.** A job of two
  600,000-task steps is rejected whether or not it declares
  `extensions: [EXPR]`. It is a compiled constant, like its sibling.
- **A breach writes nothing.** The cap is charged per step during expansion,
  which runs to completion in memory before the single submission write, so a
  two-step job of 600,000 + 600,000 is rejected with no rows persisted.

See `maxTasksPerJob`'s doc comment (`internal/openjd/expand.go`) for the
mechanism.

## Operator configuration

All nine limits above are operator configuration. The full reference, with
each key's range and what happens at the bound, is in the two configuration
guides; this table maps them:

| Bounds | Server key | Worker key |
|---|---|---|
| operations, one evaluation | [`openjd.expr_operation_limit`](../configuration.md#openjdexpr_operation_limit) | [`expr.operation_limit`](../worker-configuration.md#exproperation_limit) |
| live bytes, one evaluation | [`openjd.expr_memory_limit`](../configuration.md#openjdexpr_memory_limit) | [`expr.memory_limit`](../worker-configuration.md#exprmemory_limit) |
| positions, one walk / one assignment | [`openjd.expr_template_positions`](../configuration.md#openjdexpr_template_positions) | [`expr.assignment_positions`](../worker-configuration.md#exprassignment_positions) |
| retained bytes, one walk / one assignment | [`openjd.expr_template_retained_bytes`](../configuration.md#openjdexpr_template_retained_bytes) | [`expr.assignment_retained_bytes`](../worker-configuration.md#exprassignment_retained_bytes) |
| retained bytes, one symbol table | [`openjd.expr_template_retained_bytes`](../configuration.md#openjdexpr_template_retained_bytes) *(the walk-wide bound that upper-bounds one table)* | [`expr.let_retained_bytes`](../worker-configuration.md#exprlet_retained_bytes) |

A tenth key is not in that table:
[`openjd.expr_submission_deadline`](../configuration.md#openjdexpr_submission_deadline)
is a wall-clock backstop rather than a budget. It decides only whether this
server keeps working on a request (`503`), never whether a template is valid,
so none of the three properties below applies to it: it has no worker
counterpart and no cross-binary gate. See [Known gaps](#known-gaps).

Three properties of these keys:

1. **`0` never means "unlimited".** Each key has a validated floor and
   ceiling; an out-of-range value fails startup with a message naming the
   bound, rather than being clamped. On the server, tightening only rejects
   work at submit, on the request that can report it, so the floors sit above
   what the reference presets (`presets/sqi/*.yaml`) need;
   `TestExprLimits_FloorsAcceptReferencePresets` checks this. On the worker,
   tightening rejects work after the job was accepted, so the floors are
   higher: the server's own defaults for the two per-evaluation dimensions,
   and 2,000 positions.

   A worker at its floors is not guaranteed to run everything this server
   accepts. The worker floors are fixed numbers sized against the server's
   defaults; the server's limits are configurable up to 10× those defaults,
   and no worker reads them. `expr.let_retained_bytes`' floor is a tenth of
   `openjd.expr_template_retained_bytes`' default. Setting a worker to its
   floors and raising a server key is supported, and what happens then is
   property 3: the work is withheld, visibly, rather than failing per task
   after acceptance.
2. **The ceilings are of two kinds.** Catastrophe ceilings, one order of
   magnitude above the default, bound the dimensions that otherwise allow
   multi-minute requests and multi-gigabyte heaps. The rest are policy
   ceilings, wide but finite, sized against what a legitimate template needs.
3. **The server↔worker relation is enforced at runtime, not at load.** Each
   worker advertises all five of its caps in its registration message; the
   server persists them and the scheduler withholds EXPR work from any worker
   whose caps undercut the limits this server accepts templates under. The
   refusal is per job, not per host: a tightened worker keeps running every
   base-spec job. Neither binary can validate the relation on its own: the
   worker does not read or receive the server's configuration.

   The fifth dimension, per-table retained bytes, has no server-side
   counterpart, because the server meters no per-table scope. The
   template-wide retained-bytes budget is compared instead, as an upper bound
   on any one table: without that comparison a worker at its
   `expr.let_retained_bytes` floor would accept, and then fail once per task,
   a `let:` block the server had accepted. Comparing against the
   per-evaluation memory limit would be unsound (a table accumulates: eight
   1 MB bindings are accepted by a default server and rejected by a 1 MB
   table), and comparing against the `50 ×` form would exceed the worker key's
   own legal maximum. `internal/scheduler/exprcaps.go` carries the arithmetic.

**The gate identifies an EXPR job by reading the extension list persisted on
the job row** (`jobUsesEXPR`, `internal/scheduler/exprcaps.go`).
`jobs.declared_extensions` is filled at submission from the parsed and
registry-validated declaration, so the test is exact, including for a
declaration spelled with an escape (`"EXPR"`), which decodes to `EXPR`, is
accepted, and is stored verbatim in the raw template. The column is on the
same `SELECT` the lease path issues for each candidate task, so reading it
costs no extra query.

The column is `TEXT NOT NULL DEFAULT ''`, and `''` (not recorded) is a
different value from `'[]'` (recorded, declares nothing). For a row holding
`''`, the gate falls back to a byte scan (`jobMayUseEXPR`): the raw template
must contain both the bytes `EXPR` and the bytes `extensions`. That fallback
over-matches (a template declaring another extension and mentioning `EXPR`
elsewhere still matches), and an obfuscated declaration escapes it.

## Known gaps

- **End-to-end coverage is one job per harness.**
  `test/integration/expr_realworker_test.go` (`TestEXPRJobEndToEnd`) submits
  an `extensions: [EXPR]` job to a real server and runs it on a real worker,
  and `scripts/smoke.sh` submits a second one against the shipped binaries.
  Both assert on a value only the worker can produce: the integration job on
  `{{ upper(Param.Shot) + '.' + zfill(Task.Param.Frame * 3, 4) }}`, whose
  `Task.Param.Frame` the server cannot resolve, plus an
  `is_absolute(Session.WorkingDirectory)` that runs the path engine against
  the worker's own session directory. Submitted list-parameter values are
  covered by `test/integration/expr_paramtypes_test.go`, which calls
  `openjd.Parse`/`openjd.BindJobParameters` directly.
- **The `EXPR/jobs/` conformance fixtures are not scored.**
  `test/conformance/suite_test.go` walks only `job_templates` and
  `env_templates`, and returns early for a job test, so the runtime half of
  the extension (submitted parameter values, `Param.<name>[i]`,
  `list(Param.FrameRange)`) is covered only by sqi's own tests.
- **The worker cannot report an over-cap `let:` block.** It has no
  `validateLetElementCounts` equivalent; it truncates at 50 bindings without
  reporting it, relying on phase 2 having rejected the template.
- **Wall-clock time is bounded by a deadline, not by the counters.**
  `openjd.expr_submission_deadline` (default 5s, range 1s–60s) gives one
  submission a fixed wall-clock allowance for its expression evaluation and
  stops it when that passes, on every route that validates a template: the
  two job routes, the two product create/update routes, and the two preset
  routes
  ([`docs/configuration.md`](../configuration.md#openjdexpr_submission_deadline)
  lists them and what each answers for an invalid template). It bounds
  elapsed time directly, whatever the operation pricing permits.

  At the default limits the counters reject a template before the deadline
  is reached, and the binding dimension for realistic templates is retained
  bytes, not positions; a large legitimate template (100 steps of 1,000
  frames with `let:` bindings) evaluates in under a second. At the legal
  maximum limits a legitimate template can run past 5 s and get
  `503 … deadline exceeded`, so raise this key along with the four limits
  ([`docs/configuration.md`](../configuration.md#openjdexpr_submission_deadline)).

  A breach is reported as `503`, never `4xx`. The counters are deterministic,
  so a template that breaches one breaches it everywhere and the submitter is
  told the template is invalid; a wall-clock stop depends on load, and the
  same body would be accepted on an idle host. It is matched structurally on
  `expr.ErrDeadlineExceeded`, never by reading a message. `internal/api`'s
  deadline tests drive a real `Submitter` through the handler and assert a
  real `503` on the job, product and preset routes.

  A request refused at the deadline still spent that time, and concurrent
  requests each get their own allowance: the deadline bounds one request, not
  a host.
- **The cross-binary gate is necessary, not sufficient.** A worker at parity
  with the server can still exhaust its own budget on a concrete value phase
  2 had only a placeholder for, and the per-table dimension is compared
  against a walk-wide bound that measures a different scope in both
  directions (it ignores the worker's whole-table accounting of job
  parameters, and over-counts a template that spreads its budget across many
  steps). The worker's defaults leave room for that residual; an accepted job
  is not guaranteed to run.
- **A worker's advertised caps are not shown in the API or the web UI.**
  They are persisted on the worker row; an operator diagnosing why a host gets
  no EXPR work reads the registration warning or the task's unschedulable
  reason.
- **Workers and the server must run the same protocol version.** The server
  discards a `RegisterMsg` whose `Version` differs from its own, so a
  mismatched worker is not registered and is offered no work; the server logs
  the mismatch with both versions. The worker also rejects any `AssignMsg`
  whose `Version` differs from its own.
- **The version check covers four of six channels.** `AssignMsg`
  (worker-side), plus `RegisterMsg`, `HeartbeatMsg` and `TaskStatusMsg`
  (server-side). A discarded task status is the one that costs something: the
  server never learns the task finished, so it is reclaimed and retried. That
  message carries the outcome, and writing a misread outcome to a task is
  worse than a re-run. `LogChunkMsg` and `DeregisterMsg` are not checked.
- **Three `EXPR/job_templates` fixtures are baselined, and none of them is an
  EXPR shortfall.** `3.6--let-bindings.yaml`,
  `3.6--let-host-context-symbols.yaml` and
  `7.3.1--job-step-name-in-step-let.yaml` are valid templates that sqi
  rejects, because all three declare `FEATURE_BUNDLE_1` (RFC 0004) alongside
  `EXPR` and use that extension's `bash:` `SimpleAction` in place of
  `script:`. sqi does not register `FEATURE_BUNDLE_1`, so `validateExtensions`
  refuses the template at its other extension and its `let:` subject matter is
  never reached. These three stay baselined, with that reason recorded in
  `test/conformance/baseline.txt`, while sqi does not implement RFC 0004.
