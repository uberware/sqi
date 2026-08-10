// SPDX-License-Identifier: AGPL-3.0-or-later

package fmtres

// ─── operator-configurable expression limits (worker side) ──────────────────
//
// EXPR sub-project E4d's Task 2: the five numbers that bound what ONE
// assignment may spend inside phase-3 evaluation on a worker host, lifted out
// of package constants so an operator can size them per host. The server's
// four equivalents are internal/openjd's [openjd.ExprLimits]; the two config
// systems are independent by design (design spec §1) -- each worker reads its
// own file, which is what lets a heterogeneous farm size limits to each host's
// real memory.
//
// WHERE THE VALUES COME FROM AND WHERE THEY GO. An operator writes them in the
// worker's expr: block (internal/worker/config's ExprConfig, which validates
// the ranges below at startup); cmd/sqi-worker maps that section to this type
// and hands it to session.NewManager; session.Manager.Create builds ONE
// [AssignmentBudget] per assignment from it; and every phase-3 entry point in
// this package -- TaskSymbols/EnvSymbols, ApplyTaskLet/ApplyEnvLet,
// ResolveActionExpr/ResolveEmbeddedFilesExpr/ResolveVarsExpr -- reads the
// limits back off that budget. The budget is the carrier for all five, not
// only for the two it counts, so there is exactly one object per assignment
// that answers "what may this cost?", and no call site can be metered by a
// different set of numbers than its neighbors.

import "github.com/uberware/sqi/internal/openjd/expr"

