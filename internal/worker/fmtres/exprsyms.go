// SPDX-License-Identifier: AGPL-3.0-or-later

package fmtres

// This file builds the EXPR phase-3 symbol table: the worker-side analog of
// internal/openjd's symbolsFor (exprcheck.go), which the worker cannot call
// directly (it takes a *openjd.JobTemplate, and the worker has no template —
// only the assignment). See EXPR sub-project E4a's design spec, section 3,
// whose table is the contract every family below implements, and section
// 3.1 for why parameter TYPES travel on the assignment (protocol.AssignMsg's
// JobParameterTypes/ParameterTypes fields) rather than being inferred from
// value text.
//
// Two builders, mirroring the split TaskScope/EnvScope already establish for
// the base-spec (non-EXPR) resolution path in fmtres.go:
//
//   - TaskSymbols: the task-action scope (a step's onRun command, args, and
//     embedded files).
//   - EnvSymbols: the environment-action scope (one environment's
//     onEnter/onExit actions, variable values, and embedded files).
//
// Both share the job-parameter and Session.* symbols; TaskSymbols alone adds
// Task.Param.*/Task.RawParam.*/Task.File.* (environments are session-scoped
// and entered once, not per task); EnvSymbols alone adds Env.File.* (scoped
// to the ONE environment the caller identifies, matching
// internal/openjd's symbolsFor -- a template may declare many environments,
// each with its own files).
//
// Every symbol this file binds is CONCRETE (expr.Unresolved never appears in
// the returned tables directly, though a value that failed to parse its
// declared type -- see expr.ValueFromText -- falls back to it), which is the
// one thing that distinguishes phase 3 from phases 1 and 2: "the same walk
// with a different table," now with every placeholder replaced by a value.
//
// THREE DIVERGENCES FROM PHASE 2 ARE CORRECT AND INTENDED, written down here
// and in docs/openjd-extensions/expr.md ("Deliberate phase-2/phase-3
// divergences") so a reviewer does not have to re-derive them and conclude
// they are bugs:
//
//  1. Session.PathMappingRulesFile is bound CONDITIONALLY (only when the
//     session has rules), matching the pre-EXPR addPathMappingKeys rule --
//     bindSessionSymbols below.
//  2. Paths are PathNative here and PathPOSIX at phase 2 -- pathFlavor below,
//     which quotes Expression-Language section 1.2.1 for it.
//  3. Path mapping is applied to Param.<PATH>/Task.Param.<PATH> only HERE.
//     Section 1.2.2 requires it; phase 2 has no session, hence no rules, to
//     apply -- paramValueForBinding below.
//
// FIX ROUND 1 (Task 4 review): Param.<name>/Task.Param.<name> for a PATH-
// declared parameter are now bound with the assignment's PathMap rules
// ALREADY APPLIED, via [mapPathParamValue] (expres.go) -- section 1.2.2:
// "Param.<name> ... path mapping rules already applied", RawParam carrying
// "the original unmapped value". Earlier revisions of this file bound BOTH
// from identical, unmapped raw text, which is silently wrong on every
// platform (path mapping never applied to a PATH parameter reached through
// Param./Task.Param. at all, apply_path_mapping() being the only way to
// invoke it) and additionally wrong in a Windows-specific way had mapping
// been bolted on at the TEMPLATE level instead of here: matching a rule
// against an already-host-flavor-rendered string (a PathNative Value's
// stringified form) rather than the untouched submitted text would make a
// POSIX-sourced rule silently fail to match on a Windows worker. Binding the
// mapped value from raw, submitted text -- before any flavor-specific
// rendering happens -- avoids that: SourceFormat matching happens on the
// form the rule itself declares, not on a re-rendered value.

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/uberware/sqi/internal/openjd/expr"
	"github.com/uberware/sqi/internal/openjd/fmtstring"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// pathFlavor is the expr.PathFormat every concrete Path value this file
// constructs uses -- job/task PATH parameters, Task.File.*/Env.File.*,
// Session.WorkingDirectory, Session.PathMappingRulesFile.
//
// PathNative, not the PathPOSIX internal/openjd's own phase-2
// concreteJobParamValue hardcodes. The specification settles this outright,
// Expression-Language section 1.2.1: "The path type can have either POSIX or
// Windows path semantics depending on the evaluation context... In TEMPLATE
// scope contexts, POSIX semantics are used for consistency. In host contexts
// (SESSION and TASK scopes), the semantics match the host's operating
// system." Phase 3 IS a host context -- it runs ON THE WORKER HOST, against
// a real session directory -- so the flavor is the host's, and phase 2, which
// runs at TEMPLATE scope, correctly keeps POSIX. (expr.PathFormat's own doc
// comment anticipates this -- "Nothing in sqi selects [PathNative] yet;
// sub-project E does, for host contexts" -- but it is a note about sqi's
// wiring, not the authority; section 1.2.1 is.)
//
// Using PathNative here means a worker running on
// Windows gets Windows path semantics for its own session paths, matching
// the filesystem it is actually writing to, rather than the POSIX default
// that exists specifically so a server-side template expands identically
// regardless of the submission machine's OS -- a concern that does not
// apply to a Value built from the worker's own, already-concrete, host
// paths.
const pathFlavor = expr.PathNative

