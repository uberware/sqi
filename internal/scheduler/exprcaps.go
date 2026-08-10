// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// exprcaps.go closes the one invariant that spans the two binaries: a worker's
// OpenJD EXPR evaluation caps must not be TIGHTER than the server's, or a job
// the server accepts fails on the worker — after submission, once per task,
// naming a budget the submitter never saw.
//
// THE MEASURED FAILURE (EXPR design spec §2). With the server at 10,000
// expression positions and the worker at 5,000, a job with one 5,000-variable
// environment was accepted, created and persisted, and then every task in it
// failed at runtime. EXPR sub-project E4c fixed it by relating the two
// CONSTANTS (internal/openjd's TestTemplateBudget_WorkerCapIsNotTighter, which
// is the only place that can see both packages). E4d Tasks 1 and 2 turned both
// constants into operator configuration, and a test that compares constants
// cannot see a YAML file.
//
// THE MECHANISM. The worker advertises the caps it will enforce in its
// registration message; the server persists them on the worker record
// ([store.WorkerExprLimits]); and the scheduler REFUSES TO DISPATCH an EXPR job
// to a worker whose advertised caps are below this server's configured limits.
// The refusal is the bound. The reason string it produces is also what the
// unschedulable sweep writes onto the task, so an operator sees the cause on
// the job rather than having to correlate two config files.
//
// WHY REFUSING TO DISPATCH RATHER THAN REFUSING TO SUBMIT. Rejecting the
// template at submit would need the server to know every worker's caps at
// submission time, making acceptance depend on which hosts happen to be online
// and defeating the per-worker sizing that made these knobs per-worker in the
// first place (design spec §1). Refusing to dispatch keeps a heterogeneous farm
// working: a capable worker still runs the job.
//
// WHY IT NARROWS TO EXPR JOBS. The caps bound nothing but EXPR phase-3
// evaluation. Taking a whole host out of the farm over them would be a far
// larger outage than the one being prevented, and a small host tightening its
// limits is exactly the use this configuration exists for.
//
// WHAT IT CANNOT DO. worker >= server is NECESSARY, not sufficient: phase 3
// evaluates concrete values where phase 2 had placeholders, so the same
// expression can legitimately cost more on the worker than it did at submit
// (that is why the worker's shipped defaults are 100x the server's operation
// budget and 20x its memory budget, not equal to them). This gate closes the
// half an operator can misconfigure. It cannot close the half that comes from
// the values themselves.

import (
	"fmt"
	"strings"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
)

// legacyWorkerExprCaps is what a worker that advertises nothing is assumed to
// enforce: the caps compiled into every sqi-worker built before EXPR
// sub-project E4d Task 2 made them configurable.
//
// This is knowledge, not a guess. Only a pre-Task-2 binary can send zeroes — a
// current worker's configuration layer rejects 0 as out of range, and
// registration normalizes before publishing — and a pre-Task-2 binary enforces
// exactly these numbers. Task 2's TestExprLimits_DefaultsMatchPreE4dConstants
// pins that fmtres.DefaultExprLimits() still equals them, and this package's
// TestExprCaps_UnadvertisedCapsAreThePreE4dDefaults pins this copy against
// fmtres so the two cannot drift.
//
// They are duplicated rather than imported because internal/scheduler is server
// code: it has no business linking the worker's evaluator into the server
// binary for four integers.
var legacyWorkerExprCaps = store.WorkerExprLimits{
	OperationLimit:          1_000_000,
	MemoryLimit:             20_000_000,
	AssignmentPositions:     10_000,
	AssignmentRetainedBytes: 20_000_000,
}

// workerExprCapsOrLegacy replaces every unadvertised (<= 0) cap with what such
// a worker actually enforces. Treating an absent value as 0 would report a
// shortfall in every dimension for every legacy worker; treating it as
// "unlimited" would fail open in exactly the case this file exists for.
func workerExprCapsOrLegacy(c store.WorkerExprLimits) store.WorkerExprLimits {
	if c.OperationLimit <= 0 {
		c.OperationLimit = legacyWorkerExprCaps.OperationLimit
	}
	if c.MemoryLimit <= 0 {
		c.MemoryLimit = legacyWorkerExprCaps.MemoryLimit
	}
	if c.AssignmentPositions <= 0 {
		c.AssignmentPositions = legacyWorkerExprCaps.AssignmentPositions
	}
	if c.AssignmentRetainedBytes <= 0 {
		c.AssignmentRetainedBytes = legacyWorkerExprCaps.AssignmentRetainedBytes
	}
	return c
}

