// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// exprcaps.go enforces the one invariant that spans the two binaries: a
// worker's OpenJD EXPR evaluation caps must not be tighter than the server's,
// or a job the server accepts fails on the worker — after submission, once per
// task, naming a budget the submitter never saw.
//
// The failure is measured. With the server at 10,000 expression positions and
// the worker at 5,000, a job with one 5,000-variable environment was accepted,
// created and persisted, and then every task in it failed at runtime. A test
// relating the two binaries' default constants (internal/openjd's
// TestTemplateBudget_WorkerCapIsNotTighter, a test-only import of
// internal/worker/fmtres from a package no production file of which may import
// it -- this package's tests do the same) cannot see operator configuration,
// and both sides are configurable.
//
// The mechanism: the worker advertises the caps it will enforce in its
// registration message; the server persists them on the worker record
// ([store.WorkerExprLimits]); and the scheduler refuses to dispatch an EXPR job
// to a worker whose advertised caps are below this server's configured limits.
// The refusal is the bound. The reason string it produces is also what the
// unschedulable sweep writes onto the task, so an operator sees the cause on
// the job rather than having to correlate two config files.
//
// Why refuse to dispatch rather than refuse to submit: rejecting the template
// at submit would need the server to know every worker's caps at submission
// time, making acceptance depend on which hosts happen to be online and
// defeating the per-worker sizing these knobs exist for. Refusing to dispatch
// keeps a heterogeneous farm working: a capable worker still runs the job.
//
// Why it narrows to EXPR jobs: the caps bound nothing but EXPR phase-3
// evaluation. Taking a whole host out of the farm over them would be a far
// larger outage than the one being prevented, and a small host tightening its
// limits is exactly the use this configuration exists for.
//
// What it cannot do: worker >= server is necessary, not sufficient. Phase 3
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
// enforce: the caps compiled into every sqi-worker built before workers
// advertised them.
//
// A worker that advertises always reports real values (its configuration layer
// rejects 0 as out of range and registration normalizes before publishing), so
// silence means an older binary, whose limits were fixed constants equal to
// these numbers. Reading silence as 0 would refuse every such worker in the
// farm, and reading it as unlimited would fail open in exactly the case this
// file exists for.
//
// fmtres's TestExprLimits_DefaultsMatchLiteralValues pins that
// fmtres.DefaultExprLimits() still equals these numbers, and this package's
// TestExprCaps_UnadvertisedCapsAreTheDefaults pins this copy against
// fmtres so the two cannot drift.
//
// They are duplicated rather than imported because internal/scheduler is server
// code: it has no business linking the worker's evaluator into the server
// binary for five integers.
var legacyWorkerExprCaps = store.WorkerExprLimits{
	OperationLimit:          1_000_000,
	MemoryLimit:             20_000_000,
	AssignmentPositions:     10_000,
	AssignmentRetainedBytes: 20_000_000,
	LetRetainedBytes:        10_000_000,
}

