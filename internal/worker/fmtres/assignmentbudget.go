// SPDX-License-Identifier: AGPL-3.0-or-later

package fmtres

// The assignment-wide expression budget for phase-3 evaluation: this package,
// running on the worker against an assignment's concrete values. It is the
// worker's counterpart of the server's template-wide budget
// (internal/openjd/exprcheck.go's templateBudget), enforced separately in a
// different process.
//
// # What it bounds
//
// defaultLetRetainedBytes (exprsyms.go, 10 MB) bounds one symbol table's let:
// retention, and [ExprLimits.OperationLimit]/[ExprLimits.MemoryLimit]
// (expres.go) bound each individual evaluation. Neither bounds the assignment
// as a whole: one assignment resolves many tables (the task's own TaskSymbols
// table plus one EnvSymbols table per environment entered in the session) and,
// within each table, many positions (a command, every Args entry, every
// embedded file's data, every variable value). This budget sums both across
// the assignment, as the server's template-wide budget does across a template.
//
// Only environment entry is charged. Teardown (session.go's resolveEnvAction,
// reached via ExitEnvironments) gets a fresh budget per evaluation: an
// exhausted budget at teardown would make ExitEnvironments skip an
// environment's onExit (it treats a resolve error as a warning and continues),
// dropping license check-ins, daemon shutdowns and unmounts. See
// resolveEnvAction's doc comment in session.go.
//
// # Two dimensions, matching the server's split
//
// positions: charged once per format-string field resolved through the
// phase-3 evaluator: a command, one Args entry, one embedded file, one
// variable value (see ResolveActionExpr/ResolveEmbeddedFilesExpr/
// ResolveVarsExpr). As in exprcheck.go's chargePositions, each position costs
// one charge regardless of what its evaluation costs;
// [ExprLimits.OperationLimit]/[ExprLimits.MemoryLimit] bound the latter.
//
// retainedBytes: charged from every let: block a phase-3 table evaluates
// (evalLetBindings, exprsyms.go), the only construct in this package that
// retains a value across evaluations, as on the server (see
// checkLetBindings' doc comment). Every other phase-3 evaluation renders its
// result to text and drops the Value.
//
// # Scope: per assignment, not per table and not process-wide
//
// One [AssignmentBudget] is created per session: session.Manager.Create
// allocates it before any environment is entered, and every table the
// assignment builds (the task's own and every environment's) shares it through
// session.Session's accessor. This matches the server's per-phase scoping
// (templateBudget, fresh per checkTemplateExpressions call): a fresh budget per
// assignment keeps one task's cost from being charged against another's
// (TestAssignmentBudget_FreshPerAssignment).
//
// It is not process-wide. A counter shared by every concurrently running
// session would need a release step tied to session lifetime
// (Manager.Cleanup) and would let one large but compliant assignment starve
// every other task on the host. Instead the per-assignment cap is small enough
// (see DefaultAssignmentPositions and defaultAssignmentRetainedBytes) that
// concurrency is accounted for by the product of that cap and the number of
// tasks the host runs at once. The server gates concurrency by CPU cores and
// never leases more simultaneous tasks than the host has cores for, so
// worst-case worker-wide phase-3 allocation is a bounded function of core
// count, the resource the operator already provisioned RAM against.
//
// # Thread safety
//
// [AssignmentBudget] is safe for concurrent use (a mutex-guarded counter
// pair). Sessions are not reused across tasks (session.go's package comment:
// one task runs in one session), but session.Session's type contract promises
// concurrent use, and a session's environment-teardown path
// (ExitEnvironments) can run concurrently with a still-finishing task action.
// TestAssignmentBudget_ConcurrentCharges drives concurrent charges under -race
// and asserts the total is exact.

import (
	"fmt"
	"sync"
)