// TaskSymbols builds the phase-3 EXPR symbol table for a task action (a
// step's onRun command, args, and embedded files). It exposes:
//
//   - Param.<name> / RawParam.<name>, typed from msg.JobParameterTypes and
//     concretized from msg.JobParameters.
//   - Task.Param.<name> / Task.RawParam.<name>, typed from
//     msg.ParameterTypes and concretized from msg.Parameters.
//   - Task.File.<name>, one path per entry in msg.EmbeddedFiles, at its
//     materialized on-disk location under workDir (via AddFileVars -- the
//     SAME computation the pre-EXPR path already uses, so the two paths
//     cannot diverge).
//   - Session.WorkingDirectory, Session.PathMappingRulesFile (only when
//     hasPathMap), Session.HasPathMappingRules.
//   - Job.Name, Step.Name, from msg.JobName/msg.StepName.
//
// It does NOT expose Env.File.* -- see EnvSymbols for that family, and
// TestTaskSymbols_ExcludesEnvFile for the negative this asymmetry is tested
// against.
//
// workDir is the absolute session working directory; pathMapFile is the
// absolute path to the written OpenJD path-mapping JSON file (meaningful
// only when hasPathMap is true). These are host facts the caller supplies,
// exactly as they already do for [TaskScope].
//
// An error is returned only when an embedded file's Name/Filename is
// invalid -- see [EmbeddedFileName].
func TaskSymbols(msg *protocol.AssignMsg, workDir, pathMapFile string, hasPathMap bool) (expr.MapSymbols, error) {
	syms := expr.MapSymbols{}
	if err := bindJobParamSymbols(msg, syms); err != nil {
		return nil, err
	}
	if err := bindTaskParamSymbols(msg, syms); err != nil {
		return nil, err
	}
	bindSessionSymbols(syms, workDir, pathMapFile, hasPathMap)
	syms["Job.Name"] = expr.String(msg.JobName)
	syms["Step.Name"] = expr.String(msg.StepName)
	if err := bindFileSymbols(syms, "Task.File", msg.EmbeddedFiles, workDir); err != nil {
		return nil, err
	}
	return syms, nil
}

// EnvSymbols builds the phase-3 EXPR symbol table for one environment's
// actions (onEnter/onExit) and variable values. It exposes:
//
//   - Param.<name> / RawParam.<name>, exactly as TaskSymbols.
//   - Env.File.<name>, one path per entry in env.EmbeddedFiles -- THIS
//     environment's own files, not any other environment's and not the
//     step's Task.File.* files (a template may declare many environments,
//     each with its own files; mirrors internal/openjd's symbolsFor, which
//     takes the specific *Environment for the same reason).
//   - Session.WorkingDirectory, Session.PathMappingRulesFile (only when
//     hasPathMap), Session.HasPathMappingRules.
//   - Job.Name, from msg.JobName.
//   - Step.Name, from msg.StepName, but ONLY when env.StepEnvironment is
//     true -- see below.
//   - The ENCLOSING STEP TEMPLATE's let-bound names (msg.StepTemplateLet),
//     again ONLY when env.StepEnvironment is true -- see below.
//
// It does NOT expose Task.Param.*/Task.RawParam.*/Task.File.*:
// environments are session-scoped and entered once, not per task --
// matching [EnvScope]'s existing, pre-EXPR behavior.
//
// Step.Name is CONDITIONAL, unlike every other symbol here, because
// section 3.6.2 (and internal/openjd's own ScopeJobEnvironment/
// ScopeStepEnvironment split, scope.go) grants it to a STEP environment's
// script but not a JOB environment's: ScopeStepEnvironment is exactly
// ScopeJobEnvironment plus Step.Name. protocol.AssignEnvironment.
// StepEnvironment is the bit that distinguishes them on the wire (assign.go
// sets it from which of tmpl.JobEnvironments/stepTmpl.StepEnvironments the
// entry came from), so this function trusts it directly rather than
// guessing.
//
// FIX ROUND 2 (E4a whole-branch review, Critical 1): msg.StepTemplateLet is
// evaluated and folded in here, for a step environment only. Template
// Schemas section 3.6.2 row 1 makes a <StepTemplate>.let binding's names
// available in stepEnvironments, hostRequirements, parameterSpace AND
// script -- not only in the script -- and section 3.6's prose states the
// same rule from the other side ("a let binding in a <StepTemplate>'s
// stepEnvironments cannot shadow a binding from that step's let block").
// Phase 2 already implements it: checkStepExpressions passes stepLet as
// checkEnvironmentExpressions' outerLet, which folds it into baseSyms
// BEFORE anything else and therefore behind variables, embedded files,
// onEnter and onExit alike. Phase 3 did not, so a template phase 2 accepted
// with zero errors failed every task in the step with unknown symbol
// "<name>" at enterOne -- naming a symbol the submitter had been told was
// valid -- and failed teardown the same way on the exit path.
//
// It is folded in HERE, inside the table builder, rather than exposed as a
// second "apply" step the caller could forget, precisely because forgetting
// it is the defect being fixed. That also mirrors phase 2 structurally:
// checkEnvironmentExpressions treats outerLet as part of the table an
// environment STARTS from (baseSyms := clone(outerLet); then symbolsFor's
// own additions), not as a let: block the environment itself evaluates. The
// environment's OWN let: block (section 3.6.2 row 4) remains a separate,
// later step -- [ApplyEnvLet] -- because section 3.6.2 places the
// environment's Variables BETWEEN the two, and phase 2's ordering there is
// observable.
//
// JOB environments get NOTHING from this: they have no enclosing step, so
// there is no step-template let block in scope, and sub-project E3's
// section 2.2 ruling plus the
// 7.3.1--step-name-in-job-environment-let.invalid.yaml fixture require the
// negative half to hold. env.StepEnvironment is the same bit Step.Name
// keys off, for the same reason.
//
// An error is returned when an embedded file's Name/Filename is invalid
// (see [EmbeddedFileName]), when a PATH parameter's value cannot be mapped,
// or when any step-template let: binding fails to parse or evaluate.
func EnvSymbols(
	msg *protocol.AssignMsg, env *protocol.AssignEnvironment, workDir, pathMapFile string, hasPathMap bool,
) (expr.MapSymbols, error) {
	syms := expr.MapSymbols{}
	if err := bindJobParamSymbols(msg, syms); err != nil {
		return nil, err
	}
	bindSessionSymbols(syms, workDir, pathMapFile, hasPathMap)
	syms["Job.Name"] = expr.String(msg.JobName)
	if env == nil {
		return syms, nil
	}
	if env.StepEnvironment {
		syms["Step.Name"] = expr.String(msg.StepName)
	}
	if err := bindFileSymbols(syms, "Env.File", env.EmbeddedFiles, workDir); err != nil {
		return nil, err
	}
	if env.StepEnvironment {
		if err := applyStepTemplateLet(msg.StepTemplateLet, syms, ExprEvalOptions(msg.PathMap)); err != nil {
			return nil, err
		}
	}
	return syms, nil
}

