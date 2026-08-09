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

Two things, both **out of scope by design**, not bugs to fix:

- **`EXPR`** (the OpenJD Expression Language extension) — new value types
  (`BOOL`, `RANGE_EXPR`, `LIST[...]`), `let` on step templates, and the
  `*_LIST` `userInterface.control` variants all belong to `EXPR` and are not
  implemented. A template that declares `extensions: [EXPR]` is rejected with a
  `422` at `/extensions/0` — unconditionally, the same as any other unregistered
  extension name.
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

### EXPR: a temporary, second scoring path

**CORRECTION (sub-project E2, 2026-08-08):** this section's title and much of
its body describe an arrangement E2 replaced. When written, `EXPR/job_templates`
was scored through a genuinely separate path — `test/conformance/exprcase.go`'s
`RunExprCase`, which parsed and evaluated a fixture's embedded `{{ ... }}`
expressions directly and never touched `openjd.Parse` or
`openjd.ValidateWithOptions` at all. That is no longer how scoring works.
Sub-project E2's Task 2 moved `TestConformance_Expressions` onto `runEXPRCase`
(`test/conformance/suite_test.go`), a thin wrapper around the **same real
parse-and-validate path** `TestConformance_Templates` uses for every other
suite directory (`conformance.RunCase`'s machinery), which additionally
discounts only the EXPR extension's own registered-but-unsupported status-gate
error before deciding pass/fail — see `runEXPRCase`'s own doc comment for why
that discount exists and is temporary. `RunExprCase` and `exprcase.go` still
exist in the tree — several `TestConformance_*ProtectedFixtures` tests
(`B3`, `C1`, `C2`, `C3`, `C4`) still call `RunExprCase` directly, predating
this change — but `TestConformance_Expressions` itself, the test that produces
the headline EXPR score, no longer does. `RunExprCase` survives as unused,
dead weight for the score itself, until sub-project H deletes it alongside
`exprcase.go`, `exprcase_test.go` and `baseline-expr.txt`, per
`TestConformance_EXPRNotSupported`'s own checklist.

The paragraphs below this note describe the pre-E2 arrangement and are kept
for history, not because they are still accurate — read them as "how EXPR
scoring used to work," not as current behavior. Two concrete facts they get
wrong today: `EXPR` **is** in the registry (`Status: StatusInProgress`, added
by the commit immediately before E2 began), so the "not in the registry"
claim in the next paragraph is stale; and the score they cite (143/209 pass,
66 baselined) is likewise a pre-E2 snapshot. As of E2 (commit range
`111b0b2..b989aa5`), `EXPR/job_templates` scores **175/209 pass, 34
baselined** — see `test/conformance/baseline-expr.txt`'s own 2026-08-08 notes
for the full accounting of what moved and why, and
`TestConformance_E2ProtectedFixtures` for the 53 fixtures E2's routing and
scope-model changes put at risk of a silent regression-plus-compensating-pass
swap.

`EXPR` was **not** in the registry when this paragraph was written, so the
production behavior above was unchanged: a template declaring
`extensions: [EXPR]` was rejected with a `422` at `/extensions/0`,
unconditionally, the same as any other unregistered extension name. (As the
correction above states, this is no longer true: `EXPR` is registered, status
`StatusInProgress` — still rejected in production because it is not yet
`StatusSupported`, but for a different, more specific reason than "unknown
extension name.")

What has changed is that `internal/openjd/expr` now exists as a self-contained
reader and evaluator implementing the spec's real type system — nested types,
coercion, and static type checking via placeholders for values that don't
exist yet — for the EXPR expression language. `internal/openjd` does not call
it yet, so it has no effect on template validation. Building it ahead of
registration made it possible to start measuring conformance against the
suite's 209 `EXPR/job_templates` fixtures, which `TestConformance_Templates`
cannot do: every one of them declares `extensions: [EXPR]`, so the template path
rejects all 209 for the extension-gating reason alone, and 180 of them are
marked `.invalid` — scoring that rejection as a pass would report 180 false
greens before a single line of EXPR semantics existed.

So the suite scores `EXPR/job_templates` through a **second, temporary path**
(`test/conformance/exprcase.go`, `TestConformance_Expressions`) that parses AND
evaluates the `{{ ... }}` expressions a fixture embeds, instead of validating
the template as a whole. Evaluation runs against a symbol table built from the
fixture's own declared parameter types: every symbol section 1.2.2 defines is
bound as an unresolved placeholder of its declared type, so a type error, an
int64 overflow, a division by zero, or an unknown symbol is caught without any
parameter value ever existing — the same static type checking `internal/openjd/expr`
performs everywhere else. A name introduced by a `let:` block binds untyped,
since this path does not track `let` scoping or evaluate a binding's
right-hand side to learn its real type. As of this measurement it scores
**143 / 209 pass, 66 baselined** in `test/conformance/baseline-expr.txt`. A
fixture that is invalid for a reason this path cannot see — a runtime-only
condition, or a `let` binding whose real type would have caught it — still
parses and evaluates fine, is accepted, and therefore fails and is baselined
— that is deliberate reporting, the same principle as the not-applicable rows
below, not a defect to chase down here. One fixture that is NOT on the
baseline list — it currently passes — is worth calling out, because it passes
for a reason narrower than the rule it exists to test; two more are worth
calling out for having passed that way until sub-project D took the reason
away.

`expr1.3.9--memory-limit-exceeded` (`{{ 'a' * 100000000 }}`) passes, but not
because section 1.3.9's memory limit is enforced — it isn't; the spec's
*configurable* limit is still sub-project E's. It passes because
`internal/openjd/expr` carries a hard, non-configurable safety bound
(`limits.go`'s `maxStringBytes`, 10,000,000 bytes, applied by `checkRepeat`),
and that bound rejects the expression. **String repetition is implemented** and
has been since sub-project B2 — `ops.go`'s `OpMul` table registers
`{TString, TInt} -> repeatString`, and `'a' * 3` evaluates to `"aaa"`. An
earlier revision of this paragraph said the fixture passed because
`__mul__(string, int)` was unimplemented and predicted the entry would start
failing once repetition shipped; repetition shipped, the entry kept passing,
and the stated mechanism was wrong. The conclusion is unchanged: this is not a
section 1.3.9 pass, because a hard per-operation ceiling is not a memory
budget — an expression that stays just under it repeatedly still allocates
without bound.

`7.3--apply-path-mapping-in-job-name.invalid.yaml` and
`7.3--apply-path-mapping-in-timeout.invalid.yaml` **used to** pass, because
`apply_path_mapping` was an **unknown function** — not because either
fixture's actual rule, that the function is available in host context only, was
checked. Sub-project D registered the function (it was the last name missing
from the RFC 0006 library), so both expressions now evaluate cleanly and both
`.invalid` fixtures flip to failing: the score moved **145 → 143** and the
baseline **64 → 66**. Both are now listed in
`test/conformance/baseline-expr.txt` with that reason. They are burn-down
entries, not permanent ones: sub-project E is the layer with a scope model, and
when its scope-aware evaluation can reject the two expressions for the rule they
actually violate, both come off the list and the score goes back up. See that
file's header, which records the whole sequence.

**CORRECTION (sub-project E2):** this prediction has now happened. E2's scope
model rejects both `apply_path_mapping(...)` calls for their real reason —
`apply_path_mapping` is host-context-only, and neither a job name field nor a
plain `@fmtstring` timeout is a host context — so both fixtures are off
`baseline-expr.txt` and count toward the 175/209 pass score cited above. Both
are asserted by name in `TestConformance_E2ProtectedFixtures`
(`test/conformance/suite_test.go`), so a future regression that silently swaps
them back to passing for the wrong reason (or fails them outright) is caught.

The path is deleted the moment EXPR is supported for real: as of sub-project E2,
EXPR is registered (status `"in-progress"`) so this suite can score EXPR fixtures
through the real parse and validate path, but production still rejects an EXPR
template because it is not yet `StatusSupported`. `TestConformance_EXPRNotSupported`
fails the build the moment `internal/openjd` marks EXPR `StatusSupported`, forcing
`test/conformance/exprcase.go`, `exprcase_test.go`, `baseline-expr.txt`,
`TestConformance_Expressions` and that guard itself to be deleted in favor of
letting `TestConformance_Templates` score `EXPR/job_templates` end to end.

This is also why the portability claim in `README.md` and `ROADMAP.md` carries an
explicit caveat rather than being dropped. The measured position is narrower and
more useful than a slogan: `sqi` currently accepts **every valid base-spec
template in the conformance suite** — no conforming job template is turned away.
Both remaining classes of gap are elsewhere:

- **Unimplemented extensions**, rejected by name at submission time, as argued
  above. A template hits this only by opting in explicitly.
- **Over-permissiveness** — templates the spec says are *invalid* that `sqi`
  nonetheless accepts. As of the measurement below there are **none**: every
  template-validation fixture in the suite, valid and invalid alike, now scores
  correctly. `test/conformance/baseline.txt` is empty, and CI fails if any entry
  is added without being fixed.

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
| `EXPR/job_templates` | not applicable to the template path (209 tests) — scored separately, see [below](#expr-a-temporary-second-scoring-path): **143 / 209 pass, 66 baselined** |
| `EXPR/env_templates` | not applicable — extension not registered (6 tests) |
| `FEATURE_BUNDLE_1/job_templates` | not applicable — extension not registered (41 tests) |
| `FEATURE_BUNDLE_1/env_templates` | not applicable — extension not registered (4 tests) |
| `WRAP_ACTIONS/env_templates` | not applicable — extension not registered (9 tests) |

768 fixtures collected in total: 460 live passes, 0 baselined failures, 308
not applicable — **every live template-validation fixture passes.**

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