// ExprLimits is the worker's phase-3 expression budget, as five numbers.
//
// They split into two per-EVALUATION bounds (OperationLimit, MemoryLimit --
// what ONE expression may cost, section 1.3.10 and section 1.3.9), one
// per-TABLE bound (LetRetainedBytes -- what one phase-3 symbol table may hold
// live), and two per-ASSIGNMENT bounds (AssignmentPositions,
// AssignmentRetainedBytes -- summed across every table one assignment builds).
//
// A ZERO field means "unset, use the default" -- see [ExprLimits.orDefaults].
// A NEGATIVE field means the same thing; neither can ever mean "unlimited",
// which is the one meaning a resource-exhaustion guard must not acquire by
// accident. internal/worker/config rejects an out-of-range configured value at
// startup, long before one could reach here.
//
// The ranges an OPERATOR may choose from are not enforced by this type. They
// live in the Min/Max constants below (and are mirrored, deliberately, by
// internal/worker/config, which must not import this package), so that a test
// can set an absurd value to prove a knob is genuinely wired -- which is
// exactly how this wave's mutation tests work.
type ExprLimits struct {
	// OperationLimit is the section 1.3.10 operation budget for ONE phase-3
	// expression evaluation.
	//
	// POLICY BOUND. No measured worker-side catastrophe is attributable to
	// this dimension: E4a's Critical 2 (50 let bindings retaining 495 MB in
	// 49 ms, an OOM-kill of sqi-worker and with it every unrelated task on the
	// host) was a MEMORY construction that "spends almost no operations", and
	// E4c's own note says an operation budget would not have caught it. What
	// this number bounds is CPU inside one evaluation, charged to the task
	// slot the assignment already occupies -- so an operator who raises it is
	// lengthening their own worst assignment, not exposing the host to a fault
	// that kills unrelated work. That is why its ceiling is sized against what
	// a legitimate template plausibly needs rather than against a measurement.
	//
	// It is nonetheless one of the two multiplicands in the DERIVED cumulative
	// bound for an assignment (AssignmentPositions x OperationLimit), and
	// nothing here bounds wall clock: section 1.3.10 prices 256 bytes at one
	// operation, so a raised value buys proportionally more byte-heavy work
	// per position. Design spec §4 caveats 1 and 3 apply to this knob on the
	// worker exactly as they do to the server's.
	OperationLimit int64

	// MemoryLimit is the section 1.3.9 live-byte budget for ONE phase-3
	// expression evaluation.
	//
	// CATASTROPHE BOUND. It is the multiplicand of the measured OOM: E4a's
	// Critical 2 measured 50 bindings of `a<i> = "x" * (Task.Param.N * 100000)`
	// retaining 495 MB in 49 ms, and recorded the structural ceiling as
	// maxLetBindings x this number = 1 GB per block -- on a shared, long-lived
	// process whose death takes every unrelated task on the host with it, and
	// which phase 2 CANNOT reject (Task.Param.N is unresolved at submission,
	// so `"x" * unresolved` costs nothing there). Raising it raises that
	// ceiling in direct proportion, per concurrently-resolving task slot,
	// which is why the ceiling below is a tight multiple of the default rather
	// than an open-ended range.
	//
	// NOTE FOR ANYONE SIZING IT: this bound and internal/openjd/expr's own
	// fixed maxStringBytes (10,000,000) are NOT the same kind of thing and the
	// default is already ABOVE it. maxStringBytes bounds one PRODUCED STRING;
	// section 1.3.9's meter bounds the SUM of live values and recurses into
	// containers, so a list of two 9 MB strings holds
	// 18,000,128 bytes live with no fixed guard firing (measured). An earlier
	// revision of expres.go's own comment claimed both worker limits "sit
	// comfortably UNDER" the fixed guards;
	// that was false for this one (20,000,000 > 10,000,000) and has been
	// withdrawn.
	MemoryLimit int64

	// AssignmentPositions is how many format-string positions -- a command,
	// one args entry, one embedded file, one variable value -- ONE assignment
	// may resolve, summed across the task's own symbol table and every
	// environment table the session enters.
	//
	// POLICY BOUND, sized against what a real session plausibly needs:
	// E4c's own worked calculation for a generous session (one task action
	// with 30 args and 10 embedded files, plus 50 entered environments at 36
	// entry-side positions each) is 1,841 positions, and the default of 10,000
	// leaves ~5.4x headroom over that. Nothing measured a catastrophe in this
	// dimension on the WORKER; the server's equivalent has one (an 84 KB
	// template of ~2,000 args entries cost 11.3 s of CPU per anonymous
	// request), but the worker is neither anonymous nor synchronous.
	//
	// IT CARRIES A CROSS-BINARY RELATION THIS PACKAGE CANNOT CHECK: an
	// assignment's positions are a SUBSET of its template's, so a value here
	// BELOW the server's own template-position cap is reachable by a template
	// the server ACCEPTED -- failing every task in the job after submission,
	// naming a budget the submitter never saw. E4c pinned that with
	// TestTemplateBudget_WorkerCapIsNotTighter comparing two CONSTANTS. With
	// both sides now configurable the relation is breakable through YAML, and
	// re-closing it is E4d Task 3's work, not this type's. What this file does
	// do about it is set the CEILING (below) equal to the server's own
	// ceiling, so the relation stays satisfiable at every value an operator
	// can legally give the server.
	AssignmentPositions int64

	// AssignmentRetainedBytes is the cumulative section 1.3.9 size of every
	// let-bound value EVERY phase-3 table this assignment builds retains,
	// summed across the task's own table and every environment's.
	//
	// CATASTROPHE BOUND -- the aggregate of the same mechanism MemoryLimit and
	// LetRetainedBytes bound per evaluation and per table. Without it, a
	// session entering N environments could allocate N x LetRetainedBytes over
	// the course of entry with every individual table compliant.
	//
	// IT MEASURES CUMULATIVE ALLOCATION ACROSS ONE SESSION'S ENTRY-TIME
	// EVALUATION, NOT PEAK LIVE RETENTION AT ANY ONE INSTANT (each
	// environment's table is a local variable that becomes GC-eligible once
	// that environment finishes resolving) -- see [AssignmentBudget]'s own doc
	// comment, which states the distinction at length. An operator sizing host
	// RAM against this number over-provisions, and sizing this number against
	// an observed RSS under-bounds it. That is design spec §4 caveat 2, and it
	// belongs in the operator documentation, not only here.
	AssignmentRetainedBytes int64

	// LetRetainedBytes is the total section 1.3.9 size of everything ONE
	// phase-3 symbol table holds live, measured across every let: block
	// evaluated into it.
	//
	// CATASTROPHE BOUND, and the most direct one of the five: it IS the bound
	// E4a's Critical 2 added in response to the measured 495 MB / 49 ms
	// OOM construction described on MemoryLimit. Its absence is not
	// hypothetical.
	//
	// TWO PROPERTIES OF THE DEFAULT WORTH KNOWING BEFORE MOVING IT, both
	// inherited unchanged from the constant it replaces (this wave changes no
	// default, by requirement):
	//
	//  1. 10,000,000 is exactly internal/openjd/expr's maxStringBytes, so the
	//     largest single string the evaluator can produce cannot be bound at
	//     all, even into an empty table (the check is retained+size > limit,
	//     and a 10,000,000-byte string measures slightly more than that once
	//     its value header is counted). E4a recorded that coincidence and said
	//     the day the limit became configurable it should not default to
	//     sitting exactly on another bound. It still does -- moving the
	//     DEFAULT is a behavior change this wave is not permitted to make.
	//     The knob is now the way out for an operator who needs one.
	//  2. The accounting measures the WHOLE table, not only let-bound names,
	//     so a job whose own parameters are large spends budget its let: block
	//     never asked for. That is deliberate (metering only let names would
	//     let a table hold parameters PLUS a full budget of bindings), and E4a
	//     said what it needed was "a knob, not a different formula". This is
	//     the knob.
	LetRetainedBytes int64
}