// workerExprCapsOrLegacy replaces every unadvertised (<= 0) cap with what such
// a worker is assumed to enforce ([legacyWorkerExprCaps], which records where
// that assumption comes from). Treating an absent value as 0 would report a
// shortfall in every dimension for every worker that predates advertising;
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
	if c.LetRetainedBytes <= 0 {
		c.LetRetainedBytes = legacyWorkerExprCaps.LetRetainedBytes
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
// meters the same quantity one phase later:
//
//	openjd.expr_template_positions       <= expr.assignment_positions
//	openjd.expr_operation_limit          <= expr.operation_limit
//	openjd.expr_memory_limit             <= expr.memory_limit
//	openjd.expr_template_retained_bytes  <= expr.assignment_retained_bytes
//	openjd.expr_template_retained_bytes  <= expr.let_retained_bytes
//
// The first is a true subset relation: an assignment resolves a subset of the
// template's positions, and a position is a position on both sides. The others
// relate budgets over the same quantity at different scopes (one evaluation,
// one symbol table, one assignment, one template walk), where the worker's
// value is additionally inflated by concrete data — so they are necessary
// conditions, not guarantees. See the file header.
//
// The fifth pairing has no exact counterpart, which is why it compares against
// the server's template-wide budget. expr.let_retained_bytes bounds one phase-3
// symbol table; the server meters no such scope. What it does meter is part of
// the same quantity at a wider scope: openjd.expr_template_retained_bytes is
// the cumulative size of every let: binding the whole walk retains, so for any
// template this server accepts, the let bindings landing in any one of its
// tables are a subset of that sum and therefore fit under it.
//
// That bounds the let bindings and nothing else, and the difference is the
// residual this comparison does not close. The server charges only the net
// delta a let: block adds (exprcheck.go's stepLet diff and the script blocks'
// before/after subtraction, which cancels the baseline); the worker measures
// the whole table it is about to bind into -- Task.Param.*, Task.File.*,
// Session.*, Env.File.* included. A 3 MB string parameter puts a table at
// ~6 MB before a single binding is evaluated (measured; see evalLetBindings),
// which this server never charged and therefore never compared. So a worker
// set exactly at openjd.expr_template_retained_bytes can still fail an
// accepted job on its parameters alone. This comparison narrows the gap; it
// does not close it.
//
// It is conservative in one direction: a template that spreads the
// template-wide budget across many steps needs far less than that in any one
// table, so a worker can be blocked over a budget no single table of the jobs
// it would actually be given ever wanted. That is the same over-strictness the
// assignment_retained_bytes row above already carries (an assignment's tables
// are likewise a subset of a template's), and it is the safe direction: the
// cost is a visible refusal naming both keys, against an invisible per-task
// failure after acceptance.
//
// Comparing against openjd.expr_memory_limit, the largest single value one
// evaluation may hold, is unsound because a table accumulates. At the shipped
// defaults (memory 1,000,000; template retained 10,000,000) a worker at the
// expr.let_retained_bytes floor of 1,000,000 would pass that comparison, while
// a let: block of eight bindings at 1,000,000 bytes each is accepted by this
// server (8,000,000 <= 10,000,000) and rejected on that worker at the second
// binding. Section 3.6 allows 50 bindings per block, so the sufficient form of
// that comparison is 50 x openjd.expr_memory_limit -- 500,000,000 at the
// server's own ceiling, which exceeds fmtres.MaxExprLetRetainedBytes
// (100,000,000) and would therefore be unsatisfiable by any legal worker
// configuration. The template-wide budget is satisfiable at every legal server
// setting (both ceilings are 100,000,000) and is the tightest server-side
// quantity that provably upper-bounds one table's let bindings.
//
// As with the other three, it does not promise more: the worker measures the
// whole table, parameters included, and phase 3 binds concrete values phase 2
// only had placeholders for. A job whose own parameters are large, or whose
// bindings grow once resolved, can still exceed a worker that passes this
// comparison.
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
	if c.LetRetainedBytes < srv.TemplateRetainedBytes {
		short = append(short, fmt.Sprintf(
			"lets one symbol table hold %d live bytes but this server accepts templates whose "+
				"let: bindings retain up to %d (expr.let_retained_bytes vs "+
				"openjd.expr_template_retained_bytes)",
			c.LetRetainedBytes, srv.TemplateRetainedBytes,
		))
	}
	if len(short) == 0 {
		return ""
	}
	return "worker EXPR limits are tighter than this server's: it " + strings.Join(short, "; ") +
		". A job accepted under the server's limits would fail on this worker once per task, " +
		"so it is not offered EXPR work"
}

// jobUsesEXPR reports whether job declares the OpenJD EXPR extension, and it is
// exact for every job submitted since migration 00027 added
// jobs.declared_extensions: internal/openjd parses and registry-validates the
// declaration at submission and the answer is persisted on the job row, so this
// reads a decoded list rather than guessing from bytes.
//
// It costs no extra query. The column is on jobCols, so the GetJob the lease
// path already issues for each candidate task carries it, unlike a
// per-candidate document parse (see item 3 under [jobMayUseEXPR]).
//
// The fallback is not dead code. Every job row written before that migration
// reads as not recorded, and for those the byte scan remains the only evidence
// there is. That is why the column defaults to ” rather than to '[]': '[]'
// would say "this template declares nothing", ungating every EXPR job already
// in the queue at upgrade time. Recorded-and-empty is a different state, and it
// does not fall through here.
func jobUsesEXPR(job store.Job) bool {
	if declared, recorded := job.DeclaresExtension(openjd.ExtensionEXPR); recorded {
		return declared
	}
	return jobMayUseEXPR(job)
}

