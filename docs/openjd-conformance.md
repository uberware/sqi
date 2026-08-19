# OpenJD Spec Conformance

This page states, in one place, exactly how far `sqi`'s OpenJD support goes: which
spec version it implements, where that's enforced in code, which extensions it
understands, and — just as importantly — what it deliberately does not implement
and why that is correct rather than a gap. Read this before re-auditing OpenJD
conformance from scratch.

## Spec version

`sqi` implements **`jobtemplate-2023-09`** — the OpenJD job template schema. A
template's `specificationVersion` field must equal that string exactly; anything
else (missing, a different version string) is a `422` validation error.

`sqi` does **not** implement the standalone `environment-2023-09` template type — a
second, separate top-level document format in the OpenJD spec for defining
reusable environments outside a job template. `sqi` only ever parses
`jobtemplate-2023-09` documents; environments are supported only as they appear
embedded in a job template (`jobEnvironments`, `stepEnvironments`). This is a
missing feature, not a violated rule — nothing in the spec requires an
implementation to support both template types.

## Where validation lives

The pipeline is parse, then validate, then expand:

1. **`internal/openjd/parse.go`** (`Parse`) — decodes YAML or JSON into a
   `*JobTemplate` (`internal/openjd/model.go`). Parsing is strict about shape:
   a field that must be a scalar but arrives as a mapping or sequence is a parse
   error, not a silently-wrong value.
2. **`internal/openjd/validate.go`** (`Validate`, `ValidateWithOptions`) — walks
   the parsed template and returns zero or more `ValidationError`s, each a JSON
   Pointer (RFC 6901) to the offending field plus a message. `Validate(t)` is
   `ValidateWithOptions(t, ValidateOptions{EnforceLimits: true})`; the submission
   pipeline can run with `EnforceLimits: false` (quantitative caps such as range
   size relaxed) while every *structural* correctness check — required fields,
   dependency resolution, extension gating, host-requirement shape — still runs
   unconditionally. A structural check that only fired under `EnforceLimits: true`
   would silently vanish for any caller that flips the flag; that class of bug is
   exactly what Task 4 of this cycle closed.
3. **`internal/openjd/expand.go`** — turns a validated template's parameter
   spaces into concrete tasks.

