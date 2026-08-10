// SPDX-License-Identifier: AGPL-3.0-or-later

package fmtres

// EXPR sub-project E4c's Task 4: the worker's share of the template-wide
// budget design spec §3 introduces server-side (internal/openjd/exprcheck.go's
// templateBudget) -- a DIFFERENT budget, in a DIFFERENT process, for the
// THIRD of the three walks the design spec's own table names: phase 3, this
// package, running on the worker against an assignment's concrete values.
//
// # What already existed, and the gap this closes
//
// EXPR sub-project E4a already bounds ONE symbol table's let: retention
// (defaultLetRetainedBytes, exprsyms.go, 10 MB) and every individual
// evaluation's own operation/memory cost ([ExprLimits.OperationLimit]/
// [ExprLimits.MemoryLimit], expres.go). Neither bounds the ASSIGNMENT as a whole:
// one assignment resolves MANY tables (the task's own TaskSymbols table, plus
// one EnvSymbols table per environment ENTERED in the session -- a template
// may declare many job and step environments) and, within each table, MANY
// positions (a command, every Args entry, every embedded file's data, every
// variable value). Before this task nothing summed any of that: an
// assignment with, say, 20 environments each individually under
// defaultLetRetainedBytes could ALLOCATE up to 20x that in aggregate over the
// course of entering them (see defaultAssignmentRetainedBytes' own comment,
// below, for why that is a claim about cumulative allocation across entry,
// not simultaneously live memory), and an assignment with thousands of cheap
// positions across many actions paid no aggregate cost at all -- both are
// the same class of gap E4c Task 3 closed server-side (a per-position and
// per-table bound that does not sum across the whole walk).
//
// Scoped to ENTRY only, not teardown: fix round 1 (post-implementation
// review, Critical 2) found that charging teardown (session.go's
// resolveEnvAction, reached via ExitEnvironments) against this same budget
// let an assignment-wide cap intended to bound resource use become a
// resource LEAK instead -- an exhausted budget at teardown time made
// ExitEnvironments silently skip an environment's onExit entirely (it treats
// a resolve error as a warning and continues), so license check-ins, daemon
// shutdowns, and unmounts were dropped with only a log line. Teardown is
// therefore exempt from this budget entirely; see resolveEnvAction's own doc
// comment in session.go for the full reasoning.
//
// # Two dimensions, matching the server's split exactly
//
// positions: charged once per format-string FIELD actually resolved through
// the phase-3 evaluator -- a command, one Args entry, one embedded file, one
// variable value (see ResolveActionExpr/ResolveEmbeddedFilesExpr/
// ResolveVarsExpr). Mirrors exprcheck.go's chargePositions exactly, one
// charge per position regardless of what that position's own evaluation
// costs -- [ExprLimits.OperationLimit]/[ExprLimits.MemoryLimit] already bound the latter.
//
// retainedBytes: charged from every let: block a phase-3 table evaluates
// (evalLetBindings, exprsyms.go) -- the ONLY construct in this package that
// RETAINS a value across evaluations, exactly as it is server-side (see
// checkLetBindings' own doc comment for why that asymmetry is what makes a
// per-Eval budget insufficient for exactly this one construct). Every other
// phase-3 evaluation renders its result to text and drops the Value.
//
// # Scope: per assignment, not per table and not process-wide
//
// One [AssignmentBudget] is created per SESSION -- session.Manager.Create
// allocates it once, before any environment is entered, and every table this
// assignment builds (the task's own, and every environment's) shares that
// SAME object, via session.Session's own accessor. This is deliberately
// analogous to design spec §3.1's per-phase scoping server-side (Task 3's
// templateBudget, fresh per checkTemplateExpressions call): a fresh budget
// per assignment is what keeps one task's cost from being charged against
// another's, exactly as phase 1's budget must not be charged against phase
// 2's -- see TestAssignmentBudget_FreshPerAssignment (assignmentbudget_test.go)
// for the mutation-proven property.
//
// It is deliberately NOT process-wide (one global counter shared by every
// concurrently-running session on the worker). Building that correctly needs
// a release step tied to session lifetime (Manager.Cleanup) and would let one
// large but otherwise-compliant assignment starve every OTHER concurrently
// running task's budget on the same host -- a much larger design change than
// this task's brief scopes. Instead, [AssignmentBudget] is intentionally
// small enough (see DefaultAssignmentPositions/defaultAssignmentRetainedBytes, below)
// that CONCURRENCY is accounted for by the PRODUCT of the per-assignment cap
// and the worker's own concurrent-task ceiling, not by a shared counter: "The
// server gates concurrency by CPU cores; the worker runs whatever it is
// leased" (executor.go's own doc comment) -- an operator who has sized a
// worker host's core count has, in the same act, sized how much concurrent
// phase-3 memory that host can carry, because the server never leases more
// simultaneous tasks than the host has cores for. Capping THIS budget at a
// small, fixed multiple of defaultLetRetainedBytes keeps worst-case worker-wide
// phase-3 memory a bounded, PREDICTABLE function of core count -- the same
// resource the operator already provisioned RAM against -- rather than an
// open-ended one. See defaultAssignmentRetainedBytes' own comment for the number.
//
// # Thread safety
//
// [AssignmentBudget] is safe for concurrent use (a mutex-guarded counter
// pair), even though Phase 1 defers session reuse across tasks (session.go's
// own package comment: today exactly one task ever runs in one session). Two
// reasons this still matters now, not merely "for later": session.Session's
// own type contract already promises "safe for concurrent use... multiple
// tasks may execute within a single session simultaneously", and a session's
// environment-teardown path (ExitEnvironments) can run concurrently with -- or
// immediately after -- a still-finishing task action in today's code, both of
// which would charge the SAME AssignmentBudget from different goroutines.
// TestAssignmentBudget_ConcurrentCharges (assignmentbudget_test.go) drives
// concurrent charges under -race and asserts the total is exact -- not merely
// that nothing crashes -- which a plain unsynchronized counter would fail
// under -race even in a single-goroutine-at-a-time test run, because -race
// instruments EVERY memory access, not just ones that happen to race in a
// given run.

