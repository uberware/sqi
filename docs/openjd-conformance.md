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

Three things. The first and last are **out of scope by design**, not bugs to
fix. The middle one is a **known defect** that is merely not fixed *here* —
it is listed alongside them so a reader auditing this section against the spec
does not mistake it for intended behavior:

- **`EXPR`** (the OpenJD Expression Language extension) — a template that
  declares `extensions: [EXPR]` is rejected with a `422` at `/extensions/0`,
  unconditionally, regardless of anything else the template does.

  **CORRECTION (sub-project F1, extended parameter types):** this bullet used
  to say the new parameter types (`BOOL`, `RANGE_EXPR`, `LIST[...]`) and the
  `*_LIST` `userInterface.control` variants "are not implemented". That is no
  longer true. `internal/openjd` parses and validates all eight types with
  their nested `item:` constraints, case-insensitive type names, and every
  control RFC 0007 defines; `internal/openjd/expr` resolves their values in
  both evaluation phases. What remains true is the sentence above it: none of
  it is reachable in production, because `EXPR`'s status gate rejects the
  template before any of it runs. The types are real, tested code, reachable
  today only through the conformance suite's direct calls to `openjd.Parse` /
  `openjd.ValidateWithOptions` — the same standing as `let`, described
  immediately below.

  **CORRECTION (sub-project E3, `80d69b1..fa3267d`, plus this docs correction):** this bullet used to
  also list `let` on step templates as unimplemented. That is no longer true.
  `internal/openjd` models, parses, and validates `let:` at the three
  locations it can reach today — `<StepTemplate>.let`, `<StepScript>.let`,
  `<EnvironmentScript>.let` — enforcing section 3.6's own rules: duplicate-name
  and shadow-enclosing rejection, the `<UserIdentifier> = <expr>` grammar,
  self-reference rejection, the lowercase-first-letter name rule, and the 1-50
  element-count bound. The fourth location, `<SimpleAction>.let`, is still
  out of reach: `SimpleAction` itself is an unmodeled `FEATURE_BUNDLE_1`
  element (see the not-applicable rows below). This changed **base-spec**
  behavior too, not only EXPR's: a template carrying a `let:` block without
  declaring `EXPR` used to be silently *accepted*, `let:` ignored outright; it
  is now rejected with `"let" requires the EXPR extension to be declared` at
  the block's own pointer — a real conformance gap closed, not a new
  restriction invented. None of this makes `let` usable in production,
  though: a template that DOES declare `EXPR` is still rejected outright at
  `/extensions/0` before any of `let`'s validation runs, because `EXPR`'s own
  status gate (`StatusInProgress`) rejects every `EXPR`-declaring template
  unconditionally regardless of what else about it is correct. `let` is real,
  tested code, reachable today only through the conformance suite's direct
  calls to `openjd.Parse`/`openjd.ValidateWithOptions` — see
  [below](#expr-a-temporary-second-scoring-path).
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
exist in the tree, and **are still called**: five `TestConformance_*ProtectedFixtures`
tests (`B3`, `C1`, `C2`, `C3`, `C4`) call `RunExprCase` directly, predating this
change. **Deleting it today would break those five tests.** What changed is
narrower: `TestConformance_Expressions` itself — the test that produces the
headline EXPR score — no longer goes through it, so `RunExprCase` no longer
contributes to the score. It goes when sub-project H deletes it alongside
`exprcase.go`, `exprcase_test.go` and `baseline-expr.txt`, per
`TestConformance_EXPRNotSupported`'s own checklist, which is also when those
five tests are rewritten onto the real path.

The paragraphs below this note describe the pre-E2 arrangement and are kept
for history, not because they are still accurate — read them as "how EXPR
scoring used to work," not as current behavior. Two concrete facts they get
wrong today: `EXPR` **is** in the registry (`Status: StatusInProgress`, added
by E2's own Task 1, `111b0b2`), so the "not in the registry"
claim in the next paragraph is stale; and the score they cite (143/209 pass,
66 baselined) is likewise a pre-E2 snapshot. As of E2 (commit range
`0519aee..1966cb5`), `EXPR/job_templates` scores **175/209 pass, 34
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
because section 1.3.9's memory limit rejected it. That limit *is* implemented
and is now operator configuration
([`openjd.expr_memory_limit`](configuration.md#openjdexpr_memory_limit),
default 1,000,000 bytes) — but this fixture never reaches it: measured on this
branch, the expression is refused with the same error at the submission
defaults and with no limit options at all. It passes because
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

**CORRECTION (sub-project E2):** this prediction was fulfilled for **one** of
the two fixtures, not both. Both are off `baseline-expr.txt` and both count
toward the 175/209 pass score cited above, but for different reasons, and only
one is the scope model's doing:

- `7.3--apply-path-mapping-in-job-name.invalid.yaml` — **the prediction, met.**
  E2's scope model rejects the call for its real rule: `apply_path_mapping` is
  host-context-only and the job name field is not a host context.
- `7.3--apply-path-mapping-in-timeout.invalid.yaml` — **still incidental.** It
  is rejected by `decodeAction` (`internal/openjd/parse.go`), which decodes
  `timeout` with a strict integer parse, so `openjd.Parse` fails with
  `openjd: timeout must be an integer` before validation — and therefore before
  the scope model — runs at all. `checkActionExpressions` does carry a timeout
  position, but it is wired-and-unreachable for any real template; see the
  standing comment above that function in `internal/openjd/exprcheck.go`.
  Making the rule actually fire there requires teaching `decodeAction` to
  accept a format-string body for `timeout`, which changes every existing
  template's timeout, base-spec included. That remains open.

Both are asserted by name in `TestConformance_E2ProtectedFixtures`
(`test/conformance/suite_test.go`), with each entry's "why" stating which of
the two mechanisms applies, so a future regression that silently swaps them
back to passing for the wrong reason (or fails them outright) is caught.

**Sub-project E3 (`80d69b1..fa3267d`, plus this docs correction) — `let` bindings.** `let` is now real at
the three locations `internal/openjd/model.go` can reach (`<StepTemplate>.let`,
`<StepScript>.let`, `<EnvironmentScript>.let`; the fourth, `<SimpleAction>.let`,
needs `SimpleAction` itself, an unmodeled `FEATURE_BUNDLE_1` element). See the
correction above under [What `sqi` deliberately does not
implement](#what-sqi-deliberately-does-not-implement) for what that means for
production behavior — nothing, yet, because `EXPR`'s own status gate still
rejects every `EXPR`-declaring template first. `EXPR/job_templates` moved
**175/209 (34 baselined) → 186/209 (23 baselined)** across three tasks (+3,
+2, +6), each confirmed individually and recorded in `baseline-expr.txt`'s own
dated notes. (Sub-project F1 later took it to **206/209, 3 baselined** — see
[Extended parameter types](#extended-parameter-types-rfc-0007) below.) `baseline-expr.txt` carries *four* E3-dated notes, one more than
the number of tasks that moved the score: Task 11's note clears nothing, it
surveys the disposition of every remaining `let` fixture. Do not read the note
count as a score-moving-task count:

- Task 2: `validateLetExtension` rejects any `let:` block on a template that
  does not declare `EXPR` — three fixtures clear (the `*-requires-expr` set).
- Task 4: `validateLetElementCounts` enforces section 3.6's "at least one,
  at most 50" element-count bound — two fixtures clear.
- Task 8: `checkLetBindings` (built by Tasks 6-7) is wired into all three real
  `let:` locations for the first time, so its duplicate-name/shadow-enclosing
  rejection, the `<UserIdentifier> = <expr>` grammar check, and
  self-reference-as-unknown-symbol all run for real — six fixtures clear,
  including `expr1.3.7--loop-var-shadows-binding.invalid.yaml`, whose
  rejection (a comprehension loop variable shadowing a `let` name, section
  1.3.7) was already implemented in `internal/openjd/expr/comp.go` since
  sub-project B3 and needed no new evaluator code, only a symbol table that
  finally contains `let` names for it to shadow.

`TestConformance_E3ProtectedFixtures` (`test/conformance/suite_test.go`) pins
all 11 by name — and, going further than `TestConformance_E2ProtectedFixtures`
does, by the *exact* discounted validation-error text each one produces, not
merely `res.Passed`. Asserting only pass/fail is exactly the gap that let E2
credit a fixture to the scope model when the YAML decoder was doing the work
instead; see that test's own doc comment for why `res.Reason` on
`conformance.Result` cannot answer this by itself (it is blanked to `""` the
moment a fixture passes) and how the test works around it by re-running the
validation pipeline directly.

Three `let` fixtures remain baselined, blocked on `FEATURE_BUNDLE_1` — a
separate, unregistered RFC 0004 extension (see [Supported extensions and the
vendor-prefix rule](#supported-extensions-and-the-vendor-prefix-rule)) —
rather than on anything E3 left undone: `3.6--let-host-context-symbols.yaml`,
`7.3.1--job-step-name-in-step-let.yaml`, and `3.6--let-bindings.yaml`, which
is additionally — and more proximately, since it fires first — blocked by an
unrelated `LIST[INT]` job parameter (sub-project F). Five more,
`3.6--let-comprehension-shadows*.invalid.yaml`, already pass, but not for the
rule they exist to test: each declares a `LIST[INT]` job parameter, which
`internal/openjd`'s parameter parsing rejects at PARSE time, before `let:` or
the comprehension-shadow rule is ever reached. E3 *does* implement that rule;
the conformance score structurally cannot see it land (E3 could implement
none of it and the score would not move), so
`internal/openjd/exprcheck_let_test.go` pins it with hand-authored tests
instead, transcribing each of the five fixtures' intent with an `INT`,
`STRING`, or `PATH` parameter standing in for the unsupported `LIST[INT]` —
the same technique sub-project D used for `apply_path_mapping`'s 31
expectations when no oracle could see them either. See `baseline-expr.txt`'s
own Task 11 note for the full, per-fixture accounting.

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

### Extended parameter types (RFC 0007)

Sub-project F1 implemented [RFC 0007][rfc0007] — the EXPR extension's extended
parameter types — on the template side. `EXPR/job_templates` moved **186/209
(23 baselined) → 206/209 (3 baselined)**: twenty fixtures, measured, not
predicted.

[rfc0007]: ../third_party/openjd-specifications/rfcs/0007-extend-parameter-types.md

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
| `EXPR/job_templates` | not applicable to the template path (209 tests) — scored separately, see [below](#expr-a-temporary-second-scoring-path): **206 / 209 pass, 3 baselined** |
| `EXPR/env_templates` | not applicable — extension not registered (6 tests) |
| `FEATURE_BUNDLE_1/job_templates` | not applicable — extension not registered (41 tests) |
| `FEATURE_BUNDLE_1/env_templates` | not applicable — extension not registered (4 tests) |
| `WRAP_ACTIONS/env_templates` | not applicable — extension not registered (9 tests) |

**CORRECTION:** the `EXPR/job_templates` row above went uncorrected through
the whole of sub-project E2 — it still carried the pre-E2 figure (143/209
pass, 66 baselined) that predates even the routing change onto `runEXPRCase`,
while the actual score had already moved to 175/209 (34 baselined) with E2
and then, across the three E3 tasks that moved it, to today's 186/209 (23
baselined). It is now
current as of sub-project E3 (`80d69b1..fa3267d`, plus this docs correction); see [EXPR: a temporary,
second scoring path](#expr-a-temporary-second-scoring-path) for the full
accounting of both moves.

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