A `422 Unprocessable Entity` from `POST /api/v1/jobs` carries a `detail` string
built from `ValidationErrors.Error()` — one or more `<pointer>: <message>` entries
joined with `; `. See [`docs/openjd-submission.md`](openjd-submission.md#validation-errors)
for a real captured example.

## Supported extensions and the vendor-prefix rule

OpenJD extensions are **opt-in**: a template lists the ones it needs in its
top-level `extensions: [...]` array. `sqi` validates that array unconditionally
(not gated by `EnforceLimits`) against a fixed registry
(`internal/openjd/extension.go`, `LookupExtension`):

| Extension | Origin | What it does |
|---|---|---|
| `TASK_CHUNKING` | official | Chunked integer task parameters (`CHUNK[INT]`). See [`docs/openjd-extensions/task-chunking.md`](openjd-extensions/task-chunking.md). |
| `REDACTED_ENV_VARS` | official | The `openjd_redacted_env` stdout directive that redacts a variable's value from logs. See [`docs/openjd-extensions/redacted-env-vars.md`](openjd-extensions/redacted-env-vars.md). |
| `EXPR` | official | The expression language: a Python subset in format strings, `let` bindings, the RFC 0006 function library, and the RFC 0007 parameter types. Supported since sub-project H2 — see [EXPR](#expr) below and [`docs/openjd-extensions/expr.md`](openjd-extensions/expr.md). |
| `SQI_PATH_TRANSLATION` | vendor | Per-product path-delivery checklist (`swap_in_place`/`translation_file`/`command_flags`/`environment`/`stage_locally`). See [`docs/openjd-extensions/path-translation.md`](openjd-extensions/path-translation.md). |
| `SQI_CHUNK_BOUNDS` | vendor | Exposes a `CHUNK[INT]` chunk's first/last integer as `Task.Param.<name>.Start`/`.End`. See [`docs/openjd-extensions/sqi-chunk-bounds.md`](openjd-extensions/sqi-chunk-bounds.md). |

Every **vendor** extension (`Origin: OriginVendor` — one `sqi` defines itself,
as opposed to one specified upstream by OpenJD) name **must** carry the `SQI_`
prefix. This is enforced by a test invariant
(`internal/openjd/extension_test.go`), not just convention: it guarantees a
vendor extension name can never collide with a future official OpenJD name,
which are always bare identifiers (e.g. `TASK_CHUNKING`, not `SQI_TASK_CHUNKING`).
If a vendor extension is later upstreamed into the OpenJD spec, the promotion
path is to drop the `SQI_` prefix and flip its `Origin` to `OriginOfficial` — the
registry entry moves, it doesn't get a second copy.

Declaring an extension name not present in the registry is a `422` at
`/extensions/{i}`, regardless of `EnforceLimits`. Declaring `CHUNK[INT]` without
`TASK_CHUNKING` in `extensions` is likewise rejected — the extension gate and the
feature it gates are checked together.

## What `sqi` deliberately does not implement

Two entries. Standalone `environment-2023-09` templates are **out of scope by
design**, not a bug to fix. The other is a **known defect** that is merely not
fixed *here* — it is listed alongside so a reader auditing this section against
the spec does not mistake it for intended behavior:

**CORRECTION (sub-project H2, 2026-08-14): `EXPR` used to head this list and no
longer belongs on it.** Every earlier revision of this section said a template
declaring `extensions: [EXPR]` was rejected with a `422` at `/extensions/0`
"unconditionally, regardless of anything else the template does", and carried
two corrections narrowing that claim as sub-projects F1 (the RFC 0007 parameter
types) and E3 (`let` bindings) landed the pieces without making any of them
reachable. H2 flipped `EXPR` to `StatusSupported` in
`internal/openjd/extension.go`. The extension is **accepted in production**: an
`EXPR` template is parsed, validated, scope-checked, expression-checked at
submission with concrete parameters, and resolved on the worker at run time.
Nothing about EXPR is "reachable only through the conformance suite" any more.
See [EXPR](#expr) below.

- **`Job.Name` / `Step.Name` in `hostRequirements` and `parameterSpace`** — a
  **known gap against section 7.3.1**, present since sub-project E2 and *not*
  closed by E3. `checkStepExpressions` evaluates both of those positions at
  `ScopeJob`, whose fixed-symbol set (`scopeFixed`, `internal/openjd/scope.go`)
  is empty, so a bare `Job.Name` or `Step.Name` there is rejected as an unknown
  symbol. Reproduced at `3e2f87c`, all three verbatim:

  ```
  /steps/0/hostRequirements/attributes/0/anyOf/0: col 1: unknown symbol "Step.Name"
  /steps/0/hostRequirements/attributes/0/anyOf/0: col 1: unknown symbol "Job.Name"
  /steps/0/parameterSpace/taskParameterDefinitions/0/range/0: col 1: unknown symbol "Step.Name"
  ```

  Section 7.3.1's symbol table grants both: `Step.Name` is "Available within
  the Step Template scope: `stepEnvironments`, `hostRequirements`,
  `parameterSpace`, and `script`", and `Job.Name` is "Available in every Format
  String in the Job Template, except the `name` field of the Job Template
  itself". Section **3.6.2 does not authorize the rejection** and must not be
  cited for it — 3.6.2 governs where *`let` names* are visible and is silent on
  the fixed symbols. A comment in `exprcheck.go` did cite it that way until this
  fix wave retracted it. **No fixture covers this**, which is why it survived
  both E2 and E3 unnoticed and why it does not show in the scores below.
  Closing it means splitting `scopeFixed(ScopeJob)` or adding a distinct scope
  for these two positions — a change that can move conformance scoring, so it
  belongs to sub-project E4 or a dedicated follow-up, not to a closing fix wave.
  **Sub-project H2 raised its severity without changing its content:** EXPR is
  now supported, so a submitted template hitting this gap gets a `422` a real
  user sees, where before the extension's own status gate rejected it first.
- **Standalone `environment-2023-09` templates** — see [Spec version](#spec-version)
  above.

**Why rejecting an unimplemented opt-in extension is correct, not a gap:**
OpenJD extensions exist precisely so a template can declare "I need this
capability" and a conformant implementation that lacks it can say "then I can't
run you" instead of guessing. Accepting a template whose syntax `sqi` cannot
interpret — silently ignoring the parts it doesn't understand — is strictly
worse than refusing it outright: the alternative is a job that appears to
submit successfully and then does something other than what the template
author asked for, discovered only when the render is wrong. The spec does not
mandate rejection of an unimplemented extension either way; `sqi` chooses to
reject because a loud `422` at submission time is a better failure mode than a
silent misinterpretation at run time.

### EXPR

`EXPR` is a **supported** extension as of sub-project H2 (2026-08-14): a
template declaring `extensions: [EXPR]` is accepted, and `EXPR/job_templates`
is scored by `TestConformance_Templates` through the same
`openjd.Parse` + `openjd.ValidateWithOptions` path as every other live
directory. It scores **206 / 209 pass, 3 baselined**.

**This section used to be titled "EXPR: a temporary, second scoring path" and
described the arrangement that made scoring possible before the flip.** That
arrangement is gone, and the summary of it is kept short here rather than
deleted outright, because two long-lived test files still refer to it:

- `EXPR` was in the registry with `Status: StatusInProgress` from sub-project E2
  (`111b0b2`) until H2. Production rejected every `EXPR`-declaring template on
  that status alone, at `/extensions/{i}`.
- Because that one error rejected the template whatever else was in it, and 180
  of the 209 fixtures are marked `.invalid`, scoring the directory through the
  ordinary path would have reported 180 passes for the wrong reason. So the
  suite scored it separately: `test/conformance/exprcase.go`'s `RunExprCase`
  (originally an expression-only reader that never called `openjd.Parse` at
  all), `TestConformance_Expressions`, and a baseline file of its own,
  `baseline-expr.txt`. E2's Task 2 moved the score onto the real path behind a
  wrapper that discounted only the status-gate error; H2 deleted all of it —
  `exprcase.go`, `exprcase_test.go`, `baseline-expr.txt`,
  `TestConformance_Expressions`, `TestConformance_EXPRNotSupported` and the
  discount — because with `EXPR` supported the discount always discounts zero
  errors. The history of what moved the score, fixture by fixture and wave by
  wave, is in `baseline-expr.txt`'s own dated notes; read them in git.
- The score did **not** move when the paths were swapped: 206/209 on both,
  measured before and after.

**The three baselined fixtures are not EXPR failures.** All three are valid
templates that `sqi` rejects, and all three do it for the same reason:
they declare `FEATURE_BUNDLE_1` — a separate official OpenJD extension
([RFC 0004][rfc0004]) `sqi` does not register — and use that extension's `bash:`
`SimpleAction` in place of `script:`, so each reports both an unsupported
extension name and a missing required `script`. They are
`3.6--let-bindings.yaml`, `3.6--let-host-context-symbols.yaml` and
`7.3.1--job-step-name-in-step-let.yaml`; per-fixture measured errors are in
`test/conformance/baseline.txt`. They come off the list when `sqi` implements
`FEATURE_BUNDLE_1`, or not at all. `<SimpleAction>.let` — the fourth `let`
location — is unreachable for the same reason; the other three
(`<StepTemplate>.let`, `<StepScript>.let`, `<EnvironmentScript>.let`) are
implemented and enforce section 3.6's rules.

**A correction the flip forced, worth stating because the old claim is repeated
in several places:** `3.6--let-bindings.yaml` was described until now as blocked
*first* by its `LIST[INT]` job parameter, rejected at parse time before
validation ran. Sub-project F implemented the RFC 0007 parameter types, so that
is no longer true — it parses, and only the `FEATURE_BUNDLE_1` gap remains. The
same correction applies to the five `3.6--let-comprehension-shadows*.invalid.yaml`
fixtures, which are **not** baselined because they pass: four of them used to
pass for the wrong reason (rejected at parse time for `LIST[INT]`), and now pass
because section 1.3.7's real loop-variable-shadowing rule rejects them, at the
position that carries it. The fifth uses a `bash:` `SimpleAction` and is still a
`FEATURE_BUNDLE_1` rejection. `TestConformance_B3ProtectedFixtures` records
which is which.

**Two other fixtures pass for a reason narrower than the rule they name**, both
recorded here because the aggregate score cannot show it:

`expr1.3.9--memory-limit-exceeded` (`{{ 'a' * 100000000 }}`) passes, but not
because section 1.3.9's memory limit rejected it. That limit *is* implemented
and is operator configuration
([`openjd.expr_memory_limit`](configuration.md#openjdexpr_memory_limit),
default 1,000,000 bytes) — but this fixture never reaches it: measured, the
expression is refused with the same error at the submission defaults and with
no limit options at all. It passes because `internal/openjd/expr` carries a
hard, non-configurable safety bound (`limits.go`'s `maxStringBytes`, 10,000,000
bytes, applied by `checkRepeat`), and that bound rejects the expression.
**String repetition is implemented** and has been since sub-project B2 —
`ops.go`'s `OpMul` table registers `{TString, TInt} -> repeatString`, and
`'a' * 3` evaluates to `"aaa"`. An earlier revision of this paragraph said the
fixture passed because `__mul__(string, int)` was unimplemented and predicted
the entry would start failing once repetition shipped; repetition shipped, the
entry kept passing, and the stated mechanism was wrong. The conclusion is
unchanged: this is not a section 1.3.9 pass, because a hard per-operation
ceiling is not a memory budget.

`7.3--apply-path-mapping-in-timeout.invalid.yaml` is rejected by `decodeAction`
(`internal/openjd/parse.go`), which decodes `timeout` with a strict integer
parse, so `openjd.Parse` fails with `openjd: timeout must be an integer` before
validation — and therefore before the scope model — runs at all.
`checkActionExpressions` does carry a timeout position, but it is
wired-and-unreachable for any real template; see the standing comment above that
function in `internal/openjd/exprcheck.go`. Its sibling
`7.3--apply-path-mapping-in-job-name.invalid.yaml` *is* the scope model's doing:
`apply_path_mapping` is host-context-only and the job name field is not a host
context. Both were predicted to clear together when sub-project E landed a scope
model; only one did, and this paragraph exists so the other is not miscredited.

**Protected-fixture tests.** The score is an aggregate, and an aggregate cannot
see one fixture regressing while another starts passing for an unrelated reason.
Seven tests in `test/conformance/suite_test.go` pin fixtures by name against
exactly that swap: `TestConformance_{B3,C1,C2,C3,C4,E2,E3}ProtectedFixtures`.
Each entry carries a "why" string naming the rule it depends on, and each test's
doc comment states plainly how much its entries actually pin — several are
rejection *floors* rather than proofs that a named mechanism still fires,
because `conformance.Result` blanks `Reason` the moment a fixture passes.
`TestConformance_E3ProtectedFixtures` goes further than the rest, asserting each
fixture's exact validation-error text by re-running the pipeline on the side.

[rfc0004]: https://github.com/OpenJobDescription/openjd-specifications/blob/mainline/rfcs/0004-enhanced-limits-and-capabilities.md

This is also why the portability claim in `README.md` and `ROADMAP.md` carries an
explicit caveat rather than being dropped. The measured position is narrower and
more useful than a slogan: `sqi` currently accepts **every valid base-spec
template in the conformance suite** — no conforming job template is turned away.
Both remaining classes of gap are elsewhere:

- **Unimplemented extensions**, rejected by name at submission time, as argued
  above. A template hits this only by opting in explicitly. This is what the
  three baselined `EXPR/job_templates` fixtures are: valid templates turned away
  because they *also* declare `FEATURE_BUNDLE_1`.
- **Over-permissiveness** — templates the spec says are *invalid* that `sqi`
  nonetheless accepts. As of the measurement below there are **none**: every
  `.invalid` template-validation fixture in the suite is correctly rejected.
  `test/conformance/baseline.txt` holds only the three under-permissive
  `FEATURE_BUNDLE_1` cases above, and CI fails if any entry is added without
  being fixed, removed once it starts passing, or left matching nothing.

### Extended parameter types (RFC 0007)

Sub-project F1 implemented [RFC 0007][rfc0007] — the EXPR extension's extended
parameter types — on the template side. `EXPR/job_templates` moved **186/209
(23 baselined) → 206/209 (3 baselined)**: twenty fixtures, measured, not
predicted.

[rfc0007]: https://github.com/OpenJobDescription/openjd-specifications/blob/mainline/rfcs/0007-extend-parameter-types.md

What landed:

- **Case-insensitive type names** for every type, job and task, including
  compound ones (`list[list[int]]`). Gated on `extensions: [EXPR]`: without it
  `type: int` is still rejected exactly as before, because RFC 0007 is an
  extension specification and accepting lowercase unconditionally would widen
  what the base spec admits.
- **`BOOL`**, with the RFC's accepted-values table (`true`/`1`/`1.0`/`yes`/
  `on` and their negatives) and its explicit prohibition on `allowedValues`.
- **`RANGE_EXPR`**, validated against the `<IntRangeExpr>` grammar under the
  **specification's permissive policy** rather than `internal/openjd`'s
  stricter one. That divergence is deliberate and documented on
  `validateRangeExprParamConstraints`: the fixture declares `"10-1:-1"` and
  `"-1--10:-1"`, which the strict policy rejects, and the permissive policy is
  what the value meets downstream anyway. Literal base-spec range text keeps
  the strict policy, unchanged.
- **Six `LIST[*]` types** with nested `item:` constraints, one level deep.
  A list default is stored as canonical JSON in the existing `Default` field;
  element checking is by JSON **type**, which is what the RFC's `<string>` /
  `<integer>` schemas actually constrain.
- **The `*_LIST` controls**, plus the two rules written against the scalar spin
  box that had to learn its list form (`singleStepDelta`, `decimals`).
- **The `<ArgString>` control-character amendment** — CR, LF and TAB become
  legal in an argument when `EXPR` is declared, "to support multi-line
  expressions in YAML literal block scalars". `<CommandString>` is a separate
  type and is **not** amended, so a command keeps the base-spec rule.

**Three fixtures remain baselined, and none of them are F1's.**
`3.6--let-bindings.yaml`, `3.6--let-host-context-symbols.yaml` and
`7.3.1--job-step-name-in-step-let.yaml` are each blocked by the unregistered
`FEATURE_BUNDLE_1` extension and an unmodeled `bash:` action type.
`3.6--let-bindings.yaml` is the one worth naming: it was on F1's own list of
sixteen because a `LIST[INT]` parameter blocked it at parse time, that blocker
is gone, and it still does not pass. Fifteen of the sixteen cleared; the
sixteenth had a third cause behind the two that were visible.

**Sub-project F2 completed the runtime half** — submitted-value validation,
element-wise `loc://` resolution, and per-element staging and path mapping for
`LIST[PATH]` — with the `EXPR/job_templates` score unchanged **by design**:
F2 touched no template-level parse or validate behavior, only code paths a
conformance fixture (parse-and-validate only, never expand or submit) cannot
see. `206/209, 3 baselined` is F1's number, still current; a reader should not
look here for movement F2 was never going to produce.

### A second, independent check on EXPR: the reference-implementation oracle

The fixture suite scores whether `sqi` reaches the right verdict on the
templates upstream happens to ship. It cannot tell you whether the *evaluator*
agrees with upstream on an expression no fixture contains, and it cannot catch a
misreading of the spec applied consistently across `sqi`'s own tests.

`make test-expr-oracle` covers that gap by evaluating a corpus with both `sqi`
and the implementation the EXPR spec names as its reference, and comparing.
It is supplementary to this suite, never a substitute — and the reference is
**Beta**, so the spec still outranks it. Setup, the baseline format, and the
currently accepted divergences are documented in
[`docs/development.md`](development.md#differential-testing-expr-against-the-reference-implementation).

## Known divergence: sqi names under the reserved `worker` scope

The spec reserves `worker`, `job`, `step`, and `task` as the first identifier
after a capability namespace, "for use in this and future revisions" (§3.3.1.1).
`sqi` validates that: a name under a reserved scope must be one the spec defines,
so `amount.worker.custom` is rejected.

Four `sqi` names are exempted, and this is a **real divergence, not a rule**:

| Name | Backs |
|---|---|
| `attr.worker.tag.*` | [worker capability tags](worker-capabilities.md) — every shipped DCC preset gates on one |
| `amount.worker.usagepool.*` | [usage pools](openjd-submission.md#4-usage-pools) |
| `attr.worker.computelocation` | [compute locations](compute-locations.md) |
| `attr.worker.os.version` | OS version matching |

These predate the check and are load-bearing; enforcing the spec strictly would
invalidate `sqi`'s own presets. The conformant fix is to move them behind a
vendor prefix — `sqi:attr.worker.tag.nuke`, which the validator already accepts
(see [the vendor-prefix rule](#supported-extensions-and-the-vendor-prefix-rule))
— but that renames a capability every existing template and worker config uses,
so it is a breaking change that has not been made. The exemption list lives in
`sqiReservedScopeNames` in `internal/openjd/validate.go`.

## Adding a deliberate divergence

If `sqi` ever needs to diverge from strict spec conformance on purpose — the
concrete example on the table right now is restoring something like the
non-standard `CHIP_INPUT` parameter control that Task 9 of this cycle removed
— the route is the extension mechanism above, not a silent parser change:

1. A registry entry in `internal/openjd/extension.go` naming the divergence,
   e.g. `SQI_UI_CONTROLS`, with `Origin: OriginVendor` (so the `SQI_` prefix is
   mandatory and enforced by the test invariant).
2. Parse/validate support in `internal/openjd` gated on that extension being
   declared — the divergent behavior only activates for a template that opts
   in by name.
3. A doc under `docs/openjd-extensions/`, following the shape of the existing
   entries (motivation, schema, validation, worker behavior).
4. Any worker-side behavior the divergence needs, under `internal/worker/`.

`SQI_UI_CONTROLS` is not built — nothing declares it, nothing depends on it.
It is recorded here only as the shape a future divergence would take: opt-in,
named, registered, documented — never a bare change to what the base spec's
syntax means.

## Measured conformance

`sqi` runs the official [OpenJD conformance test
suite](https://github.com/OpenJobDescription/openjd-specifications/tree/mainline/conformance-tests)
on every CI build, from a pinned submodule (commit `42a1fb674c94`). These are
measured results, not assertions:

| Suite | Result |
|---|---|
| `base/job_templates` | **449 / 449 pass** |
| `base/env_templates` | not applicable — standalone environment templates unsupported (39 tests) |
| `TASK_CHUNKING/job_templates` | **11 / 11 pass** |
| `REDACTED_ENV_VARS` | no template fixtures — the suite ships only `jobs/` (8 job-execution tests), which are out of scope; see **Scope** below |
| `EXPR/job_templates` | **206 / 209 pass, 3 baselined** — see [EXPR](#expr) |
| `EXPR/env_templates` | not applicable — standalone environment templates unsupported (6 tests) |
| `FEATURE_BUNDLE_1/job_templates` | not applicable — extension not registered (41 tests) |
| `FEATURE_BUNDLE_1/env_templates` | not applicable — extension not registered (4 tests) |
| `WRAP_ACTIONS/env_templates` | not applicable — extension not registered (9 tests) |

**CORRECTIONS to the `EXPR` rows, kept because both were wrong for a while.**
The `job_templates` row went uncorrected through the whole of sub-project E2,
still carrying a pre-E2 figure (143/209 pass, 66 baselined) while the real score
had moved to 175/209 with E2 and then 186/209 across E3; F1 took it to 206/209.
Sub-project H2 changed how it is scored, not what it scores: `EXPR` is a
supported extension, so the row is now **live** on the same template path as
`base` — measured identical, 206/209 — instead of "not applicable, scored
separately". The `env_templates` row said "extension not registered", which was
never the operative reason and is now plainly wrong: those six fixtures are
`environment-2023-09` documents, a top-level format `sqi` does not implement at
all, so they would be not-applicable no matter which extension they declared.

768 fixtures collected in total: 666 live passes, 3 baselined failures, 99
not applicable.

**Scope.** Only template-validation tests run today. The suite's job-execution
tests require a live session runtime and are not yet wired in.

**`env_templates`.** `sqi` does not implement standalone `environment-2023-09`
templates at all — every fixture under any `env_templates/` directory,
`base` included, is rejected on `/specificationVersion: unsupported version
"environment-2023-09"; expected "jobtemplate-2023-09"`, never on the fixture's
own encoded defect. Scoring those results would be meaningless: an
`.invalid` fixture would be "rejected" for the wrong reason and read as a
pass, exactly the false-green failure mode described below for unregistered
extensions, just keyed on document kind instead of extension name. So
`env_templates` fixtures are classified `StateNotApplicable` unconditionally
(`test/conformance/classify.go`, `Classify`) and are not scored at all — this
is a deliberate deferral, not an unknown defect, but it is reported as
not-applicable rather than as pass/fail for the same reason the extension
rows below are.

**Not-applicable rows.** Two distinct things land here, both for the same
underlying reason — scoring them would be meaningless because rejection
doesn't mean what the fixture is testing:

- `sqi` rejects templates declaring an extension it has not implemented,
  which is intentional — accepting a template whose syntax it cannot
  interpret is strictly worse.
- `sqi` rejects every `env_templates` fixture regardless of extension,
  because it does not implement the standalone environment document type at
  all — see above.

Those fixtures are therefore reported separately and never counted as passes,
so an unimplemented extension or document kind can never be mistaken for a
conforming one.

**Known failures** are tracked in `test/conformance/baseline.txt`. CI fails both
when an unlisted test breaks and when a listed test starts passing, so the list
can only shrink deliberately.

## Verifying documentation examples against the real parser

Every OpenJD YAML/JSON example in `docs/openjd-submission.md` is verified by
actually parsing and validating it — via `openjd.Parse` +
`openjd.Validate` — not by inspection. `internal/openjd`'s existing
`TestParse*` suite exercises the parser broadly; when correcting a specific
documented example, the fastest way to confirm it is a throwaway
`_test.go` file in `internal/openjd` that parses the exact YAML/JSON block
verbatim and asserts zero validation errors, run once, then deleted before
committing — it is not meant to become a permanent regression test. Two real
bugs surfaced this way during this conformance cycle that a read-through would
have missed: the step-level dependency key is `dependencies:` (a list of
`{dependsOn: <Name>}`), not a bare `dependsOn:` list of `{stepName: <Name>}`
as earlier documentation showed; and an explicit-but-empty
`hostRequirements: {}` is now rejected (only *omitting* the key reserves the
whole machine) now that host-requirement structural checks run
unconditionally.