import (
	"fmt"
	"sync"
)

const (
	// DefaultAssignmentPositions is the DEFAULT cap on the number of
	// format-string positions
	// (command, one Args entry, one embedded file, one variable value) ONE
	// assignment's phase-3 evaluation may resolve, summed across the task's
	// own table and every environment's.
	//
	// WORKED CALCULATION, generous but plausible for a real session, and
	// CORRECTED by fix round 1 (post-implementation review): an EARLIER
	// version of this comment counted onEnter AND onExit as 21 positions
	// each (42/environment) -- wrong in TWO ways the fix round's other
	// change (Critical 2, session.go's resolveEnvAction) resolved together.
	// First, it undercounted: Variables is resolved at BOTH entry
	// (resolveEnvEntry) and exit (resolveEnvAction) in the pre-fix code, so
	// the real PRE-FIX charge was onEnter(21) + onExit(21) + variables
	// charged TWICE (10 + 10) + files(5) = 67, not 57. Second, and mooting
	// the first: fix round 1's Critical 2 made resolveEnvAction's teardown
	// path NOT charge this budget AT ALL (see that method's own doc comment
	// in session.go for why a budget that cannot avert the memory it is
	// charging for must not be allowed to block cleanup) -- so onExit's
	// command/args and the SECOND (exit-time) variables resolution now
	// contribute ZERO positions, not 21 or 10.
	//
	// The CURRENT, accurate calculation: one task action (command + 30 args
	// + 10 embedded files = 41, unaffected -- a task has no exit/teardown
	// counterpart) plus up to 50 environments entered in one session (no
	// structural cap on environment count exists today -- see
	// internal/openjd's own maxSteps comment for the analogous reasoning it
	// applies to steps), each environment costing ONLY its entry-side work:
	// onEnter (command + 20 args = 21) + 10 variables (entry only) + 5
	// embedded files = 36 positions. 50 x 36 = 1,800, plus the task's own 41,
	// is 1,841.
	//
	// THE VALUE IS NOT SIZED BY THAT CALCULATION ALONE. It is 10,000 because
	// internal/openjd's own template-position default is 10,000, and this cap
	// must never be the TIGHTER of the two -- raised from 5,000 by fix round 2
	// (whole-branch review, IMPORTANT 1), which found the two constants had
	// no stated relation, no test, and no mention of each other.
	//
	// E4d TASK 2 MADE BOTH SIDES CONFIGURABLE, so this constant is now only
	// the DEFAULT: the enforced value is [ExprLimits.AssignmentPositions],
	// carried on the assignment's own budget. It stays exported because
	// internal/openjd's TestTemplateBudget_WorkerCapIsNotTighter is the only
	// place that sees both sides of the relation and still needs to read it --
	// but note that with both sides configurable, that test now compares two
	// DEFAULTS and cannot see a farm whose YAML violates the relation. Closing
	// that is E4d Task 3's work. What Task 2 did do is make the relation
	// SATISFIABLE at every legal setting, by giving this dimension the same
	// ceiling as the server's (see MaxExprAssignmentPositions).
	//
	// WHY THE RELATION IS LOAD-BEARING: the server charges its 10,000-position
	// template-wide budget at validate and submit; this budget is charged on
	// the WORKER, after the job exists. The positions one assignment resolves
	// -- its own step action and embedded files, plus every job and step
	// environment the session enters -- are a SUBSET of the positions the
	// server's walk already charged for the whole template. So a cap here
	// BELOW the server's is reachable by a template the server ACCEPTED: one
	// job environment with 5,000 Variables charged ~5,001 positions
	// server-side (accepted, well under 10,000) and tripped the old 5,000 cap
	// here inside session.Manager.Create -- failing EVERY task in the job,
	// one at a time, after submission, naming a budget the submitter was
	// never shown. Post-submission per-task failure is the worst available
	// place to enforce a template-shape rule; making this cap >= the server's
	// moves that rejection back to the one synchronous request that can
	// report it. Pinned by TestTemplateBudget_WorkerCapIsNotTighter
	// (internal/openjd/exprcheck_budget_test.go), which is where BOTH
	// constants are visible.
	//
	// The alternative -- keeping 5,000 here and having the server charge a
	// per-ASSIGNMENT sub-budget so the rejection happens at submit -- was
	// rejected as the larger change: the server would have to model which of
	// a template's positions land in one assignment (a task's own step plus
	// the environments that task enters), which is a partition of the walk
	// that nothing in internal/openjd computes today.
	//
	// The worked calculation above still stands as the FLOOR: 1,841 for a
	// generous real session, so 10,000 leaves ~5.4x headroom rather than the
	// ~2.7x that 5,000 gave. The wall-clock tradeoff of raising it is the
	// same one internal/openjd's own template-position limit records for the
	// server, on a host that is executing one task rather than serving an
	// API request.
	//
	// ONE ASSUMPTION IS WORTH NAMING, because a future change could void it:
	// "an assignment's positions are a subset of the template's" holds while
	// exactly one task runs per session (see this file's "Scope" section and
	// session.go's own package comment). If session reuse across tasks lands,
	// N tasks would share one budget and the subset argument becomes N x the
	// per-task share -- at which point this cap, and the test pinning it,
	// both need revisiting.
	DefaultAssignmentPositions int64 = 10_000

	// defaultAssignmentRetainedBytes is the DEFAULT cap on the cumulative
	// section 1.3.9 size of
	// every let-bound value EVERY phase-3 table this assignment builds
	// retains, summed across the task's own table and every environment's --
	// the aggregate defaultLetRetainedBytes (exprsyms.go, 10 MB) does not
	// provide on its own, since that bound is per TABLE.
	//
	// 20,000,000 (20 MB) is exactly 2x defaultLetRetainedBytes: two tables'
	// worth (the task's own, plus one environment's -- the common shape for a
	// session that enters a handful of environments, not dozens with heavy
	// let: blocks each) fits without pressure, while a session entering many
	// MORE environments, each near its own 10 MB per-table ceiling, is now
	// caught by THIS budget instead of accumulating unboundedly.
	//
	// THIS COUNTER MEASURES CUMULATIVE ALLOCATION ACROSS ONE SESSION'S
	// ENTRY-TIME EVALUATION, NOT PEAK LIVE RETENTION AT ANY ONE INSTANT --
	// corrected by fix round 1 (post-implementation review), which flagged
	// an earlier revision of this comment for conflating the two. Each
	// environment's phase-3 symbol table (EnvSymbols, built in
	// resolveEnvEntry) is a LOCAL variable: it goes out of scope, and
	// becomes eligible for GC, once that one environment's onEnter/
	// Variables/embedded files finish resolving -- only the RESOLVED TEXT
	// (a command string, a handful of variable values) survives into
	// s.staticEnv and the returned action, not the let-bound VALUES that
	// produced it. So "N environments could retain N x
	// defaultLetRetainedBytes" describes the SUM of everything charged
	// across the session's entry phase, which is what this counter
	// enforces -- it is NOT a claim that N tables' worth of bytes are ever
	// simultaneously live in the process. This mirrors exprcheck.go's own
	// template-retained-bytes counter, which states the identical distinction
	// for the server-side counter it is deliberately kept equal to in
	// magnitude reasoning (see below): "measures CUMULATIVE ALLOCATION
	// across the walk, not PEAK LIVE RETENTION at any one instant."
	//
	// THE CONCURRENCY ARGUMENT this number is chosen against, restated with
	// that distinction now explicit (see this file's own package-level doc
	// comment for the full account): worst-case worker-wide CUMULATIVE
	// ALLOCATION THROUGHPUT from this mechanism, summed across every
	// concurrently-creating session, is bounded by (concurrent session
	// count) x defaultAssignmentRetainedBytes, and concurrent session count is
	// itself bounded by the host's CPU core count (the server never leases
	// more simultaneous work than that). A 64-core worker's worst case is
	// therefore ~64 x 20 MB = 1.28 GB of allocation activity across
	// concurrently-creating sessions, NOT 1.28 GB of simultaneously live
	// heap -- an operator reading this constant should size against
	// allocation/GC pressure proportional to core count, not against a
	// claimed live-memory footprint, which this mechanism does not measure
	// and did not, before this task, bound at all (the gap this task
	// closes is the unbounded per-session ACCUMULATION, not a claim about
	// instantaneous RAM). This is a real, standing tradeoff, not a proof of
	// a tight global ceiling -- a genuine process-wide accounting is future
	// work (E4d, operator configuration, is the natural place for a
	// process-wide knob alongside a per-assignment one), recorded here
	// rather than silently accepted as solved.
	defaultAssignmentRetainedBytes int64 = 20_000_000
)

