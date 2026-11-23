// SPDX-License-Identifier: AGPL-3.0-or-later

package fmtres

// ─── operator-configurable expression limits (worker side) ──────────────────
//
// The five numbers that bound what one assignment may spend inside phase-3
// evaluation on a worker host, configurable so an operator can size them per
// host. The server's four equivalents are internal/openjd's
// [openjd.ExprLimits]; the two configurations are independent, since each
// worker reads its own file and a heterogeneous farm can size limits to each
// host's memory.
//
// An operator writes them in the worker's expr: block (internal/worker/config's
// ExprConfig, which validates the ranges below at startup); cmd/sqi-worker maps
// that section to this type and hands it to session.NewManager;
// session.Manager.Create builds one [AssignmentBudget] per assignment from it;
// and every phase-3 entry point in this package (TaskSymbols/EnvSymbols,
// ApplyTaskLet/ApplyEnvLet, ResolveActionExpr/ResolveEmbeddedFilesExpr/
// ResolveVarsExpr) reads the limits back off that budget. The budget carries
// all five, not only the two it counts, so one object per assignment answers
// "what may this cost?" and no call site can be metered by different numbers
// than its neighbors.

import "github.com/uberware/sqi/internal/openjd/expr"

// ExprLimits is the worker's phase-3 expression budget, as five numbers.
//
// They split into two per-evaluation bounds (OperationLimit, MemoryLimit:
// what one expression may cost, section 1.3.10 and section 1.3.9), one
// per-table bound (LetRetainedBytes: what one phase-3 symbol table may hold
// live), and two per-assignment bounds (AssignmentPositions,
// AssignmentRetainedBytes: summed across every table one assignment builds).
//
// A zero or negative field means "unset, use the default" (see
// [ExprLimits.orDefaults]); no value means "unlimited".
// internal/worker/config rejects an out-of-range configured value at startup.
//
// This type does not enforce the ranges an operator may choose from. They
// live in the Min/Max constants below (mirrored by internal/worker/config,
// which must not import this package), so a test can set an out-of-range value
// to prove a knob is wired.
type ExprLimits struct {
	// OperationLimit is the section 1.3.10 operation budget for one phase-3
	// expression evaluation.
	//
	// Policy bound. It bounds CPU inside one evaluation, charged to the task
	// slot the assignment already occupies, so raising it lengthens the
	// operator's own worst assignment without exposing the host to a fault
	// that kills unrelated work. Memory-heavy let: constructions spend almost
	// no operations, so this budget does not bound them; MemoryLimit and
	// LetRetainedBytes do. Its ceiling is sized against what a legitimate
	// template needs.
	//
	// It is one of the two factors in the derived cumulative bound for an
	// assignment (AssignmentPositions x OperationLimit), and nothing here
	// bounds wall clock: section 1.3.10 prices 256 bytes at one operation, so
	// a raised value buys proportionally more byte-heavy work per position.
	// The same applies to the server's operation limit.
	OperationLimit int64

	// MemoryLimit is the section 1.3.9 live-byte budget for one phase-3
	// expression evaluation.
	//
	// Catastrophe bound. A let: block of maxLetBindings (50) bindings such as
	// `a<i> = "x" * (Task.Param.N * 100000)` can retain up to maxLetBindings x
	// this number, 1 GB per block at the default, in a shared, long-lived
	// process whose death ends every task on the host. Phase 2 cannot reject
	// it (Task.Param.N is unresolved at submission, so `"x" * unresolved`
	// costs nothing there). Raising this value raises that ceiling in
	// proportion, per concurrently resolving task slot, so the ceiling below
	// is a tight multiple of the default.
	//
	// This bound and internal/openjd/expr's fixed maxStringBytes (10,000,000)
	// measure different things, and the default is above maxStringBytes.
	// maxStringBytes bounds one produced string; section 1.3.9's meter bounds
	// the sum of live values and recurses into containers, so a list of two
	// 9 MB strings holds 18,000,128 bytes live with no fixed guard firing.
	MemoryLimit int64

	// AssignmentPositions is how many format-string positions (a command, one
	// args entry, one embedded file, one variable value) one assignment may
	// resolve, summed across the task's own symbol table and every environment
	// table the session enters.
	//
	// Policy bound, sized against what a real session needs: a generous
	// session (one task action with 30 args and 10 embedded files, plus 50
	// entered environments at 36 entry-side positions each) resolves 1,841
	// positions, and the default of 10,000 leaves ~5.4x headroom. The worker,
	// unlike the server's API, is neither anonymous nor synchronous.
	//
	// It carries a cross-binary relation this package cannot check: an
	// assignment's positions are a subset of its template's, so a value here
	// below the server's template-position cap is reachable by a template the
	// server accepted, failing every task in the job after submission with a
	// budget the submitter never saw. TestTemplateBudget_WorkerCapIsNotTighter
	// compares the two defaults; because both sides are configurable, the
	// relation is enforced outside this package. This value is advertised to
	// the server in the worker's registration message (protocol.ExprLimits),
	// and internal/scheduler does not dispatch an EXPR job to a worker whose
	// advertised value is below the server's configured one. This file sets
	// the ceiling (below) equal to the server's ceiling, so the relation is
	// satisfiable at every value an operator can legally give the server.
	AssignmentPositions int64

	// AssignmentRetainedBytes is the cumulative section 1.3.9 size of every
	// let-bound value retained by every phase-3 table this assignment builds,
	// summed across the task's own table and every environment's.
	//
	// Catastrophe bound: the aggregate of the mechanism MemoryLimit and
	// LetRetainedBytes bound per evaluation and per table. Without it, a
	// session entering N environments could allocate N x LetRetainedBytes over
	// the course of entry with every individual table compliant.
	//
	// It measures cumulative allocation across one session's entry-time
	// evaluation, not peak live retention (each environment's table is a
	// local variable that becomes GC-eligible once that environment finishes
	// resolving); see [AssignmentBudget]'s doc comment. Sizing host RAM
	// against this number over-provisions, and sizing this number against an
	// observed RSS under-bounds it.
	AssignmentRetainedBytes int64

	// LetRetainedBytes is the total section 1.3.9 size of everything one
	// phase-3 symbol table holds live, measured across every let: block
	// evaluated into it.
	//
	// Catastrophe bound, and the most direct of the five against the let:
	// construction described on MemoryLimit.
	//
	// It is advertised and compared against the server's template-wide
	// retained-bytes budget. The server has no per-table quantity to pair it
	// with, but a template-wide sum is an upper bound on any one of that
	// template's tables, and comparing against it keeps a worker at this
	// field's floor from rejecting, once per task, a let: block the server
	// accepted. internal/scheduler/exprcaps.go gives the full argument,
	// including why comparing against the server's per-evaluation memory
	// limit would be unsound.
	//
	// Two properties of the default to know before changing it:
	//
	//  1. 10,000,000 is internal/openjd/expr's maxStringBytes, so the largest
	//     single string the evaluator can produce cannot be bound, even into
	//     an empty table (the check is retained+size > limit, and a
	//     10,000,000-byte string measures slightly more once its value header
	//     is counted). An operator who needs to bind such a string raises
	//     this knob.
	//  2. The accounting measures the whole table, not only let-bound names,
	//     so a job with large parameters spends budget its let: block never
	//     asked for. Metering only let names would let a table hold its
	//     parameters plus a full budget of bindings; this knob is the
	//     adjustment.
	LetRetainedBytes int64
}

