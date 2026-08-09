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
// concreteJobParamValue hardcodes: phase 3 runs ON THE WORKER HOST, so this
// is the one evaluation context in sqi today that IS a host context in the
// sense expr.PathFormat's own doc comment describes ("Nothing in sqi
// selects [PathNative] yet; sub-project E does, for host contexts") --
// E4a's whole premise is that the worker is where a template first executes
// against a real machine. Using PathNative here means a worker running on
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
	for name, raw := range msg.JobParameters {
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
	for name, raw := range msg.Parameters {
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
// present as a KEY in syms, checked by lookup, not by a parallel name list,
// so it equally catches a collision with a spec symbol (unreachable in
// practice only because section 3.6.1's lowercase-first <UserIdentifier>
// rule keeps the two namespaces disjoint, not because this code assumes it).
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
// that actually retains the bindings' values. The worker has no
// validateLetElementCounts of its own to report the over-count at all -- an
// over-long block simply never reaches a running task, because
// checkLetBindings' own enforcement already rejects the template at
// submission -- so this guard is defense in depth for a phase-3 evaluator
// that should never legitimately see more than 50 bindings in a block, not
// the primary bound. Truncating (not erroring) matches checkLetBindings:
// evaluating the first 50 and silently dropping the rest is the same choice
// phase 2 already makes.

// maxLetBindings caps the number of bindings evalLetBindings will evaluate
// from a single let: block -- see this section's own doc comment, above, for
// why this mirrors (does not import; internal/openjd is not reachable from
// the worker binary) internal/openjd/validate.go's constant of the same
// name and value.
const maxLetBindings = 50

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
// Every error encountered is collected rather than stopping the block, and
// errors.Join'd into one returned error -- nil when every binding in the
// (possibly truncated) block evaluated successfully.
func evalLetBindings(label string, lets []string, syms expr.MapSymbols, opts []expr.Option) error {
	if len(lets) > maxLetBindings {
		lets = lets[:maxLetBindings]
	}

	var errs []error
	for i, raw := range lets {
		name, src, err := splitLetBinding(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s[%d]: %w", label, i, err))
			continue
		}

		if _, taken := syms[name]; taken {
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
		syms[name] = v
	}
	return errors.Join(errs...)
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
func ApplyTaskLet(msg *protocol.AssignMsg, syms expr.MapSymbols, pathMap []protocol.PathMapRule) error {
	opts := ExprEvalOptions(pathMap)
	tmplErr := applyStepTemplateLet(msg.StepTemplateLet, syms, opts)
	scriptErr := evalLetBindings("step script let", msg.StepScriptLet, syms, opts)
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
func applyStepTemplateLet(lets []string, syms expr.MapSymbols, opts []expr.Option) error {
	tmplScope := stepTemplateLetScope(syms)
	preLetKeys := make(map[string]struct{}, len(tmplScope))
	for k := range tmplScope {
		preLetKeys[k] = struct{}{}
	}
	err := evalLetBindings("step template let", lets, tmplScope, opts)
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
// pathMap.
func ApplyEnvLet(env *protocol.AssignEnvironment, syms expr.MapSymbols, pathMap []protocol.PathMapRule) error {
	if env == nil {
		return nil
	}
	return evalLetBindings("environment let", env.Let, syms, ExprEvalOptions(pathMap))
}