// exprCapShortfall returns "" when caps can run everything srv accepts, or an
// operator-facing description of every dimension where they cannot.
//
// srv must already be normalized ([openjd.ExprLimits.Normalized]); the
// scheduler normalizes once, in [New].
//
// The comparison is >=, so a worker configured to exactly the server's value
// passes: the relation is "not tighter", not "strictly wider".
//
// Each dimension pairs a server-side budget with the worker-side budget that
// meters the SAME quantity one phase later:
//
//	openjd.expr_template_positions       <= expr.assignment_positions
//	openjd.expr_operation_limit          <= expr.operation_limit
//	openjd.expr_memory_limit             <= expr.memory_limit
//	openjd.expr_template_retained_bytes  <= expr.assignment_retained_bytes
//
// The first is a true subset relation: an assignment resolves a subset of the
// template's positions, and a position is a position on both sides. The other
// three relate budgets over the same quantity at different scopes (one
// evaluation, one assignment, one template walk), where the worker's value is
// additionally inflated by concrete data — so they are necessary conditions,
// not guarantees. See the file header.
//
// expr.let_retained_bytes has no server counterpart and is deliberately not
// compared: it bounds ONE symbol table, a scope the server's walk does not
// meter separately.
func exprCapShortfall(caps store.WorkerExprLimits, srv openjd.ExprLimits) string {
	c := workerExprCapsOrLegacy(caps)

	var short []string
	if c.AssignmentPositions < srv.TemplatePositions {
		short = append(short, fmt.Sprintf(
			"resolves at most %d expression positions per assignment but this server accepts "+
				"templates costing up to %d (expr.assignment_positions vs openjd.expr_template_positions)",
			c.AssignmentPositions, srv.TemplatePositions,
		))
	}
	if c.OperationLimit < srv.SubmissionOperations {
		short = append(short, fmt.Sprintf(
			"allows %d operations per expression evaluation but this server accepts expressions "+
				"costing up to %d (expr.operation_limit vs openjd.expr_operation_limit)",
			c.OperationLimit, srv.SubmissionOperations,
		))
	}
	if c.MemoryLimit < srv.SubmissionMemoryBytes {
		short = append(short, fmt.Sprintf(
			"allows %d live bytes per expression evaluation but this server accepts expressions "+
				"holding up to %d (expr.memory_limit vs openjd.expr_memory_limit)",
			c.MemoryLimit, srv.SubmissionMemoryBytes,
		))
	}
	if c.AssignmentRetainedBytes < srv.TemplateRetainedBytes {
		short = append(short, fmt.Sprintf(
			"lets let: bindings retain %d bytes per assignment but this server accepts templates "+
				"retaining up to %d (expr.assignment_retained_bytes vs "+
				"openjd.expr_template_retained_bytes)",
			c.AssignmentRetainedBytes, srv.TemplateRetainedBytes,
		))
	}
	if len(short) == 0 {
		return ""
	}
	return "worker EXPR limits are tighter than this server's: it " + strings.Join(short, "; ") +
		". A job accepted under the server's limits would fail on this worker once per task, " +
		"so it is not offered EXPR work"
}

// jobMayUseEXPR reports whether job's template could produce an assignment
// requiring worker-side expression evaluation.
//
// Deliberately a SUPERSET, and deliberately not a parse. An extension is
// declared by its literal name in the template document, so a template whose
// bytes do not contain "EXPR" cannot declare it; a template that mentions it
// anywhere else (a comment, an environment variable, a command argument) is
// treated as EXPR-capable, which only makes the gate stricter. Re-parsing every
// candidate template on every lease request to be exact would cost far more
// than the mistake is worth: this runs per ready task per lease request, and
// buildAssignPayload is where a template is parsed, on the winner only.
func jobMayUseEXPR(job store.Job) bool {
	return strings.Contains(job.RawTemplate, openjd.ExtensionEXPR)
}

// exprCapsBlock returns "" when worker may be given job, or the operator-facing
// reason it may not.
//
// The dimension comparison runs FIRST because it is four integer comparisons on
// a value already in hand, and on a correctly-configured farm it returns ""
// immediately — the template scan only ever happens on a farm that is already
// misconfigured.
//
// Both callers matter and neither may be dropped: [Scheduler.leaseGatesPass] is
// the bound (the task is never handed over), and [Scheduler.evaluateSchedulability]
// is what the operator sees (the same reason, written onto the task by the
// unschedulable sweep). A bound with no explanation is a silent stall; an
// explanation with no bound is the warning-nobody-reads this program has
// repeatedly found is not a bound at all.
func (s *Scheduler) exprCapsBlock(worker store.Worker, job store.Job) string {
	reason := exprCapShortfall(worker.ExprLimits, s.cfg.ExprLimits)
	if reason == "" || !jobMayUseEXPR(job) {
		return ""
	}
	return reason
}