// jobMayUseEXPR is the legacy path, reached only for a job row that predates
// migration 00027 and therefore records no declared-extension list.
// [jobUsesEXPR] is the primary, and it is exact; everything below describes
// what this one can and cannot do for the rows the primary cannot answer for.
//
// It is a heuristic, not a decision procedure, and can be wrong in both
// directions. It reports whether job's raw template contains both the bytes
// "EXPR" and the bytes "extensions" -- the key any declaration of it must sit
// under.
//
// What it gets right: a declared extension is spelled literally
// (`extensions: [EXPR]` / `"extensions":["EXPR"]`) by everything that writes
// one. Measured over the vendored conformance corpus: 201 of the 209 EXPR
// fixtures contain both byte sequences, and every one of the 8 that do not is a
// fixture that deliberately does not declare the extension -- 2 omit the bytes
// EXPR entirely (3.6--let-requires-expr.invalid.yaml,
// expr-extension-missing.invalid.yaml) and 6 `*requires-expr.invalid.yaml`
// mention EXPR only in a leading comment about the declaration they omit. So
// this matches every fixture that really declares it and none that does not,
// and across the 462 base and TASK_CHUNKING fixtures it matches nothing at all.
//
// Why both byte sequences: a base-spec template containing "EXPR" in a comment
// or in an environment variable named HOUDINI_EXPR_CACHE declares no extensions
// at all, so it does not match. Matching it would withhold it from every short
// worker with a reason naming limits it does not use. On a correctly-configured
// farm that costs nothing -- the shortfall is empty and this function is never
// called -- but on a farm whose workers are tighter than the server, with no
// capable worker in the queue, the job's tasks would sit `ready` indefinitely,
// and with scheduler.unschedulable_grace <= 0 they would sit with nothing
// written on them at all. That farm is reachable through documented
// configuration ("raise the workers first" is guidance, not enforcement).
//
// A line-scoped check ("EXPR" and "extensions" on the same line) matches 0 of
// the 209 EXPR fixtures, because the block sequence form puts the value on its
// own line. It would turn a tolerable false positive into the false negative
// below, which dispatches EXPR work to a worker that cannot run it.
//
// The residual false positive is a template that declares some other extension
// and separately mentions EXPR. No fixture and no shipped preset does
// (presets/sqi/houdini-rop-render.yaml matches only lowercase `expr`), and the
// behavior when it happens is that work is withheld, never wrongly dispatched.
//
// What it gets wrong: a template whose bytes do not contain EXPR can still
// declare it. The declaration is a decoded string, not a byte sequence: YAML's
// "EXP\x52" and JSON's "\u0045XPR" both decode to EXPR, are accepted by
// openjd.Parse, and are stored verbatim in store.Job.RawTemplate, so this
// function returns false for a template that genuinely declares the extension.
// The gate then passes, the job dispatches to a tight worker, and its tasks fail
// at run time -- the incident this file exists to prevent. Requiring
// "extensions" as well widens that escape by one spelling -- obfuscating the key
// works too -- which is the same class, reachable only by deliberately
// obfuscating a declaration, and bounded by the same reasons below.
//
// Why it is kept:
//
//  1. Both error directions are bounded to pre-00027 rows. Every job submitted
//     since carries the decoded list, and [jobUsesEXPR] never consults this
//     function for those. What is left is the finite, draining set of rows a
//     deployment already held when it upgraded.
//  2. For those rows there is no better evidence. The raw template is all that
//     was persisted, so the alternatives are this scan, a document parse per
//     candidate task per lease request (AssignBatchSize defaults to 50;
//     internal/api/jobs.go accepts a 4 MiB request body) on any misconfigured
//     farm -- an availability problem strictly worse than the one it closes --
//     or a backfill, which would have to re-parse and re-validate every stored
//     template against a registry that has changed since they were accepted.
//  3. Its remaining false negative needs a deliberately obfuscated declaration
//     in a job submitted before the upgrade, which no authoring tool produces
//     and which gains the submitter nothing: the only consequence is that their
//     own job's tasks fail.
//
// Deleting it and treating an unrecorded row as "declares nothing" would ungate
// every EXPR job that survived the upgrade.
func jobMayUseEXPR(job store.Job) bool {
	return strings.Contains(job.RawTemplate, openjd.ExtensionEXPR) &&
		strings.Contains(job.RawTemplate, extensionsKey)
}

// extensionsKey is the top-level template key every extension declaration sits
// under -- the one internal/openjd's parse.go reads (`getStringSlice(raw,
// "extensions")`). A literal, because what this file matches is bytes in the
// raw document, not a decoded field.
const extensionsKey = "extensions"

// exprCapsBlock returns "" when a worker whose shortfall is workerShortfall may
// be given job, or the operator-facing reason it may not.
//
// workerShortfall is [exprCapShortfall] for that worker, taken as a parameter
// rather than recomputed because it depends only on the worker and this
// server's configuration — constant for a whole lease batch, while this
// function is called once per candidate task. On the passing path that call
// costs ten integer comparisons (five "was it advertised", five dimensions)
// and allocates nothing; on a misconfigured farm
// it formats up to five sentences, which is why [Scheduler.selectLeaseBatch]
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
	if workerShortfall == "" || !jobUsesEXPR(job) {
		return ""
	}
	return workerShortfall
}

// workerExprShortfall is [exprCapShortfall] for worker, against the limits this
// server accepts templates under.
func (s *Scheduler) workerExprShortfall(worker store.Worker) string {
	return exprCapShortfall(worker.ExprLimits, s.cfg.ExprLimits)
}