// The range an operator may configure each of the five to. Out-of-range values
// are rejected at STARTUP by internal/worker/config's validateExpr, which
// carries its own copies of these numbers (that package must not import this
// one -- config is the operator-facing layer, and this is the enforcement
// leaf) and is pinned to them by cmd/sqi-worker's TestExprLimitsBounds_MatchFmtres,
// the one place that may import both.
//
// The CEILINGS encode design spec §3's distinction between a catastrophe bound
// (tight, tied to a measurement) and a policy bound (wide but finite, sized
// against what a legitimate template plausibly needs). Which of the five is
// which, and why, is on each [ExprLimits] field. All three catastrophe
// ceilings are one order of magnitude above their default; both policy
// ceilings are set from a stated relation rather than from a multiple.
//
// The FLOORS follow one principle that does NOT apply on the server: a limit
// tightened too far on a worker rejects work AFTER the job was accepted, once
// per task, naming a budget the submitter never saw. That is the worst failure
// shape available (design spec §2 records it as a measured incident), so every
// floor here is set to at least the largest value a legitimately accepted
// assignment could need, not to the smallest value this repo's own presets
// happen to cost. TestExprLimits_PresetCostsLeaveFloorHeadroom measures the
// presets on every run and fails if any of them comes within 4x of a floor.
const (
	// MinExprOperationLimit / MaxExprOperationLimit bound
	// [ExprLimits.OperationLimit].
	//
	// Floor: the SERVER's own default per-evaluation operation budget
	// (internal/openjd's defaultSubmissionOperations, 10,000). Below that, a
	// worker could reject at execution an expression the server type-checked
	// at submit. This is a minimum, not a guarantee of parity: phase 3
	// evaluates the same expressions against concrete values and E2's Task 10
	// measured it at ~40x phase 1's cost, which is why the DEFAULT here is
	// 100x the server's rather than equal to it.
	//
	// Ceiling: one order of magnitude above the default. Policy, not
	// catastrophe -- but not open-ended either, because nothing meters wall
	// clock and section 1.3.10 prices 256 bytes at one operation, so 10^7
	// operations is already ~2.5 GB of byte-work priced into ONE evaluation,
	// repeated per position.
	MinExprOperationLimit int64 = 10_000
	MaxExprOperationLimit int64 = 10_000_000

	// MinExprMemoryLimit / MaxExprMemoryLimit bound [ExprLimits.MemoryLimit].
	//
	// Floor: the SERVER's own default per-evaluation memory budget
	// (internal/openjd's defaultSubmissionMemoryBytes, 1,000,000), for the
	// same reason as the operation floor -- a phase-3 evaluation holds the
	// concrete value a phase-2 evaluation only had a placeholder for, so a
	// worker allowance below what the server accepted rejects after the fact.
	//
	// Ceiling: one order of magnitude above the default. This is a catastrophe
	// ceiling, and the number it really sets is the per-block structural
	// ceiling maxLetBindings (50) x this value -- 1 GB at the default, 10 GB
	// at the maximum, per concurrently-resolving task slot. An operator
	// choosing the maximum should have sized the host's RAM against that
	// product, not against this number.
	MinExprMemoryLimit int64 = 1_000_000
	MaxExprMemoryLimit int64 = 200_000_000

	// MinExprAssignmentPositions / MaxExprAssignmentPositions bound
	// [ExprLimits.AssignmentPositions].
	//
	// Floor: 2,000, just above E4c's own worked figure of 1,841 positions for
	// a generous-but-plausible real session (see [ExprLimits.AssignmentPositions]).
	// Sizing it against this repo's reference presets instead would have given
	// something near 12, which is exactly the kind of floor that lets an
	// operator tighten into per-task post-acceptance failures.
	//
	// Ceiling: 100,000, which is BOTH one order of magnitude above the default
	// AND exactly internal/openjd's MaxExprTemplatePositions. The second
	// reason is the load-bearing one: the worker's cap must be able to reach
	// the highest value an operator can legally give the server, or the
	// cross-binary relation of design spec §2 would be unsatisfiable by
	// configuration. Keep the two equal.
	MinExprAssignmentPositions int64 = 2_000
	MaxExprAssignmentPositions int64 = 100_000

	// MinExprAssignmentRetainedBytes / MaxExprAssignmentRetainedBytes bound
	// [ExprLimits.AssignmentRetainedBytes].
	//
	// Both ends are exactly 2x their LetRetainedBytes counterparts, preserving
	// the ratio the defaults already carry (20 MB assignment-wide, 10 MB per
	// table): the assignment-wide cap should never be the tighter of the two
	// merely because an operator moved one end of one range. An operator MAY
	// still configure it tighter than the per-table cap -- that is allowed and
	// coherent (the tighter of the two simply becomes the effective one), and
	// deliberately not rejected at load, because tightening is the safe
	// direction and a cross-field rule would make one knob's legal range
	// depend on another's value.
	MinExprAssignmentRetainedBytes int64 = 2_000_000
	MaxExprAssignmentRetainedBytes int64 = 200_000_000

	// MinExprLetRetainedBytes / MaxExprLetRetainedBytes bound
	// [ExprLimits.LetRetainedBytes].
	//
	// Floor: 1,000,000 -- one server-default evaluation's worth of live bytes,
	// i.e. the largest single value phase 2 can pass through to a binding. A
	// table must be able to hold at least one of those, and the whole-table
	// accounting (property 2 on the field) means the job's own parameters are
	// counted alongside it, so anything smaller would be a floor that rejects
	// accepted work.
	//
	// Ceiling: one order of magnitude above the default. Catastrophe bound:
	// this is the dimension the measured 495 MB OOM lived in, and the worst
	// case an operator can hand a host is this value plus one in-flight
	// evaluation's MemoryLimit, per table, per concurrent task slot.
	MinExprLetRetainedBytes int64 = 1_000_000
	MaxExprLetRetainedBytes int64 = 100_000_000
)