// The range an operator may configure each of the five to. Out-of-range values
// are rejected at startup by internal/worker/config's validateExpr, which
// carries its own copies of these numbers (that package is the operator-facing
// layer and must not import this enforcement leaf) and is pinned to them by
// cmd/sqi-worker's TestExprLimitsBounds_MatchFmtres, the one place that may
// import both.
//
// The ceilings distinguish a catastrophe bound (tight) from a policy bound
// (wide but finite, sized against what a legitimate template needs); each
// [ExprLimits] field says which it is. The three catastrophe ceilings are one
// order of magnitude above their default; the two policy ceilings are set from
// a stated relation.
//
// The floors follow a principle that does not apply on the server: a limit
// tightened too far on a worker rejects work after the job was accepted, once
// per task, naming a budget the submitter never saw. So no floor here is sized
// against the smallest value this repository's presets cost.
// TestExprLimits_PresetCostsLeaveFloorHeadroom fails if any preset comes
// within 4x of a floor.
//
// The floors do not promise "at least the largest value a legitimately
// accepted assignment could need". MinExprOperationLimit and
// MinExprMemoryLimit are sized against the server's defaults, and those two
// are configurable on the server up to 10x their defaults: a server with
// openjd.expr_operation_limit at 100,000 and a worker at its floor gives a
// worker 10x tighter than what the server accepted. The worker does not read
// or receive the server's configuration, so the check runs the other way: the
// worker advertises all five caps at registration, and the server withholds
// EXPR work from a worker that is short in any compared dimension. A floor too
// low for a given server therefore means "this host runs no EXPR jobs", not
// "every task of an accepted job fails here". The floors promise nothing about
// a particular server; each affected field says so.
const (
	// MinExprOperationLimit / MaxExprOperationLimit bound
	// [ExprLimits.OperationLimit].
	//
	// Floor: the server's default per-evaluation operation budget
	// (internal/openjd's defaultSubmissionOperations, 10,000). Below that, a
	// worker would reject at execution an expression the server type-checked
	// at submit under its defaults.
	//
	// That falls short of parity in two ways:
	//
	//  1. It is the server's default, not its configured value, which can be
	//     up to 100,000. A worker at this floor on a farm that raised
	//     openjd.expr_operation_limit is tighter than what the server accepts;
	//     the server detects this from the worker's advertised caps and
	//     withholds EXPR work (internal/scheduler/exprcaps.go).
	//  2. Even at parity acceptance is not guaranteed: phase 3 evaluates the
	//     same expressions against concrete values, which cost more than
	//     phase 1's placeholders. That is why the default here is 100x the
	//     server's.
	//
	// Ceiling: one order of magnitude above the default. A policy bound, but
	// finite: nothing meters wall clock and section 1.3.10 prices 256 bytes at
	// one operation, so 10^7 operations is ~2.5 GB of byte-work in one
	// evaluation, repeated per position.
	MinExprOperationLimit int64 = 10_000
	MaxExprOperationLimit int64 = 10_000_000

	// MinExprMemoryLimit / MaxExprMemoryLimit bound [ExprLimits.MemoryLimit].
	//
	// Floor: the server's default per-evaluation memory budget
	// (internal/openjd's defaultSubmissionMemoryBytes, 1,000,000), for the
	// same reason as the operation floor: a phase-3 evaluation holds the
	// concrete value a phase-2 evaluation had only a placeholder for, so a
	// worker allowance below what the server accepted rejects after the fact.
	// The same gap applies: the server's value is configurable up to
	// 10,000,000, this floor tracks the server's default, and the same
	// runtime gate detects a mismatch (see MinExprOperationLimit).
	//
	// Ceiling: one order of magnitude above the default. A catastrophe
	// ceiling; the number it sets is the per-block structural ceiling
	// maxLetBindings (50) x this value, 1 GB at the default and 10 GB at the
	// maximum, per concurrently resolving task slot. An operator choosing the
	// maximum should size the host's RAM against that product.
	MinExprMemoryLimit int64 = 1_000_000
	MaxExprMemoryLimit int64 = 200_000_000

	// MinExprAssignmentPositions / MaxExprAssignmentPositions bound
	// [ExprLimits.AssignmentPositions].
	//
	// Floor: 2,000, slightly above the 1,841 positions of a generous real session
	// (see [ExprLimits.AssignmentPositions]). A floor sized against this
	// repository's presets would be near 12, low enough to let an operator
	// tighten into per-task failures after acceptance.
	//
	// Ceiling: 100,000, one order of magnitude above the default and equal to
	// internal/openjd's MaxExprTemplatePositions. The worker's cap must be able
	// to reach the highest value an operator can legally give the server, or
	// the relation worker cap >= server cap would be unsatisfiable by
	// configuration. The invariant is >=: this ceiling must never fall below
	// the server's, which TestTemplateBudget_WorkerCapIsNotTighter asserts.
	MinExprAssignmentPositions int64 = 2_000
	MaxExprAssignmentPositions int64 = 100_000

	// MinExprAssignmentRetainedBytes / MaxExprAssignmentRetainedBytes bound
	// [ExprLimits.AssignmentRetainedBytes].
	//
	// Both ends are 2x their LetRetainedBytes counterparts, the ratio the
	// defaults carry (20 MB assignment-wide, 10 MB per table), so the
	// assignment-wide cap is not the tighter of the two merely because an
	// operator moved one end of one range. An operator may still configure it
	// tighter than the per-table cap; the tighter one then applies. That is not
	// rejected at load, because tightening is the safe direction and a
	// cross-field rule would make one knob's legal range depend on another's
	// value.
	MinExprAssignmentRetainedBytes int64 = 2_000_000
	MaxExprAssignmentRetainedBytes int64 = 200_000_000

	// MinExprLetRetainedBytes / MaxExprLetRetainedBytes bound
	// [ExprLimits.LetRetainedBytes].
	//
	// Floor: 1,000,000, one server-default evaluation's worth of live bytes,
	// the largest single value phase 2 can pass through to a binding. A table
	// must be able to hold one of those, and with whole-table accounting
	// (property 2 on the field) the job's parameters are counted alongside
	// it, so a smaller floor would reject accepted work.
	//
	// It is a tenth of the server's default template-wide retained-bytes
	// budget: like the operation and memory floors, it tracks a server
	// default it cannot read. One binding is the least a table can hold; a
	// server accepts a whole template's worth. A worker at this floor is
	// offered no EXPR work, even by a default server, until it is raised or
	// openjd.expr_template_retained_bytes is lowered to meet it. That shows as
	// a registration warning naming both keys and an unschedulable reason on
	// the task, because internal/scheduler compares this dimension too.
	//
	// Ceiling: one order of magnitude above the default, and equal to
	// internal/openjd's MaxExprTemplateRetainedBytes: the server compares this
	// value against openjd.expr_template_retained_bytes, so this ceiling must
	// reach that key's highest legal value or the relation would be
	// unsatisfiable by configuration. A catastrophe bound: the worst case an
	// operator can hand a host is this value plus one in-flight evaluation's
	// MemoryLimit, per table, per concurrent task slot.
	MinExprLetRetainedBytes int64 = 1_000_000
	MaxExprLetRetainedBytes int64 = 100_000_000
)