// bindJobParamSymbols binds Param.<name> and RawParam.<name> for every entry
// in msg.JobParameters, typed from msg.JobParameterTypes[name] via
// expr.JobParamTypes -- the SAME mapping internal/openjd's phase-2
// symbolsFor uses (exprcheck.go's jobParamTypes now delegates to it), so a
// declared type cannot type differently between the two phases. A name
// missing from JobParameterTypes floors to expr.TAny via the same
// unrecognized-spelling rule expr.JobParamTypes applies to any unparseable
// spelling -- defensive only; JobParameterTypes and JobParameters share a
// key set in production (buildJobParameterTypes/buildJobParameters,
// internal/scheduler/assign.go, both walk the same parameterDefinitions/
// bound-values source).
//
// Param.<name> goes through [paramValueForBinding], which applies
// msg.PathMap for a PATH-declared parameter (section 1.2.2: "path mapping
// rules already applied"); RawParam.<name> is always built straight from
// the untouched raw text ("the original unmapped value"), never mapped --
// even where rawType is itself CodePath, which cannot happen for a JOB
// parameter (expr.JobParamTypes degrades RawParam to string for every PATH-
// family declared type) but IS reachable for a TASK parameter, see
// bindTaskParamSymbols below.
//
// An error is returned only when mapping a PATH parameter's value fails --
// see [mapPathParamValue]; ordinary INT/FLOAT/STRING parsing failures still
// fall back to expr.Unresolved exactly as before, unchanged.
func bindJobParamSymbols(msg *protocol.AssignMsg, syms expr.MapSymbols) error {
	// Sorted, because the loop returns on the FIRST failure: iterating the
	// map directly makes an assignment with two bad PATH parameters report a
	// different one run to run, which is not something an operator reading a
	// task failure should have to notice.
	for _, name := range slices.Sorted(maps.Keys(msg.JobParameters)) {
		raw := msg.JobParameters[name]
		paramType, rawType := expr.JobParamTypes(msg.JobParameterTypes[name])
		v, err := paramValueForBinding(paramType, raw, msg.PathMap)
		if err != nil {
			return fmt.Errorf("job parameter %q: %w", name, err)
		}
		syms["Param."+name] = v
		syms["RawParam."+name] = expr.ValueFromText(rawType, raw, pathFlavor)
	}
	return nil
}

// bindTaskParamSymbols binds Task.Param.<name> and Task.RawParam.<name> for
// every entry in msg.Parameters, typed from msg.ParameterTypes[name] via
// expr.TaskParamType.
//
// Unlike job parameters, sqi's assignment carries only ONE value per
// task-parameter name (no separate raw-vs-path-mapped wire variant), so
// before this fix round Task.Param and Task.RawParam were bound from
// IDENTICAL raw text. That was always wrong for a PATH-declared task
// parameter -- section 1.2.2 gives Task.RawParam the SAME type as
// Task.Param (unlike the job-parameter pair, which degrades to string) but
// still distinguishes them by whether mapping was applied -- and is fixed
// the same way as bindJobParamSymbols: Task.Param.<name> goes through
// [paramValueForBinding] (mapped when t is TPath), Task.RawParam.<name>
// stays built straight from raw (unmapped), even when both share the exact
// same declared TYPE.
func bindTaskParamSymbols(msg *protocol.AssignMsg, syms expr.MapSymbols) error {
	// Sorted for the same reason as bindJobParamSymbols, above.
	for _, name := range slices.Sorted(maps.Keys(msg.Parameters)) {
		raw := msg.Parameters[name]
		t := expr.TaskParamType(msg.ParameterTypes[name])
		v, err := paramValueForBinding(t, raw, msg.PathMap)
		if err != nil {
			return fmt.Errorf("task parameter %q: %w", name, err)
		}
		syms["Task.Param."+name] = v
		syms["Task.RawParam."+name] = expr.ValueFromText(t, raw, pathFlavor)
	}
	return nil
}

// paramValueForBinding builds the CONCRETE value a Param.<name> or
// Task.Param.<name> symbol binds to: expr.ValueFromText for every declared
// type except a bare PATH, which instead runs the assignment's real
// path-mapping rules over raw via [mapPathParamValue] -- section 1.2.2's
// "path mapping rules already applied" requirement for Param/Task.Param,
// which RawParam/Task.RawParam (bound separately, from the SAME raw text,
// by each caller above) deliberately does not get.
//
// t.Equal(expr.TPath) rather than t.Code == expr.CodePath is deliberate:
// LIST[PATH] job parameters stay expr.Unresolved regardless (sqi's template
// model cannot declare a concrete list value yet -- expr.ValueFromText's own
// list-of-anything case; see exprsyms_test.go's
// TestTaskSymbols_JobParamTypes), so there is no concrete LIST[PATH] value
// for path mapping to ever run against today. Equal, not Code alone, so a
// bare path[T]-shaped union or unresolved wrapper (neither reachable here in
// practice) does not accidentally match a scalar-only branch.
func paramValueForBinding(t expr.Type, raw string, pathMap []protocol.PathMapRule) (expr.Value, error) {
	if t.Equal(expr.TPath) {
		return mapPathParamValue(raw, pathMap)
	}
	return expr.ValueFromText(t, raw, pathFlavor), nil
}

