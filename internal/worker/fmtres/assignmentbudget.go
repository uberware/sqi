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
// (workerLetRetainedLimit, exprsyms.go, 10 MB) and every individual
// evaluation's own operation/memory cost (workerOperationLimit/
// workerMemoryLimit, expres.go). Neither bounds the ASSIGNMENT as a whole:
// one assignment resolves MANY tables (the task's own TaskSymbols table, plus
// one EnvSymbols table per environment entered in the session -- a template
// may declare many job and step environments) and, within each table, MANY
// positions (a command, every Args entry, every embedded file's data, every
// variable value). Before this task nothing summed any of that: an
// assignment with, say, 20 environments each individually under
// workerLetRetainedLimit could retain up to 20x that in aggregate, and an
// assignment with thousands of cheap positions across many actions paid no
// aggregate cost at all -- both are the same class of gap E4c Task 3 closed
// server-side (a per-position and per-table bound that does not sum across
// the whole walk).
//
// # Two dimensions, matching the server's split exactly
//
// positions: charged once per format-string FIELD actually resolved through
// the phase-3 evaluator -- a command, one Args entry, one embedded file, one
// variable value (see ResolveActionExpr/ResolveEmbeddedFilesExpr/
// ResolveVarsExpr). Mirrors exprcheck.go's chargePositions exactly, one
// charge per position regardless of what that position's own evaluation
// costs -- workerOperationLimit/workerMemoryLimit already bound the latter.
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
// small enough (see assignmentMaxPositions/assignmentMaxRetainedBytes, below)
// that CONCURRENCY is accounted for by the PRODUCT of the per-assignment cap
// and the worker's own concurrent-task ceiling, not by a shared counter: "The
// server gates concurrency by CPU cores; the worker runs whatever it is
// leased" (executor.go's own doc comment) -- an operator who has sized a
// worker host's core count has, in the same act, sized how much concurrent
// phase-3 memory that host can carry, because the server never leases more
// simultaneous tasks than the host has cores for. Capping THIS budget at a
// small, fixed multiple of workerLetRetainedLimit keeps worst-case worker-wide
// phase-3 memory a bounded, PREDICTABLE function of core count -- the same
// resource the operator already provisioned RAM against -- rather than an
// open-ended one. See assignmentMaxRetainedBytes' own comment for the number.
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
	// assignmentMaxPositions caps the number of format-string positions
	// (command, one Args entry, one embedded file, one variable value) ONE
	// assignment's phase-3 evaluation may resolve, summed across the task's
	// own table and every environment's.
	//
	// WORKED CALCULATION, generous but plausible for a real session: one task
	// action (command + 30 args + 10 embedded files = 41) plus up to 50
	// environments entered in one session (no structural cap on environment
	// count exists today -- see internal/openjd's own maxSteps comment for
	// the analogous reasoning it applies to steps), each environment costing
	// onEnter (command + 20 args = 21) + onExit (21) + 10 variables + 5
	// embedded files = 57 positions. 50 x 57 = 2,850, plus the task's own 41,
	// is 2,891. 5,000 gives that shape ~1.7x headroom while staying two
	// orders of magnitude below a pathological construction (tens of
	// thousands of cheap positions) that would cost real wall-clock time to
	// resolve one at a time even under workerOperationLimit/
	// workerMemoryLimit's own per-position bound.
	assignmentMaxPositions int64 = 5_000

	// assignmentMaxRetainedBytes caps the cumulative section 1.3.9 size of
	// every let-bound value EVERY phase-3 table this assignment builds
	// retains, summed across the task's own table and every environment's --
	// the aggregate workerLetRetainedLimit (exprsyms.go, 10 MB) does not
	// provide on its own, since that bound is per TABLE.
	//
	// 20,000,000 (20 MB) is exactly 2x workerLetRetainedLimit: two tables'
	// worth (the task's own, plus one environment's -- the common shape for a
	// session that enters a handful of environments, not dozens with heavy
	// let: blocks each) fits without pressure, while a session entering many
	// MORE environments, each near its own 10 MB per-table ceiling, is now
	// caught by THIS budget instead of accumulating unboundedly.
	//
	// THE CONCURRENCY ARGUMENT this number is chosen against, stated
	// plainly (see this file's own package-level doc comment for the full
	// account): worst-case worker-wide phase-3 memory is bounded by
	// (concurrent task/session count) x assignmentMaxRetainedBytes, and
	// concurrent task count is itself bounded by the host's CPU core count
	// (the server never leases more simultaneous work than that). A 64-core
	// worker's worst case is therefore ~64 x 20 MB = 1.28 GB -- large, but
	// BOUNDED and PROPORTIONAL to a resource the operator already sized RAM
	// against, not the unbounded-per-session accumulation that existed
	// before this task. This is a real, standing tradeoff, not a proof of a
	// tight global ceiling -- a genuine process-wide accounting is future
	// work (E4d, operator configuration, is the natural place for a
	// process-wide knob alongside a per-assignment one), recorded here rather
	// than silently accepted as solved.
	assignmentMaxRetainedBytes int64 = 20_000_000
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
	mu        sync.Mutex
	positions int64
	retained  int64
	err       error
}

// NewAssignmentBudget returns a fresh, unspent [AssignmentBudget].
func NewAssignmentBudget() *AssignmentBudget {
	return &AssignmentBudget{}
}

// assignmentBudgetOrFresh returns budget[0] when the caller supplied one, or
// a brand-new [AssignmentBudget] otherwise -- see [AssignmentBudget]'s own
// doc comment for why this is the mechanism that keeps every pre-Task-4 call
// site in this package compiling and behaving unchanged. budget[0] == nil is
// treated the same as "no budget supplied", since a nil *AssignmentBudget
// must never reach ChargePositions/ChargeRetainedBytes -- both dereference
// the receiver.
func assignmentBudgetOrFresh(budget []*AssignmentBudget) *AssignmentBudget {
	if len(budget) > 0 && budget[0] != nil {
		return budget[0]
	}
	return NewAssignmentBudget()
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
	if b.positions > assignmentMaxPositions {
		b.err = fmt.Errorf(
			"assignment-wide expression budget exceeded: at most %d expression positions may be "+
				"resolved for one assignment (reached %d at %s)",
			assignmentMaxPositions, b.positions, where,
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
	if b.retained > assignmentMaxRetainedBytes {
		b.err = fmt.Errorf(
			"assignment-wide expression budget exceeded: let bindings may retain at most %d bytes "+
				"across one assignment (reached %d at %s)",
			assignmentMaxRetainedBytes, b.retained, where,
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