// DefaultExprLimits returns the default value of each of the five limits, the
// values a worker started with no expr: configuration meters by.
func DefaultExprLimits() ExprLimits {
	return ExprLimits{
		OperationLimit:          defaultWorkerOperationLimit,
		MemoryLimit:             defaultWorkerMemoryLimit,
		AssignmentPositions:     DefaultAssignmentPositions,
		AssignmentRetainedBytes: defaultAssignmentRetainedBytes,
		LetRetainedBytes:        defaultLetRetainedBytes,
	}
}

// Normalized is the exported form of [ExprLimits.orDefaults]. The worker
// advertises its caps to the server at registration (protocol.ExprLimits), and
// what it advertises must be what it enforces: an un-normalized zero would be
// published as "not advertised" while the evaluator used the default.
// internal/worker/registration is the caller.
func (l ExprLimits) Normalized() ExprLimits { return l.orDefaults() }

// orDefaults returns l with every unset (<= 0) field replaced by its default.
//
// It is applied once, in [NewAssignmentBudget], so every consumer reads a
// normalized value. A field is treated as unset rather than "unlimited"
// because the zero value is the common case (every test in this package that
// does not set limits, and [budgetOrDefault]'s throwaway budget for a nil
// argument) and because these numbers must never mean "unlimited".
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
// the session's path-mapping rules; this method assembles the two configured
// numbers in one place.
func (l ExprLimits) evalOptions() []expr.Option {
	return []expr.Option{
		expr.WithOperationLimit(l.OperationLimit),
		expr.WithMemoryLimit(l.MemoryLimit),
	}
}