// bindSessionSymbols binds the three fixed Session.* symbols shared by
// TaskSymbols and EnvSymbols. Mirrors [addPathMappingKeys]'s existing
// (pre-EXPR) rule for the path-mapping pair: Session.PathMappingRulesFile is
// bound only when hasPathMap is true, so a reference to it with no rules
// present fails as an unknown symbol rather than resolving to an empty
// path.
func bindSessionSymbols(syms expr.MapSymbols, workDir, pathMapFile string, hasPathMap bool) {
	syms["Session.WorkingDirectory"] = expr.Path(workDir, pathFlavor)
	syms["Session.HasPathMappingRules"] = expr.Bool(hasPathMap)
	if hasPathMap {
		syms["Session.PathMappingRulesFile"] = expr.Path(pathMapFile, pathFlavor)
	}
}

// bindFileSymbols binds "<prefix>.<name>" as a concrete path Value for every
// entry in files, reusing [AddFileVars]'s own filename computation via a
// scratch fmtstring.MapScope rather than recomputing it -- the string path
// AddFileVars produces and the path Value bound here must be the exact same
// on-disk location, and calling the one existing computation is what makes
// that true by construction instead of by two implementations happening to
// agree.
func bindFileSymbols(syms expr.MapSymbols, prefix string, files []protocol.EmbeddedFile, workDir string) error {
	scratch := fmtstring.MapScope{}
	if err := AddFileVars(scratch, prefix, files, workDir); err != nil {
		return err
	}
	for k, v := range scratch {
		syms[k] = expr.Path(v, pathFlavor)
	}
	return nil
}

// ─── let bindings (EXPR sub-project E4a, Task 5) ────────────────────────────
//
// Section 2 of the design spec (docs/superpowers/specs/
// 2026-08-09-expr-phase3-worker-design.md) explains why a let: block cannot
// be pre-resolved server-side: a StepScript or EnvironmentScript binding may
// reference Task.Param.*, Task.File.*, Env.File.* or Session.* -- none of
// which exist until a session directory has been created on a specific
// worker host. So Task 2 shipped the RAW binding strings on the wire
// (AssignMsg.StepTemplateLet/StepScriptLet, AssignEnvironment.Let), and this
// section re-evaluates them here, against the phase-3 tables TaskSymbols/
// EnvSymbols already build -- the same "re-evaluate, don't transport a
// value" shape phase 2 already uses for job/task parameters.
//
// The mechanism mirrors internal/openjd/exprcheck.go's checkLetBindings,
// which is this function's phase-1/phase-2 counterpart and was read first,
// as the brief for this task requires: bindings are evaluated in DECLARATION
// ORDER, each successful result is inserted into syms so a LATER binding (in
// the same block, or -- for the step-level pair -- the following block) can
// reference it, a FAILED binding is not inserted (so a later reference to it
// reports unknown-symbol rather than a second copy of the same fault), and
// evaluation CONTINUES past a failure rather than aborting the rest of the
// block. Section 3.6's shadowing rule -- "a let binding may not shadow a
// previous binding in the same let block or any enclosing scope" -- is
// enforced the identical way checkLetBindings enforces it: a name already
// present as a KEY, checked by lookup rather than against a parallel name
// list, so it equally catches a collision with a spec symbol (unreachable
// from a submitted template only because section 3.6.1's lowercase-first
// <UserIdentifier> grammar keeps the two namespaces disjoint, and
// splitLetBinding deliberately does not re-check that grammar on wire data).
//
// FIX ROUND 2 (whole-branch review, Important 3): the lookup is against the
// table the results are MERGED INTO, which for the step-template block is
// NOT the (narrowed) table the block is evaluated against. Fix round 1 added
// the ScopeStepTemplate narrowing and left the shadow check keyed off the
// narrowed view, which re-opened a hole a previous reviewer had verified
// closed: a binding named for a spec symbol OUTSIDE the projection --
// "Session.WorkingDirectory = \"/pwned\"", "Task.Param.N = 999" -- passed the
// check (the projection does not hold those keys) and then OVERWROTE the real
// symbol on merge, so an onRun rendered "/pwned" and "999" with no error at
// all. Only "Step.Name", which the projection does hold, was reported.
// evalLetBindings therefore takes the merge target separately, as outer.
// No privilege boundary moved -- a submitted template cannot reach this,
// parseLetBinding rejecting a dotted name at submit -- but the invariant
// "a let binding never silently replaces a spec symbol" is not one to hold
// only by accident of which table was handy.
//
// Three deliberate differences from checkLetBindings, all because phase 3 is
// an EXECUTION phase with no author to hand a JSON-pointer-keyed diagnostic
// back to, unlike phase 1/2's synchronous validation request:
//
//  1. checkHostOnlyFunctions has no counterpart here.
//
//     FIX ROUND 1 (Task 5 review): an earlier revision of this comment
//     claimed phase 3 is "always a host context in the sense
//     Scope.IsHostContext means" -- that is FALSE and was corrected.
//     IsHostContext (scope.go) is a POSITIVE list of ScopeJobEnvironment,
//     ScopeStepEnvironment and ScopeStepScript; ScopeStepTemplate --
//     StepTemplateLet's own phase-1/2 scope, and that constant's own doc
//     comment names it as exactly the case that exposed an earlier
//     negation-based IsHostContext as wrong -- returns false. A
//     StepTemplateLet binding is evaluated at submission time server-side,
//     specifically BECAUSE it must not see host state, and this file's own
//     stepTemplateLetScope (below) now enforces that at phase 3 too, so the
//     claim "there is no non-host-context phase-3 position" was simply
//     wrong on its own terms even before considering functions.
//
//     The missing gate is still safe, for a narrower reason: hostOnlyFunctions
//     (exprcheck.go) has exactly one member, apply_path_mapping, which is
//     side-effect-free (it returns a mapped path; it does not touch
//     anything), and anyone able to forge StepTemplateLet already controls
//     Command/Args on the same unauthenticated AssignMsg -- gating THIS one
//     function here would not shrink what a forged assignment can already
//     do. STANDING CAVEAT: if a second, side-effecting function ever joins
//     hostOnlyFunctions, this reasoning stops holding and this file gains no
//     gate of its own automatically -- nothing here would fail, silently.
//     Revisit this comment (and consider a real checkHostOnlyFunctions
//     counterpart) the day hostOnlyFunctions grows a second member.
//  2. Errors are collected and returned as ONE joined error (errors.Join),
//     not a ValidationErrors slice keyed by JSON pointer. A worker task fails
//     with a single error message, not a structured multi-error report meant
//     for a template author's editor.
//  3. [splitLetBinding] does not re-validate section 3.6.1's <UserIdentifier>
//     character-class grammar the way internal/openjd's own parseLetBinding
//     does (letbinding.go) -- see that function's own doc comment for why: it
//     is a submission-time-only concern the server has already enforced by
//     the time an assignment reaches the worker. It DOES trim differently:
//     splitLetBinding uses strings.TrimSpace where parseLetBinding uses
//     strings.Trim(s, " \t"), because section 3.6.1's <WS> is deliberately
//     tab-and-space only. The two agree on every binding the server would
//     ever accept (a name/expression the server validated never carries a
//     newline or other space character at its edges); the wider trim here
//     only ever matters for a binding the server has ALREADY rejected, which
//     is not a case phase 3 needs to reproduce faithfully.
//
// The 50-binding cap (maxLetBindings, mirroring internal/openjd/validate.go's
// constant of the same name) is enforced here exactly as checkLetBindings
// enforces it, for the identical reason: sub-project E3's whole-branch review
// found a 183 KB template body reaching 6.9 GB in 1.45s because the cap was
// REPORTED (validateLetElementCounts) but never ENFORCED at the evaluator
// that actually retains the bindings' values.
//
// Which of the two server-side halves does what matters, and an earlier
// revision of this comment got it backwards. checkLetBindings' enforcement
// TRUNCATES: it evaluates the first 50 and silently drops the rest, exactly
// as this function does, which is a COST bound, not a rejection.
// validateLetElementCounts is what actually REJECTS the template -- and
// validate.go's own constant says so in as many words ("Reporting is not
// guarding -- ValidateWithOptions does not short-circuit on the report -- so
// both are required"). The worker has neither reporter nor rejector of its
// own; it relies on validateLetElementCounts having already failed the
// submission, and truncates here as defense in depth for a phase-3 evaluator
// that should never legitimately see more than 50 bindings in a block. The
// bound that actually protects the worker against an accepted-but-expensive
// block is workerLetRetainedLimit, below.