// AssignmentBudget bounds the CUMULATIVE section 1.3.9/1.3.10 cost phase-3
// evaluation may charge against ONE assignment -- see this file's own
// package-level doc comment for the full account of what it bounds, why it
// is scoped per-assignment rather than per-table or process-wide, and how
// concurrency figures into the constants above.
//
// Use [NewAssignmentBudget] to create one; every phase-3 entry point in this
// package (ApplyTaskLet, ApplyEnvLet, ResolveActionExpr,
// ResolveEmbeddedFilesExpr, ResolveVarsExpr) accepts one as an optional
// trailing argument -- omitting it (every call site that existed before this
// task) gives that ONE call its own fresh, throwaway allowance, exactly as
// [templateBudgetOrFresh] does server-side; see [assignmentBudgetOrFresh].
type AssignmentBudget struct {
	// limits is the operator's configuration for this assignment, already
	// normalized (orDefaults) exactly once, in NewAssignmentBudget. It carries
	// all FIVE numbers, not only the two this type counts: every phase-3 entry
	// point in this package already takes a budget, so making it the single
	// carrier is what guarantees one assignment's evaluations are all metered
	// by the same set of numbers. See exprlimits.go.
	limits    ExprLimits
	mu        sync.Mutex
	positions int64
	retained  int64
	err       error
}

