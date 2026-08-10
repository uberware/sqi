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
// CONSTANTS (internal/openjd's TestTemplateBudget_WorkerCapIsNotTighter, a
// TEST-ONLY import of internal/worker/fmtres from a package no production file
// of which may import it -- this package's tests now do the same). E4d Tasks 1
// and 2 turned both constants into operator configuration, and a test that
// compares constants cannot see a YAML file.
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
// sub-project E4d Task 3 taught it to advertise them.
//
// IT IS KNOWLEDGE FOR ONE OF THE TWO BINARIES THAT CAN SEND ZEROES, AND A GUESS
// FOR THE OTHER. Any worker built from Task 3 onward always reports real values
// (its configuration layer rejects 0 as out of range and registration
// normalizes before publishing), so silence means an older binary — of which
// there are two kinds:
//
//   - Built before Task 2: the limits were fixed constants equal to these
//     numbers, so the assumption is exact.
//   - Built between Task 2 and Task 3 (this branch only, never released): the
//     limits were already configurable, so such a worker may be enforcing
//     something TIGHTER than this while reporting nothing, and the gate would
//     pass it work it cannot run.
//
// The second case is accepted because it cannot exist outside this unreleased
// branch, and because the alternatives are worse: reading silence as 0 refuses
// every pre-Task-3 worker in the farm, and reading it as unlimited fails open
// in exactly the case this file exists for.
//
// Task 2's TestExprLimits_DefaultsMatchPreE4dConstants pins that
// fmtres.DefaultExprLimits() still equals these numbers, and this package's
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
// a worker is assumed to enforce ([legacyWorkerExprCaps], which records where
// that assumption is exact and where it is not). Treating an absent value as 0
// would report a shortfall in every dimension for every pre-Task-3 worker;
// treating it as "unlimited" would fail open in exactly the case this file
// exists for.
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

// jobMayUseEXPR is a HEURISTIC, not a decision procedure, and the difference
// matters in both directions. It reports whether job's raw template contains
// the bytes "EXPR" anywhere.
//
// WHAT IT GETS RIGHT: a declared extension is spelled literally
// (`extensions: [EXPR]` / `"extensions":["EXPR"]`) by everything that writes
// one -- 207 of the 209 vendored EXPR conformance fixtures contain the bytes,
// and the two that do not are the two that deliberately omit the declaration
// (3.6--let-requires-expr.invalid.yaml, expr-extension-missing.invalid.yaml).
// So a real EXPR template matches.
//
// ITS FALSE POSITIVE IS LIVE TODAY, unlike the false negative below. A
// base-spec template containing those four bytes anywhere -- a comment, an
// environment variable named HOUDINI_EXPR_CACHE -- is treated as EXPR-capable.
// On a correctly-configured farm that costs nothing (the shortfall is empty and
// this function is never called). On a farm whose workers are tighter than the
// server it is a REAL, REACHABLE misdiagnosis: those workers refuse the job and
// the task is flagged with a reason about EXPR limits it has nothing to do
// with. It errs in the safe direction -- work is withheld, never wrongly
// dispatched -- but the operator sees a message naming a feature they may not
// use. No shipped preset trips it today (presets/sqi/houdini-rop-render.yaml
// matches only lowercase `expr`), and that is luck rather than design.
//
// WHAT IT GETS WRONG, stated plainly because an earlier revision of this
// comment claimed the opposite ("a template whose bytes do not contain EXPR
// cannot declare it") and that claim is FALSE. The declaration is a decoded
// STRING, not a byte sequence: YAML's "EXP\x52" and JSON's "\u0045XPR" both
// decode to EXPR, are accepted by openjd.Parse, and are stored verbatim in
// store.Job.RawTemplate, so this function returns false for a template that
// genuinely declares the extension. The gate then passes, the job dispatches to
// a tight worker, and its tasks fail at run time — the design spec §2 incident
// this file exists to prevent.
//
// WHY IT SHIPS ANYWAY, and what would replace it:
//
//  1. THE FALSE NEGATIVE is unreachable today -- and only the false negative;
//     see the live false positive above. Reaching it needs a template that
//     declares EXPR, and EXPR is StatusInProgress, so no such template can be
//     submitted at all until sub-project H.
//  2. Reaching it requires deliberately obfuscating an extension declaration,
//     which no authoring tool does and which gains the submitter nothing: the
//     only consequence is that their own job's tasks fail.
//  3. The exact check is a document parse, and the cheap placements for it do
//     not exist. Doing it here costs a parse per CANDIDATE TASK per lease
//     request (AssignBatchSize defaults to 50; internal/api/jobs.go accepts a
//     4 MiB request body) on
//     any misconfigured farm — an availability problem strictly worse than the
//     one it closes. Doing it in buildAssignPayload, where the template is
//     already parsed for the winner only, is after LeaseReadyTask and
//     createAttemptAndClaimUsage, so it would need a revert that leaves an
//     orphaned attempt row behind on every retry.
//
// H HANDOFF — the clean fix is neither of those two: persist the declared
// extension list on the job row at submission (where internal/openjd has
// already parsed and validated it) and read a column here. Exact, no parse on
// the lease path, and it needs this heuristic only as the fallback for rows
// written before that column existed.
func jobMayUseEXPR(job store.Job) bool {
	return strings.Contains(job.RawTemplate, openjd.ExtensionEXPR)
}

// exprCapsBlock returns "" when a worker whose shortfall is workerShortfall may
// be given job, or the operator-facing reason it may not.
//
// workerShortfall is [exprCapShortfall] for that worker, taken as a parameter
// rather than recomputed because it depends only on the worker and this
// server's configuration — constant for a whole lease batch, while this
// function is called once per candidate task. On the passing path that call
// costs eight integer comparisons (four "was it advertised", four dimensions)
// and allocates nothing; on a misconfigured farm
// it formats up to four sentences, which is why [Scheduler.selectLeaseBatch]
// hoists it out of the candidate loop. The template scan below likewise only
// ever runs on a farm that is already misconfigured.
//
// Both callers matter and neither may be dropped: [Scheduler.leaseGatesPass] is
// the bound (the task is never handed over), and [Scheduler.evaluateSchedulability]
// is what the operator sees (the same reason, written onto the task by the
// unschedulable sweep). A bound with no explanation is a silent stall; an
// explanation with no bound is the warning-nobody-reads this program has
// repeatedly found is not a bound at all.
func exprCapsBlock(workerShortfall string, job store.Job) string {
	if workerShortfall == "" || !jobMayUseEXPR(job) {
		return ""
	}
	return workerShortfall
}

// workerExprShortfall is [exprCapShortfall] for worker, against the limits this
// server accepts templates under.
func (s *Scheduler) workerExprShortfall(worker store.Worker) string {
	return exprCapShortfall(worker.ExprLimits, s.cfg.ExprLimits)
}