// maxLetBindings caps the number of bindings evalLetBindings will evaluate
// from a single let: block -- see this section's own doc comment, above, for
// why this mirrors (does not import; internal/openjd is not reachable from
// the worker binary) internal/openjd/validate.go's constant of the same
// name and value.
const maxLetBindings = 50

// workerLetRetainedLimit caps the total section 1.3.9 size of everything ONE
// phase-3 symbol table holds live, measured across every let: block evaluated
// into it.
//
// EXPR sub-project E4a whole-branch review, Critical 2. expres.go's
// workerOperationLimit/workerMemoryLimit are PER-Eval budgets, and a let:
// block is the one construct in this package that RETAINS a result rather
// than rendering it and dropping it: every binding got a fresh 20 MB budget
// it was then allowed to keep. maxLetBindings bounds the COUNT, so the
// structural ceiling was maxLetBindings x workerMemoryLimit = 1 GB per block,
// two blocks per task table plus one per environment table, all of them live
// concurrently across a worker's task slots. Measured through the real
// ApplyTaskLet, 50 bindings of `a<i> = "x" * (Task.Param.N * 100000)` with
// N = 99 retained 495 MB in 49 ms -- and phase 2 CANNOT reject that template,
// because Task.Param.N is unresolved[int] at submission, so `"x" *
// unresolved` costs nothing under submissionLimits(). An ordinary jobs.write
// user could therefore OOM-kill sqi-worker and take down every unrelated
// task on the host. That is E3's server-side Critical transposed and worse:
// server-side the same gap is bounded by a 50 MB request-scoped table in a
// process whose death is one restart.
//
// This is the worker-LOCAL bound; the general, template-wide cumulative
// budget remains sub-project E4c's. It is deliberately a RETAINED-BYTES
// bound rather than an operation bound, because the construction above
// spends almost no operations -- an operation budget would not have caught
// it.
//
// Enforced by measuring the table itself (tableRetainedBytes) rather than by
// threading an accumulator between the callers, which is what makes it a
// PER-TABLE limit for free: the step-template block's results are merged into
// the same table the step-script block then measures, and EnvSymbols' merged
// step-template names are in the table ApplyEnvLet measures. Nothing has to
// remember to pass a budget along. The table measured is always the one the
// bindings LAND in, never the narrower one a block may resolve against --
// see evalLetBindings' mergeTarget, and fix round 3's note there for the
// measurement that showed why the distinction is load-bearing.
//
// 10 MB is the number: generous for any legitimate block (bindings are
// derived from parameters and path text, and phase 2 type-checks the whole
// block inside a 1 MB per-Eval budget), while making the worst case an
// operator can be handed 10 MB retained plus at most one in-flight
// evaluation's workerMemoryLimit -- 30 MB per table, not 2 GB.
//
// TWO ROUGH EDGES, both for E4d (operator configuration) rather than for a
// hardcoded constant to solve, recorded so the next reader does not mistake
// either for an oversight:
//
//  1. 10,000,000 is EXACTLY limits.go's maxStringBytes, so the largest single
//     string the evaluator can produce at all can never be bound, even into
//     an otherwise empty table. That is a coincidence of two independently
//     chosen round numbers, not a designed relationship; the day the limit
//     becomes configurable it should not default to sitting exactly on
//     another bound.
//  2. Because the accounting measures the whole table (tableRetainedBytes),
//     a job whose own parameters are already large -- a big LIST[PATH], a
//     multi-megabyte STRING -- spends budget its let: block never asked for,
//     and fails at EXECUTION with no phase-2 counterpart to warn the
//     submitter. Metering only let-bound names would trade that for the
//     opposite defect (a table could then hold parameters plus a full budget
//     of bindings), so the whole-table measurement is the right one here;
//     what it needs is a knob, not a different formula.
const workerLetRetainedLimit int64 = 10_000_000