// DefaultExprLimits returns the five values this package used as fixed
// constants before E4d. A worker started with no expr: configuration meters
// exactly as every release before E4d did.
func DefaultExprLimits() ExprLimits {
	return ExprLimits{
		OperationLimit:          defaultWorkerOperationLimit,
		MemoryLimit:             defaultWorkerMemoryLimit,
		AssignmentPositions:     DefaultAssignmentPositions,
		AssignmentRetainedBytes: defaultAssignmentRetainedBytes,
		LetRetainedBytes:        defaultLetRetainedBytes,
	}
}

// orDefaults returns l with every unset (<= 0) field replaced by its default.
//
// Applied ONCE, in [NewAssignmentBudget], so every consumption point reads an
// already-normalized value and no call site has to remember to do it. A field
// is treated as unset rather than as "unlimited" because the zero value is the
// overwhelmingly common case (every test in this package that does not care,
// and [assignmentBudgetOrFresh]'s throwaway budget) and because "unlimited" is
// the one meaning these numbers must never acquire by accident.
func (l ExprLimits) orDefaults() ExprLimits {
	d := DefaultExprLimits()
	if l.OperationLimit <= 0 {
		l.OperationLimit = d.OperationLimit
	}
	if l.MemoryLimit <= 0 {
		l.MemoryLimit = d.MemoryLimit
	}
	if l.AssignmentPositions <= 0 {
		l.AssignmentPositions = d.AssignmentPositions
	}
	if l.AssignmentRetainedBytes <= 0 {
		l.AssignmentRetainedBytes = d.AssignmentRetainedBytes
	}
	if l.LetRetainedBytes <= 0 {
		l.LetRetainedBytes = d.LetRetainedBytes
	}
	return l
}

// evalOptions builds the metering half of the [expr.Option] slice every
// phase-3 evaluation runs under. [ExprEvalOptions] adds the path flavor and
// the session's path-mapping rules; this method exists so the two numbers that
// are now CONFIGURATION are assembled in exactly one place.
func (l ExprLimits) evalOptions() []expr.Option {
	return []expr.Option{
		expr.WithOperationLimit(l.OperationLimit),
		expr.WithMemoryLimit(l.MemoryLimit),
	}
}
