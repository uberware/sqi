# OpenJD Spec Conformance

This page states, in one place, how far `sqi`'s OpenJD support goes: which spec
version it implements, where that is enforced in code, which extensions it
understands, and what it does not implement and why. Read this before
re-auditing OpenJD conformance from scratch.

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
   would vanish for any caller that flips the flag.
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
| `EXPR` | official | The expression language: a Python subset in format strings, `let` bindings, the RFC 0006 function library, and the RFC 0007 parameter types. See [EXPR](#expr) below and [`docs/openjd-extensions/expr.md`](openjd-extensions/expr.md). |
| `SQI_PATH_TRANSLATION` | vendor | Per-product path-delivery checklist (`swap_in_place`/`translation_file`/`command_flags`/`environment`/`stage_locally`). See [`docs/openjd-extensions/path-translation.md`](openjd-extensions/path-translation.md). |
| `SQI_CHUNK_BOUNDS` | vendor | Exposes a `CHUNK[INT]` chunk's first/last integer as `Task.Param.<name>.Start`/`.End`. See [`docs/openjd-extensions/sqi-chunk-bounds.md`](openjd-extensions/sqi-chunk-bounds.md). |

Every **vendor** extension (`Origin: OriginVendor` — one `sqi` defines itself,
as opposed to one specified upstream by OpenJD) name must carry the `SQI_`
prefix. A test invariant (`internal/openjd/extension_test.go`) enforces this,
so a vendor extension name can never collide with a future official OpenJD name,
which are always bare identifiers (e.g. `TASK_CHUNKING`, not `SQI_TASK_CHUNKING`).
If a vendor extension is later upstreamed into the OpenJD spec, the promotion
path is to drop the `SQI_` prefix and flip its `Origin` to `OriginOfficial` — the
registry entry moves, it doesn't get a second copy.

Declaring an extension name not present in the registry is a `422` at
`/extensions/{i}`, regardless of `EnforceLimits`. Declaring `CHUNK[INT]` without
`TASK_CHUNKING` in `extensions` is likewise rejected — the extension gate and the
feature it gates are checked together.

## What `sqi` deliberately does not implement

Standalone `environment-2023-09` templates are out of scope by design. The
other entry is a known defect, listed here so that a reader auditing this
section against the spec does not mistake it for intended behavior:

- **`Job.Name` / `Step.Name` in `hostRequirements` and `parameterSpace`** — a
  known gap against section 7.3.1. `checkStepExpressions` evaluates both of
  those positions at `ScopeJob`, whose fixed-symbol set (`scopeFixed`,
  `internal/openjd/scope.go`) is empty, so a bare `Job.Name` or `Step.Name`
  there is rejected as an unknown symbol:

  ```
  /steps/0/hostRequirements/attributes/0/anyOf/0: col 1: unknown symbol "Step.Name"
  /steps/0/hostRequirements/attributes/0/anyOf/0: col 1: unknown symbol "Job.Name"
  /steps/0/parameterSpace/taskParameterDefinitions/0/range/0: col 1: unknown symbol "Step.Name"
  ```

  Section 7.3.1's symbol table grants both: `Step.Name` is "Available within
  the Step Template scope: `stepEnvironments`, `hostRequirements`,
  `parameterSpace`, and `script`", and `Job.Name` is "Available in every Format
  String in the Job Template, except the `name` field of the Job Template
  itself". Section 3.6.2 does not authorize the rejection: it governs where
  *`let` names* are visible and is silent on the fixed symbols. No fixture
  covers this, so it does not show in the scores below. Closing it means
  splitting `scopeFixed(ScopeJob)` or adding a distinct scope for these two
  positions, a change that can move conformance scoring. Because `EXPR` is
  supported, a submitted template that hits this gap gets a `422`.
- **Standalone `environment-2023-09` templates** — see [Spec version](#spec-version)
  above.

**Why rejecting an unimplemented opt-in extension is correct, not a gap:**
OpenJD extensions exist so a template can declare "I need this capability" and
a conformant implementation that lacks it can say "then I can't run you"
instead of guessing. Accepting a template whose syntax `sqi` cannot interpret,
ignoring the parts it doesn't understand, is worse than refusing it: the result
is a job that appears to submit successfully and then does something other than
what the template author asked for, discovered only when the render is wrong.
The spec does not mandate rejection of an unimplemented extension either way;
`sqi` rejects because a `422` at submission time is a better failure mode than
a silent misinterpretation at run time.

### EXPR

`EXPR` is a supported extension: an `EXPR` template is parsed, validated,
scope-checked, expression-checked at submission with concrete parameters, and
resolved on the worker at run time. `EXPR/job_templates` is scored by
`TestConformance_Templates` through the same `openjd.Parse` +
`openjd.ValidateWithOptions` path as every other live directory. It scores
**206 / 209 pass, 3 baselined**.

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

`3.6--let-bindings.yaml` declares a `LIST[INT]` job parameter, which parses
(the RFC 0007 parameter types are implemented), so only the `FEATURE_BUNDLE_1`
gap blocks it. Of the five `3.6--let-comprehension-shadows*.invalid.yaml`
fixtures, which pass and are not baselined, four are rejected by section
1.3.7's loop-variable-shadowing rule at the position that carries it; the fifth
uses a `bash:` `SimpleAction` and is a `FEATURE_BUNDLE_1` rejection. A
protected-fixture test (below) records which is which.

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
String repetition itself is implemented (`ops.go`'s `OpMul` table registers
`{TString, TInt} -> repeatString`, and `'a' * 3` evaluates to `"aaa"`). This is
not a section 1.3.9 pass, because a hard per-operation ceiling is not a memory
budget.

`7.3--apply-path-mapping-in-timeout.invalid.yaml` is rejected by `decodeAction`
(`internal/openjd/parse.go`), which decodes `timeout` with a strict integer
parse, so `openjd.Parse` fails with `openjd: timeout must be an integer` before
validation — and therefore before the scope model — runs at all.
`checkActionExpressions` does carry a timeout position, but it is
wired-and-unreachable for any real template; see the standing comment above that
function in `internal/openjd/exprcheck.go`. Its sibling
`7.3--apply-path-mapping-in-job-name.invalid.yaml` *is* the scope model's doing:
`apply_path_mapping` is host-context-only and the job name field is not a host
context.

**Protected-fixture tests.** The score is an aggregate, and an aggregate cannot
see one fixture regressing while another starts passing for an unrelated reason.
Seven `TestConformance_*Fixtures` tests in
`test/conformance/suite_test.go` pin fixtures by name against that swap. Each
entry carries a "why" string naming the rule it depends on, and each test's
doc comment states how much its entries pin: several are rejection *floors*
rather than proofs that a named mechanism still fires, because
`conformance.Result` blanks `Reason` the moment a fixture passes. The one
covering `let` bindings also asserts each fixture's exact validation-error text
by re-running the pipeline on the side.

[rfc0004]: https://github.com/OpenJobDescription/openjd-specifications/blob/mainline/rfcs/0004-enhanced-limits-and-capabilities.md

This is also why the portability claim in `README.md` and `ROADMAP.md` carries an
explicit caveat. The measured gaps fall into three classes:

- **Unimplemented extensions**, rejected by name at submission time, as argued
  above. A template hits this only by opting in explicitly. This is what the
  three baselined `EXPR/job_templates` fixtures are: valid templates turned away
  because they *also* declare `FEATURE_BUNDLE_1`.
- **The 1024-value cap on an `<IntRangeExpr>`.** Section 3.4's "at most 1024
  values" bounds only the list form of a task parameter's `range`
  (openjd-specifications#172); `sqi` applies it to the range-expression form as
  well, so the valid base-spec fixture `3.4--wide-int-range-expression.yaml`
  (`range: "1-5000"`) is rejected. Lifting the cap is an acceptance change on
  the same policy surface as the per-step and per-job task limits.
- **Over-permissiveness** — templates the spec says are *invalid* that `sqi`
  nonetheless accepts. As of the measurement below there are none: every
  `.invalid` template-validation fixture in the suite is rejected.
  `test/conformance/baseline.txt` holds only the four under-permissive cases
  above, and CI fails if any entry is added without being fixed, removed once
  it starts passing, or left matching nothing.

### Extended parameter types (RFC 0007)

`sqi` implements [RFC 0007][rfc0007], the EXPR extension's extended parameter
types:

[rfc0007]: https://github.com/OpenJobDescription/openjd-specifications/blob/mainline/rfcs/0007-extend-parameter-types.md

- **Case-insensitive type names** for every type, job and task, including
  compound ones (`list[list[int]]`). Gated on `extensions: [EXPR]`: without it
  `type: int` is rejected, because RFC 0007 is an extension specification and accepting lowercase unconditionally would widen
  what the base spec admits.
- **`BOOL`**, with the RFC's accepted-values table (`true`/`1`/`1.0`/`yes`/
  `on` and their negatives) and its explicit prohibition on `allowedValues`.
- **`RANGE_EXPR`**, validated against the `<IntRangeExpr>` grammar under the
  **specification's permissive policy** rather than `internal/openjd`'s
  stricter one, as documented on `validateRangeExprParamConstraints`: the
  fixture declares `"10-1:-1"` and `"-1--10:-1"`, which the strict policy
  rejects, and the permissive policy is what the value meets downstream anyway.
  Literal base-spec range text keeps the strict policy.
- **Six `LIST[*]` types** with nested `item:` constraints, one level deep.
  A list default is stored as canonical JSON in the existing `Default` field;
  element checking is by JSON **type**, which is what the RFC's `<string>` /
  `<integer>` schemas actually constrain.
- **The `*_LIST` controls**, plus the list form of the two rules written
  against the scalar spin box (`singleStepDelta`, `decimals`).
- **The `<ArgString>` control-character amendment** — CR, LF and TAB become
  legal in an argument when `EXPR` is declared, "to support multi-line
  expressions in YAML literal block scalars". `<CommandString>` is a separate
  type and is **not** amended, so a command keeps the base-spec rule.

None of the three baselined `EXPR` fixtures is an RFC 0007 failure; each is
blocked by the unregistered `FEATURE_BUNDLE_1` extension and an unmodeled
`bash:` action type (see [EXPR](#expr)).

The runtime half (submitted-value validation, element-wise `loc://`
resolution, and per-element staging and path mapping for `LIST[PATH]`) is
outside what a conformance fixture can see: the fixtures only parse and
validate, never expand or submit.

### A second, independent check on EXPR: the reference-implementation oracle

The fixture suite scores whether `sqi` reaches the right verdict on the
templates upstream happens to ship. It cannot tell you whether the *evaluator*
agrees with upstream on an expression no fixture contains, and it cannot catch a
misreading of the spec applied consistently across `sqi`'s own tests.

`make test-expr-oracle` covers that gap by evaluating a corpus with both `sqi`
and the implementation the EXPR spec names as its reference, and comparing.
It supplements this suite and does not replace it. The reference is Beta, so
the spec outranks it. Setup, the baseline format, and the
currently accepted divergences are documented in
[`docs/development.md`](development.md#differential-testing-expr-against-the-reference-implementation).

## Known divergence: sqi names under the reserved `worker` scope

The spec reserves `worker`, `job`, `step`, and `task` as the first identifier
after a capability namespace, "for use in this and future revisions" (§3.3.1.1).
`sqi` validates that: a name under a reserved scope must be one the spec defines,
so `amount.worker.custom` is rejected.

Four `sqi` names are exempted. This is a divergence from the spec:

| Name | Backs |
|---|---|
| `attr.worker.tag.*` | [worker capability tags](worker-capabilities.md) — every shipped DCC preset gates on one |
| `amount.worker.usagepool.*` | [usage pools](openjd-submission.md#4-usage-pools) |
| `attr.worker.computelocation` | [compute locations](compute-locations.md) |
| `attr.worker.os.version` | OS version matching |

These predate the check and are in wide use; enforcing the spec strictly would
invalidate `sqi`'s own presets. The conformant fix is to move them behind a
vendor prefix — `sqi:attr.worker.tag.nuke`, which the validator already accepts
(see [the vendor-prefix rule](#supported-extensions-and-the-vendor-prefix-rule))
— but that renames a capability every existing template and worker config uses,
so it is a breaking change that has not been made. The exemption list lives in
`sqiReservedScopeNames` in `internal/openjd/validate.go`.

## Adding a deliberate divergence

If `sqi` needs to diverge from strict spec conformance on purpose (for
example, to offer a non-standard parameter control such as `CHIP_INPUT`), the
route is the extension mechanism above, not a parser change:

1. A registry entry in `internal/openjd/extension.go` naming the divergence,
   e.g. `SQI_UI_CONTROLS`, with `Origin: OriginVendor` (so the `SQI_` prefix is
   mandatory and enforced by the test invariant).
2. Parse/validate support in `internal/openjd` gated on that extension being
   declared — the divergent behavior only activates for a template that opts
   in by name.
3. A doc under `docs/openjd-extensions/`, following the shape of the existing
   entries (motivation, schema, validation, worker behavior).
4. Any worker-side behavior the divergence needs, under `internal/worker/`.

`SQI_UI_CONTROLS` is not built; nothing declares it or depends on it. It is
the shape a future divergence would take: opt-in, named, registered and
documented, never a bare change to what the base spec's syntax means.

## Measured conformance

`sqi` runs the official [OpenJD conformance test
suite](https://github.com/OpenJobDescription/openjd-specifications/tree/mainline/conformance-tests)
on every CI build, from a pinned submodule (commit `1e4d49e937a8`). These are
measured results:

| Suite | Result |
|---|---|
| `base/job_templates` | **449 / 450 pass, 1 baselined** — the `<IntRangeExpr>` value cap, see above |
| `base/env_templates` | not applicable — standalone environment templates unsupported (39 tests) |
| `TASK_CHUNKING/job_templates` | **11 / 11 pass** |
| `REDACTED_ENV_VARS` | no template fixtures — the suite ships only `jobs/` (8 job-execution tests), which are out of scope; see **Scope** below |
| `EXPR/job_templates` | **206 / 209 pass, 3 baselined** — see [EXPR](#expr) |
| `EXPR/env_templates` | not applicable — standalone environment templates unsupported (6 tests) |
| `FEATURE_BUNDLE_1/job_templates` | not applicable — extension not registered (41 tests) |
| `FEATURE_BUNDLE_1/env_templates` | not applicable — extension not registered (4 tests) |
| `WRAP_ACTIONS/env_templates` | not applicable — extension not registered (9 tests) |

**`TASK_CHUNKING` behavior the scored suite does not cover.** No
`job_templates` fixture exercises a format-string `defaultTaskCount` or
`targetRuntimeSeconds`, `targetRuntimeSeconds: 0` (the documented minimum), or
a `CONTIGUOUS` range with gaps (`1,10-12` at 10 must be the two chunks `1-1`
and `10-12`, never one chunk whose bounds enclose 2-9). sqi's own tests in
`internal/openjd` cover all three.

The six `EXPR/env_templates` fixtures are `environment-2023-09` documents, a
top-level format `sqi` does not implement, so they are not applicable whichever
extension they declare.

769 fixtures collected in total: 666 live passes, 4 baselined failures, 99
not applicable.

**Scope.** Only template-validation tests run today. The suite's job-execution
tests require a live session runtime and are not yet wired in.

**`env_templates`.** `sqi` does not implement standalone `environment-2023-09`
templates at all — every fixture under any `env_templates/` directory,
`base` included, is rejected on `/specificationVersion: unsupported version
"environment-2023-09"; expected "jobtemplate-2023-09"`, never on the fixture's
own encoded defect. Scoring those results would be meaningless: an
`.invalid` fixture would be "rejected" for the wrong reason and read as a
pass, the same false-green failure mode described below for unregistered
extensions, keyed on document kind instead of extension name. So
`env_templates` fixtures are classified `StateNotApplicable` unconditionally
(`test/conformance/classify.go`, `Classify`) and are not scored.

**Not-applicable rows.** Two distinct things land here, both for the same
underlying reason — scoring them would be meaningless because rejection
doesn't mean what the fixture is testing:

- `sqi` rejects templates declaring an extension it has not implemented, on
  purpose: accepting a template whose syntax it cannot interpret is worse.
- `sqi` rejects every `env_templates` fixture regardless of extension,
  because it does not implement the standalone environment document type at
  all — see above.

Those fixtures are therefore reported separately and never counted as passes,
so an unimplemented extension or document kind can never be mistaken for a
conforming one.

**Known failures** are tracked in `test/conformance/baseline.txt`. CI fails both
when an unlisted test breaks and when a listed test starts passing, so the list
changes only by an explicit edit.

## Verifying documentation examples against the real parser

Every OpenJD YAML/JSON example in `docs/openjd-submission.md` is verified by
actually parsing and validating it — via `openjd.Parse` +
`openjd.Validate` — not by inspection. `internal/openjd`'s existing
`TestParse*` suite exercises the parser broadly; when correcting a specific
documented example, the fastest way to confirm it is a throwaway
`_test.go` file in `internal/openjd` that parses the exact YAML/JSON block
verbatim and asserts zero validation errors, run once, then deleted before
committing; it is not meant to become a permanent regression test. Two details
a read-through misses: the step-level dependency key is `dependencies:` (a list
of `{dependsOn: <Name>}`), not a bare `dependsOn:` list of `{stepName: <Name>}`;
and an explicit-but-empty `hostRequirements: {}` is rejected (only *omitting*
the key reserves the whole machine), because host-requirement structural
checks run unconditionally.