// splitLetBinding splits one raw "<name> = <expression>" binding into its
// bound name and unparsed expression source. The split happens at the FIRST
// "=" byte, matching internal/openjd's parseLetBinding (letbinding.go): a
// legal <UserIdentifier> never contains "=", so the first occurrence in the
// string always ends the name correctly, however many more "=" bytes the
// expression itself goes on to contain (e.g. "x = a == b" must yield the
// expression "a == b" intact, not "a ").
//
// This does NOT re-validate section 3.6.1's <UserIdentifier> character-class
// grammar (lowercase-first start, alnum/underscore continuation, 1-512
// length) the way parseLetBinding does -- see this section's own doc comment
// for why: that grammar is a submission-time concern the server has already
// enforced (checkLetBindings runs at both ValidateWithOptions and submit,
// both upstream of the assignment this string arrived on) by the time this
// function ever sees the string.
func splitLetBinding(raw string) (name, exprSrc string, err error) {
	before, after, found := strings.Cut(raw, "=")
	if !found {
		return "", "", fmt.Errorf("let binding %q: must be of the form \"name = expression\"", raw)
	}
	name = strings.TrimSpace(before)
	exprSrc = strings.TrimSpace(after)
	if name == "" {
		return "", "", fmt.Errorf("let binding %q: missing name before \"=\"", raw)
	}
	if exprSrc == "" {
		return "", "", fmt.Errorf("let binding %q: missing expression after \"=\"", raw)
	}
	return name, exprSrc, nil
}

// evalLetBindings evaluates one let: block's raw bindings against syms,
// MUTATING it in place -- see this section's own doc comment for the full
// mechanism this mirrors from checkLetBindings. label identifies the block
// in a returned error ("step template let", "step script let",
// "environment let"); each binding's position within the (possibly
// truncated) block is included alongside it, so a block with more than one
// failure names each one distinctly rather than repeating the same label.
//
// mergeTarget is the table this block's results will ultimately land in,
// when that is a WIDER table than the syms the block RESOLVES against. That
// is the case for exactly one caller, [applyStepTemplateLet], which resolves
// against a ScopeStepTemplate projection and merges into the full phase-3
// table; every other caller passes nil, because syms is itself the target.
//
// It is used for two things, and both must key off the target rather than
// the projection or they are wrong in the same way:
//
//   - the SHADOW CHECK, or a binding named for a spec symbol the projection
//     drops passes the check and then overwrites the real symbol on merge;
//   - the RETAINED-BYTES accounting, or the budget is measured against a
//     table smaller than the one the bytes end up in, and the stated
//     invariant ("no phase-3 symbol table retains more than
//     workerLetRetainedLimit bytes") is simply false.
//
// FIX ROUND 3 (re-review): fix round 2 introduced mergeTarget for the shadow
// half only and left the byte accounting on syms, which is the identical
// defect one instance further on. Measured: a task table holding a 3 MB
// STRING task parameter starts at 6.00 MB; the step-template block measured
// only the 0.00 MB projection, admitted a 9 MB binding, and merged it, so the
// table held 15.00 MB against a 10 MB limit. The real ceiling was
// "workerLetRetainedLimit plus whatever the projection excludes", which is
// not a bound at all -- Task.Param./Task.RawParam./Task.File./Session./
// Env.File. are all outside the projection.
//
// Every error encountered is collected rather than stopping the block, and
// errors.Join'd into one returned error -- nil when every binding in the
// (possibly truncated) block evaluated successfully. The one exception is
// the retained-bytes bound, which stops the block; see workerLetRetainedLimit.
//
// budget is EXPR sub-project E4c's Task 4 addition: charged the SAME size
// (expr.SizeOf(v)) already computed for the per-table workerLetRetainedLimit
// check, immediately after that check passes -- so a binding that survives
// its own table's 10 MB ceiling but would push the WHOLE ASSIGNMENT'S
// retained bytes (task table plus every environment's) over
// assignmentMaxRetainedBytes still stops the block, exactly as the per-table
// check does, just against a wider ledger. See [AssignmentBudget]'s own doc
// comment for the full account. Omitting it (every pre-Task-4 caller) charges
// a fresh, throwaway budget that can never trip from one block alone.
func evalLetBindings(
	label string, lets []string, syms, mergeTarget expr.MapSymbols, opts []expr.Option, budget ...*AssignmentBudget,
) error {
	if len(lets) > maxLetBindings {
		lets = lets[:maxLetBindings]
	}

	// The projection's keys are a strict subset of the target's, holding the
	// identical Values (stepTemplateLetScope filters the target rather than
	// re-deriving anything), so measuring the target alone counts every live
	// value exactly once -- no double counting, nothing missed.
	budgeted := syms
	if mergeTarget != nil {
		budgeted = mergeTarget
	}

	taken := func(name string) bool {
		if _, ok := syms[name]; ok {
			return true
		}
		_, ok := budgeted[name]
		return ok
	}

	retained := tableRetainedBytes(budgeted)
	b := assignmentBudgetOrFresh(budget)
	var errs []error
	for i, raw := range lets {
		if err := b.Err(); err != nil {
			// The ASSIGNMENT-wide budget (charged by an earlier binding in
			// THIS block, an earlier block on THIS table, or a DIFFERENT
			// table this same assignment already built) is already spent.
			// Stop before evaluating another binding -- there is nothing
			// left to spend it on, and every remaining binding would just
			// repeat the same fault.
			errs = append(errs, fmt.Errorf("%s[%d]: %w", label, i, err))
			break
		}
		name, src, err := splitLetBinding(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s[%d]: %w", label, i, err))
			continue
		}

		if taken(name) {
			errs = append(errs, fmt.Errorf(
				"%s[%d]: let binding %q shadows a name already in scope", label, i, name,
			))
			continue
		}

		e, err := expr.Parse(src)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s[%d] (%s): %w", label, i, name, err))
			continue
		}
		v, err := e.Eval(syms, expr.TAny, opts...)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s[%d] (%s): %w", label, i, name, err))
			continue
		}

		// STOP the block, do not "continue": every failure mode above is a
		// fault in ONE binding that the rest of the block can be evaluated
		// past, but this one says the table is full. Continuing would keep
		// spending a fresh workerMemoryLimit per remaining binding for
		// nothing -- which is the exhaustion this guard exists to prevent.
		// The offending value is dropped rather than bound, so the table
		// never exceeds the limit.
		size := expr.SizeOf(v)
		if retained+size > workerLetRetainedLimit {
			errs = append(errs, fmt.Errorf(
				"%s[%d] (%s): let bindings would retain %d bytes, over this symbol table's %d-byte limit",
				label, i, name, retained+size, workerLetRetainedLimit,
			))
			break
		}
		if err := b.ChargeRetainedBytes(size, fmt.Sprintf("%s[%d] (%s)", label, i, name)); err != nil {
			errs = append(errs, err)
			break
		}
		retained += size
		syms[name] = v
	}
	return errors.Join(errs...)
}