// NewAssignmentBudget returns a fresh, unspent [AssignmentBudget] metered by
// lim. A zero ExprLimits (or any individually unset field) normalizes to the
// defaults -- see [ExprLimits.orDefaults]; it never means "unlimited".
//
// The parameter is deliberately REQUIRED rather than an option: this
// constructor is where an assignment acquires its limits, and a caller that
// has operator configuration to supply but silently does not is the exact
// failure E4d exists to prevent. Callers with nothing to say pass
// ExprLimits{}.
func NewAssignmentBudget(lim ExprLimits) *AssignmentBudget {
	return &AssignmentBudget{limits: lim.orDefaults()}
}

// Limits returns the normalized [ExprLimits] this budget meters against.
//
// It is how the per-evaluation and per-table numbers reach the evaluator: the
// resolvers build their expr.Option slice from it (ExprEvalOptions), and
// evalLetBindings reads LetRetainedBytes from it. Read-only, and safe without
// the mutex -- limits is written once by the constructor and never again.
func (b *AssignmentBudget) Limits() ExprLimits { return b.limits }

// budgetOrDefault returns b, or a brand-new default-limited [AssignmentBudget]
// when b is nil.
//
// EVERY phase-3 entry point in this package takes its budget as a REQUIRED
// parameter (E4d Task 2, fix round 1). It used to be a variadic tail, and that
// shape is exactly what let executor.resolveAssignmentExpr meter every PATH
// parameter against the compiled-in defaults for one commit: omitting the
// argument compiled, ran, and produced no failure anywhere. A required
// parameter cannot be omitted by accident; nil is a deliberate word a reviewer
// can see.
//
// nil is still ACCEPTED, and means "the built-in defaults" -- it is what this
// package's own unit tests pass when the limits are not what they are testing,
// and a nil budget must never reach ChargePositions/ChargeRetainedBytes, which
// both dereference the receiver. It is NOT an acceptable thing for a
// PRODUCTION call site to pass, and that is enforced rather than requested:
// TestPhase3EntryPoints_ProductionCallersAlwaysPassABudget
// (budgetguard_test.go) parses internal/worker/session and
// internal/worker/executor and fails on any non-test call that passes a
// literal nil.
func budgetOrDefault(b *AssignmentBudget) *AssignmentBudget {
	if b != nil {
		return b
	}
	return NewAssignmentBudget(ExprLimits{})
}