const (
	// DefaultAssignmentPositions is the default cap on the number of
	// format-string positions (command, one Args entry, one embedded file, one
	// variable value) one assignment's phase-3 evaluation may resolve, summed
	// across the task's own table and every environment's.
	//
	// It is 10,000 because internal/openjd's template-position default is
	// 10,000, and this cap must never be the tighter of the two. The positions
	// one assignment resolves (its own step action and embedded files, plus
	// every job and step environment the session enters) are a subset of the
	// positions the server already charged for the whole template at validate
	// and submit. A cap here below the server's is reachable by a template the
	// server accepted: one job environment with 5,000 Variables charges ~5,001
	// positions server-side and would trip a 5,000 cap here inside
	// session.Manager.Create, failing every task in the job after submission
	// with a budget the submitter never saw. Keeping this cap >= the server's
	// leaves the rejection to the submit request, where it can be reported.
	//
	// The enforced value is [ExprLimits.AssignmentPositions], carried on the
	// assignment's budget; this constant is the default. It is exported for
	// internal/openjd's TestTemplateBudget_WorkerCapIsNotTighter
	// (exprcheck_budget_test.go), which compares the two defaults and cannot
	// see a farm whose configuration violates the relation. internal/scheduler
	// enforces the relation at runtime: the configured value is advertised to
	// the server at registration, and an EXPR job is not dispatched to a
	// worker whose value is below the server's (that gate identifies an EXPR
	// job by a heuristic; see internal/scheduler/exprcaps.go's jobMayUseEXPR).
	// The relation is satisfiable at every legal setting because this
	// dimension has the same ceiling as the server's (see
	// MaxExprAssignmentPositions).
	//
	// For scale, a generous real session needs about 1,841 positions: one task
	// action (command + 30 args + 10 embedded files = 41) plus 50 environments,
	// each charging only its entry-side work (onEnter command + 20 args = 21,
	// 10 variables, 5 embedded files = 36). Teardown charges nothing.
	//
	// The subset relation holds while exactly one task runs per session (see
	// the "Scope" section above and session.go's package comment). If sessions
	// were reused across tasks, N tasks would share one budget and this cap,
	// with the test pinning it, would need revisiting.
	DefaultAssignmentPositions int64 = 10_000

	// defaultAssignmentRetainedBytes is the default cap on the cumulative
	// section 1.3.9 size of every let-bound value retained by every phase-3
	// table this assignment builds, summed across the task's own table and
	// every environment's. defaultLetRetainedBytes (exprsyms.go, 10 MB) is per
	// table and does not provide this aggregate.
	//
	// 20,000,000 (20 MB) is 2x defaultLetRetainedBytes: two tables' worth (the
	// task's own plus one environment's) fits, while a session entering many
	// environments, each near its own 10 MB per-table ceiling, is stopped by
	// this budget.
	//
	// It measures cumulative allocation across one session's entry-time
	// evaluation, not peak live retention. Each environment's phase-3 symbol
	// table (EnvSymbols, built in resolveEnvEntry) is a local variable that
	// becomes eligible for GC once that environment's onEnter, Variables and
	// embedded files are resolved; only the resolved text survives into
	// s.staticEnv and the returned action. exprcheck.go's template
	// retained-bytes counter makes the same distinction on the server.
	//
	// Worst-case worker-wide cumulative allocation from this mechanism is
	// (concurrent session count) x defaultAssignmentRetainedBytes, and
	// concurrent session count is bounded by the host's CPU core count. A
	// 64-core worker's worst case is ~64 x 20 MB = 1.28 GB of allocation
	// across concurrently creating sessions, not 1.28 GB of live heap: size
	// for allocation and GC pressure proportional to core count. There is no
	// process-wide accounting, so this is not a tight global ceiling.
	defaultAssignmentRetainedBytes int64 = 20_000_000
)

// AssignmentBudget bounds the cumulative section 1.3.9/1.3.10 cost phase-3
// evaluation may charge against one assignment. See the comment at the top of
// this file for what it bounds, why it is scoped per assignment, and how
// concurrency figures into the constants above.
//
// Use [NewAssignmentBudget] to create one; every phase-3 entry point in this
// package (TaskSymbols, EnvSymbols, ApplyTaskLet, ApplyEnvLet,
// ResolveActionExpr, ResolveEmbeddedFilesExpr, ResolveVarsExpr) takes one as a
// required trailing parameter. nil means "the built-in default limits, on a
// fresh throwaway ledger"; this package's unit tests pass it when the limits
// are not under test, and production call sites must not. See
// [budgetOrDefault] for both halves of that contract and the guard that
// enforces the second.
type AssignmentBudget struct {
	// limits is the operator's configuration for this assignment, normalized
	// (orDefaults) once, in NewAssignmentBudget. It carries all five numbers,
	// not only the two this type counts: every phase-3 entry point already
	// takes a budget, so making it the single carrier guarantees one
	// assignment's evaluations are all metered by the same numbers. See
	// exprlimits.go.
	limits    ExprLimits
	mu        sync.Mutex
	positions int64
	retained  int64
	err       error
}