// tableRetainedBytes reports the total section 1.3.9 size of every value syms
// currently holds live -- the quantity workerLetRetainedLimit bounds.
//
// It measures the WHOLE table, not just let-bound names: the invariant being
// maintained is "one phase-3 symbol table never retains more than
// workerLetRetainedLimit bytes", and a table's spec symbols (a big STRING or
// PATH parameter's value, an embedded file's path) are retained bytes too.
// Callers must therefore hand it the table the bindings will land in -- see
// evalLetBindings' mergeTarget. Cost is O(len(syms)) once per let: block --
// a few dozen entries.
//
// Note the consequence, which is deliberate and belongs to E4d: a job whose
// own parameters are already large consumes budget its let: block never asked
// for, and can fail at execution with no phase-2 counterpart. Operator
// configuration of the limit is where that gets a knob.
func tableRetainedBytes(syms expr.MapSymbols) int64 {
	var total int64
	for _, v := range syms {
		total += expr.SizeOf(v)
	}
	return total
}

// stepTemplateLetScope filters full -- a phase-3 TaskSymbols table -- down to
// exactly the symbols internal/openjd's ScopeStepTemplate exposes: Job.Name,
// Step.Name, Param.<name> and RawParam.<name> (scope.go's scopeFixed/
// scopeFamilies for ScopeStepTemplate). Nothing else -- no Task.*, no
// Session.*, no *.File.* -- because a step template's own let: block
// (Template Schemas 3.6.2 row 1) is evaluated by checkStepExpressions at
// ScopeStepTemplate, which is NOT ScopeStepScript: it is checked at
// SUBMISSION time, before any session or task exists, so a
// StepTemplateLet binding referencing Session.WorkingDirectory or
// Task.Param.* is an unknown-symbol error at phase 2, and phase 3 must
// reject it the identical way -- not silently resolve it just because the
// full TaskSymbols table happens to have the value available by the time a
// worker runs it.
//
// FIX ROUND 1 (Task 5 review): this function did not exist in the first
// round. ApplyTaskLet evaluated BOTH msg.StepTemplateLet and
// msg.StepScriptLet against the one full table, so a StepTemplateLet
// binding could see (and phase 2 would reject) Session.*/Task.*/*.File.* --
// the "one walk, different table" claim did not hold for this block. See
// ApplyTaskLet's own doc comment for how the diff this function enables gets
// carried forward into the script's block, mirroring checkStepExpressions'
// preLetKeys/stepLet mechanism (exprcheck.go) exactly.
//
// Filtering the ALREADY-BUILT full table -- rather than re-deriving
// Param./RawParam./Job.Name/Step.Name a second time from msg -- keeps this
// one formula: the exact same Values TaskSymbols computed are what a
// StepTemplateLet binding sees, just gated to the narrower key set, with no
// second, independently maintained construction that could drift from
// TaskSymbols' own.
//
// FIX ROUND 2 (whole-branch review, Important 3): the table returned here is
// what the block RESOLVES against, and nothing more. It is NOT what the
// block's shadow check consults -- that must be the merge target, or a
// binding named for a spec symbol this projection drops passes the check and
// then overwrites the real symbol. [applyStepTemplateLet] passes both.
func stepTemplateLetScope(full expr.MapSymbols) expr.MapSymbols {
	out := expr.MapSymbols{}
	for k, v := range full {
		switch {
		case k == "Job.Name", k == "Step.Name":
			out[k] = v
		case strings.HasPrefix(k, "Param."), strings.HasPrefix(k, "RawParam."):
			out[k] = v
		}
	}
	return out
}