// ChargePositions charges n additional format-string positions, naming where
// as the position that exhausted the budget if this call is the one that
// does. It returns a non-nil error -- the SAME error on every call once the
// budget is spent, either dimension -- when the caller may not do the work
// those positions represent.
func (b *AssignmentBudget) ChargePositions(n int64, where string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	if n <= 0 {
		return nil
	}
	b.positions += n
	if b.positions > b.limits.AssignmentPositions {
		b.err = fmt.Errorf(
			"assignment-wide expression budget exceeded: at most %d expression positions may be "+
				"resolved for one assignment (reached %d at %s)",
			b.limits.AssignmentPositions, b.positions, where,
		)
	}
	return b.err
}

// ChargeRetainedBytes charges n additional retained bytes -- one let:
// binding's own [expr.SizeOf] -- naming where as the position that exhausted
// the budget if this call is the one that does. n <= 0 is a no-op that still
// reports the budget's current state, matching [ChargePositions]'s own
// contract.
func (b *AssignmentBudget) ChargeRetainedBytes(n int64, where string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	if n <= 0 {
		return nil
	}
	b.retained += n
	if b.retained > b.limits.AssignmentRetainedBytes {
		b.err = fmt.Errorf(
			"assignment-wide expression budget exceeded: let bindings may retain at most %d bytes "+
				"across one assignment (reached %d at %s)",
			b.limits.AssignmentRetainedBytes, b.retained, where,
		)
	}
	return b.err
}

// Err returns the ONE error recording why the budget tripped, or nil if it
// never did.
func (b *AssignmentBudget) Err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}