// NewAssignmentBudget returns a fresh, unspent [AssignmentBudget] metered by
// lim. A zero ExprLimits (or any individually unset field) normalizes to the
// defaults (see [ExprLimits.orDefaults]); it never means "unlimited".
//
// The parameter is required rather than an option: this constructor is where
// an assignment acquires its limits, and a caller that forgot to supply its
// operator configuration would meter by the defaults instead. Callers with
// nothing to supply pass ExprLimits{}.
func NewAssignmentBudget(lim ExprLimits) *AssignmentBudget {
	return &AssignmentBudget{limits: lim.orDefaults()}
}

// Limits returns the normalized [ExprLimits] this budget meters against.
//
// The resolvers build their expr.Option slice from it (ExprEvalOptions), and
// evalLetBindings reads LetRetainedBytes from it. It needs no mutex: limits is
// written once by the constructor.
func (b *AssignmentBudget) Limits() ExprLimits { return b.limits }

// budgetOrDefault returns b, or a new default-limited [AssignmentBudget] when
// b is nil.
//
// Every phase-3 entry point in this package takes its budget as a required
// parameter rather than a variadic tail: a caller omitting a variadic argument
// would compile and meter against the compiled-in defaults with no failure
// anywhere. With a required parameter, nil is a visible choice.
//
// nil means "the built-in defaults". This package's unit tests pass it when
// the limits are not under test; a nil budget must never reach
// ChargePositions/ChargeRetainedBytes, which dereference the receiver.
// Production call sites must not pass nil:
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

// The two exceeded-message templates [AssignmentBudget.charge] formats, one per
// dimension. Each takes the limit, the count reached, and the position that
// reached it, in that order.
const (
	positionsExceededFormat = "assignment-wide expression budget exceeded: at most %d expression positions may be " +
		"resolved for one assignment (reached %d at %s)"
	retainedBytesExceededFormat = "assignment-wide expression budget exceeded: let bindings may retain at most %d " +
		"bytes across one assignment (reached %d at %s)"
)

// charge is the shared body of [AssignmentBudget.ChargePositions] and
// [AssignmentBudget.ChargeRetainedBytes], which differ only in the counter,
// the limit and the message.
//
// The error is sticky: b.err is set once, by whichever charge in either
// dimension first exceeds its limit, and every later charge in either
// dimension returns that same error without counting. On an untripped budget,
// n <= 0 is a no-op returning nil.
//
// counter must point into b, and the caller must not hold b.mu.
func (b *AssignmentBudget) charge(counter *int64, limit, n int64, format, where string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	if n <= 0 {
		return nil
	}
	*counter += n
	if *counter > limit {
		b.err = fmt.Errorf(format, limit, *counter, where)
	}
	return b.err
}

// ChargePositions charges n additional format-string positions, naming where
// as the position that exhausted the budget if this call is the one that
// does. It returns a non-nil error (the same error on every call once the
// budget is spent, in either dimension) when the caller may not do the work
// those positions represent.
func (b *AssignmentBudget) ChargePositions(n int64, where string) error {
	return b.charge(&b.positions, b.limits.AssignmentPositions, n, positionsExceededFormat, where)
}

// ChargeRetainedBytes charges n additional retained bytes (one let: binding's
// [expr.SizeOf]), naming where as the position that exhausted the budget if
// this call is the one that does. n <= 0 is a no-op that still reports the
// budget's current state, as in [ChargePositions].
func (b *AssignmentBudget) ChargeRetainedBytes(n int64, where string) error {
	return b.charge(&b.retained, b.limits.AssignmentRetainedBytes, n, retainedBytesExceededFormat, where)
}

// Err returns the error recording why the budget tripped, or nil if it never
// did.
func (b *AssignmentBudget) Err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}