// ApplyTaskLet evaluates a task's phase-3 let: bindings over syms -- the
// step template's block (msg.StepTemplateLet) first, then the step script's
// (msg.StepScriptLet) -- mutating syms in place. This is Template Schemas
// section 3.6.2 row 1/2's ordering, and the reason the two blocks travel as
// separate wire fields rather than being concatenated: the script's block
// may not shadow a name the template's block already bound, which is only
// enforceable if the two stay distinguishable (protocol.go's own doc
// comment on StepTemplateLet/StepScriptLet makes the same point).
//
// The two blocks do NOT share one table outright, mirroring
// checkStepExpressions (exprcheck.go) exactly rather than the simpler
// "evaluate both over syms" a first attempt at this function used (see
// stepTemplateLetScope's own doc comment for that history): the template's
// block is evaluated against [stepTemplateLetScope]'s NARROWER view of syms,
// then only the names it successfully bound are merged into the full syms
// before the script's block -- which DOES see the full table -- runs. A name
// the template's block bound is therefore visible to the script's block
// (and, per shadow rejection, may not be rebound by it), but a
// Session.*/Task.*/*.File.* symbol the template's block tried to reference
// is not, exactly as ScopeStepTemplate denies it server-side.
//
// CALL THIS ONCE PER TABLE. It mutates syms by inserting every successfully
// bound name (both blocks') directly into it -- calling it a second time
// over the SAME syms makes every binding in both blocks fail the shadow
// check, because every name is now already present as a key. A caller that
// resolves more than one action/embedded-file/variable against the same
// task (e.g. ResolveActionExpr called once per action) must call
// ApplyTaskLet exactly once to build the table, then reuse that same syms
// for every resolution -- not call ApplyTaskLet again per resolution.
//
// Callers pass the result of TaskSymbols as syms -- ApplyTaskLet is a
// SEPARATE step from building the base table, not folded into TaskSymbols
// itself. Two reasons: TaskSymbols has its own callers that want the base
// job/task/session table alone, with no let: block involved (Task 3's own
// tests, and TestPhase2Phase3Agreement's key-set comparison against
// SymbolsFor, which likewise never touches let); and the LET evaluation
// needs the metering options (below) that only a caller resolving pathMap
// can supply, which TaskSymbols itself has no reason to require.
//
// pathMap is forwarded to ExprEvalOptions, exactly as every other phase-3
// evaluator in this package requires -- see ResolveActionExpr's doc comment
// for why this is a plain parameter rather than a variadic opts tail: taking
// the raw ingredient closes off the possibility of a caller building an
// options value, empty or otherwise, that skips metering.
//
// Returns a non-nil error if any binding in either block failed to evaluate
// (errors.Join of every failure across both blocks) -- syms still holds
// every binding that DID succeed, since a failed binding does not stop the
// walk; see this section's own doc comment for why continuing past a
// failure, rather than aborting, is the intended behavior.
//
// budget is EXPR sub-project E4c's Task 4 addition: forwarded verbatim to
// both blocks' evalLetBindings calls, so the task's own let: retention is
// charged against the SAME [AssignmentBudget] every environment table this
// assignment builds shares -- see session.Session's own accessor for how the
// one budget object reaches every phase-3 call site for one assignment.
func ApplyTaskLet(msg *protocol.AssignMsg, syms expr.MapSymbols, pathMap []protocol.PathMapRule, budget ...*AssignmentBudget) error {
	opts := ExprEvalOptions(pathMap)
	tmplErr := applyStepTemplateLet(msg.StepTemplateLet, syms, opts, budget...)
	scriptErr := evalLetBindings("step script let", msg.StepScriptLet, syms, nil, opts, budget...)
	return errors.Join(tmplErr, scriptErr)
}

// applyStepTemplateLet evaluates a step template's let: block (lets) against
// [stepTemplateLetScope]'s narrowed view of syms and merges the names it
// newly bound back into syms, mutating it in place. It is the shared half of
// [ApplyTaskLet] and [EnvSymbols]: Template Schemas section 3.6.2 row 1 makes
// the SAME block's names visible to a step's script AND to its
// stepEnvironments, and evaluating it from one function is what keeps those
// two positions from drifting apart the way they did before the E4a
// whole-branch review (see EnvSymbols' own doc comment).
//
// The merge is a set-difference, not a wholesale copy of the narrowed table:
// the narrowed table also holds Job.Name/Step.Name/Param.*/RawParam.*, which
// syms already has (they are where the narrowed view came from). This mirrors
// checkStepExpressions' preLetKeys/stepLet computation (exprcheck.go) exactly.
func applyStepTemplateLet(lets []string, syms expr.MapSymbols, opts []expr.Option, budget ...*AssignmentBudget) error {
	tmplScope := stepTemplateLetScope(syms)
	preLetKeys := make(map[string]struct{}, len(tmplScope))
	for k := range tmplScope {
		preLetKeys[k] = struct{}{}
	}
	err := evalLetBindings("step template let", lets, tmplScope, syms, opts, budget...)
	for k, v := range tmplScope {
		if _, existed := preLetKeys[k]; !existed {
			syms[k] = v
		}
	}
	return err
}

// ApplyEnvLet is ApplyTaskLet's environment counterpart: it evaluates env's
// own script let: block (AssignEnvironment.Let, Template Schemas section
// 3.6.2 row 4) over syms, mutating it in place. A nil env is a no-op
// returning nil, matching EnvSymbols' own nil handling for the same
// parameter.
//
// It does NOT evaluate the enclosing step template's block (section 3.6.2
// row 1): [EnvSymbols] already folded those names into syms when it built
// the table, so they are visible here -- and, per section 3.6, a binding in
// this block that tries to rebind one of them is rejected by the shadow
// check rather than silently overwriting it.
//
// CALL THIS AFTER RESOLVING env.Variables, not before. Section 3.6.2 row 4
// grants an environment script's let: names to the script's own children --
// actions and embeddedFiles -- and Variables is a sibling of Script on the
// parent Environment, not a descendant of it, so a variable value may NOT
// see them. Phase 2 encodes that by checking Variables against baseSyms
// before cloning it into scriptSyms and binding the block
// (checkEnvironmentExpressions, exprcheck.go); phase 3 encodes it by
// ordering the two calls, since it mutates one table rather than cloning.
//
// Callers pass the result of EnvSymbols as syms. See [ApplyTaskLet] for
// pathMap and budget.
func ApplyEnvLet(env *protocol.AssignEnvironment, syms expr.MapSymbols, pathMap []protocol.PathMapRule, budget ...*AssignmentBudget) error {
	if env == nil {
		return nil
	}
	return evalLetBindings("environment let", env.Let, syms, nil, ExprEvalOptions(pathMap), budget...)
}
