// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// exprcaps.go enforces the one invariant that spans the two binaries: a
// worker's OpenJD EXPR evaluation caps must not be tighter than the server's,
// or a job the server accepts fails on the worker — after submission, once per
// task, naming a budget the submitter never saw.
//
// Both sides are operator configuration, so a test relating the two binaries'
// default constants (internal/openjd's TestTemplateBudget_WorkerCapIsNotTighter)
// cannot enforce it on a running farm.
//
// The worker advertises the caps it enforces in its registration message; the
// server persists them on the worker record ([store.WorkerExprLimits]); and the
// scheduler does not dispatch an EXPR job to a worker whose advertised caps are
// below this server's configured limits. The reason string is also what the
// unschedulable sweep writes onto the task, so an operator sees the cause on
// the job.
//
// The check is at dispatch, not at submission: rejecting at submission would
// make acceptance depend on which workers are online and defeat per-worker
// sizing. At dispatch a capable worker still runs the job.
//
// Only EXPR jobs are withheld, because the caps bound nothing but phase-3 EXPR
// evaluation; the worker keeps running base-spec work.
//
// Worker >= server is necessary, not sufficient. Phase 3 evaluates concrete
// values where phase 2 had placeholders, so the same expression can cost more
// on the worker than it did at submission (the worker's default operation
// budget is 100x the server's and its memory budget 20x). This gate covers
// misconfiguration, not costs that come from the values themselves.

import (
	"fmt"
	"strings"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
)

// legacyWorkerExprCaps is what a worker that advertises no caps is assumed to
// enforce: the worker defaults, fmtres.DefaultExprLimits().
//
// A worker that advertises caps always reports positive values (its
// configuration layer rejects 0 and registration normalizes before
// publishing), so a zero means the worker sent none. Reading it as 0 would
// refuse the worker everything; reading it as unlimited would fail open.
//
// fmtres's TestExprLimits_DefaultsMatchLiteralValues pins
// fmtres.DefaultExprLimits() to these numbers, and
// TestExprCaps_UnadvertisedCapsAreTheDefaults pins this copy against fmtres.
//
// They are duplicated rather than imported so the server binary does not link
// the worker's evaluator.
var legacyWorkerExprCaps = store.WorkerExprLimits{
	OperationLimit:          1_000_000,
	MemoryLimit:             20_000_000,
	AssignmentPositions:     10_000,
	AssignmentRetainedBytes: 20_000_000,
	LetRetainedBytes:        10_000_000,
}

// workerExprCapsOrLegacy replaces every unadvertised (<= 0) cap with the value
// in [legacyWorkerExprCaps].
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
// value is also inflated by concrete data, so they are necessary conditions,
// not guarantees. See the file header.
//
// The fifth pairing has no exact counterpart. expr.let_retained_bytes bounds
// one phase-3 symbol table, a scope the server does not meter.
// openjd.expr_template_retained_bytes is the cumulative size of every let:
// binding the whole template walk retains, so for any accepted template the
// let bindings in any one of its tables fit under it.
//
// That bounds the let bindings only. The server charges the net delta a let:
// block adds (exprcheck.go's stepLet diff and the script blocks' before/after
// subtraction); the worker meters the whole table it binds into, including
// Task.Param.*, Task.File.*, Session.* and Env.File.*. A 3 MB string parameter
// puts a table at about 6 MB before any binding is evaluated (see
// evalLetBindings), and the server never charges that. A worker set exactly at
// openjd.expr_template_retained_bytes can therefore still fail an accepted job
// on its parameters alone.
//
// The comparison is conservative: a template that spreads the template-wide
// budget across many steps needs far less in any one table, so a worker can be
// withheld work no single table of it would exceed. The
// assignment_retained_bytes pairing has the same property. A refusal naming
// both keys is preferred to a per-task failure after acceptance.
//
// openjd.expr_memory_limit, the largest single value one evaluation may hold,
// is not a sound counterpart because a table accumulates. At the defaults
// (memory 1,000,000; template retained 10,000,000) a worker at the
// expr.let_retained_bytes floor of 1,000,000 would pass that comparison, while
// a let: block of eight 1,000,000-byte bindings is accepted by this server and
// rejected on that worker at the second binding. Template Schemas section 3.6
// allows 50 bindings per block, so the sufficient form would be
// 50 x openjd.expr_memory_limit: 500,000,000 at the server's ceiling, above
// fmtres.MaxExprLetRetainedBytes (100,000,000), so no legal worker
// configuration could satisfy it. The template-wide budget is satisfiable at
// every legal server setting (both ceilings are 100,000,000) and is the
// tightest server-side quantity that upper-bounds one table's let bindings.
//
// As with the other pairings, a job whose own parameters are large, or whose
// bindings grow once resolved, can still exceed a worker that passes.
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

// jobUsesEXPR reports whether job declares the OpenJD EXPR extension. For a
// job row that records its declared extensions (jobs.declared_extensions,
// parsed and registry-validated by internal/openjd at submission) the answer
// is exact.
//
// It costs no extra query: the column is on jobCols, so the GetJob the lease
// path issues for each candidate task carries it.
//
// A row whose column holds the default empty string records nothing and falls back to the
// byte scan in [jobMayUseEXPR]. A recorded empty list ('[]') means the template
// declares no extensions and does not fall back.
func jobUsesEXPR(job store.Job) bool {
	if declared, recorded := job.DeclaresExtension(openjd.ExtensionEXPR); recorded {
		return declared
	}
	return jobMayUseEXPR(job)
}

// jobMayUseEXPR is the fallback for a job row that records no declared
// extensions (see [jobUsesEXPR]). It reports whether job's raw template
// contains both the bytes "EXPR" and the bytes "extensions", the key any
// declaration must sit under. It is a heuristic and can be wrong in both
// directions.
//
// Authoring tools spell a declaration literally (`extensions: [EXPR]` /
// `"extensions":["EXPR"]`). Over the vendored conformance corpus it matches
// every EXPR fixture that declares the extension and none of the base or
// TASK_CHUNKING fixtures.
//
// Requiring "extensions" as well as "EXPR" keeps a base-spec template that
// mentions EXPR in a comment or an environment variable name (such as
// HOUDINI_EXPR_CACHE) from matching; such a template would otherwise be
// withheld from every short worker with a reason naming limits it does not
// use. A line-scoped check would miss the block-sequence form, which puts the
// value on its own line.
//
// False positive: a template that declares another extension and separately
// mentions EXPR. Its work is withheld, never wrongly dispatched.
//
// False negative: a declaration is a decoded string, not a byte sequence: YAML's
// "EXP\x52" and JSON's "\u0045XPR" both decode to EXPR, are accepted by
// openjd.Parse, and are stored verbatim in store.Job.RawTemplate, so this
// returns false for them (an escaped "extensions" key likewise). The job can
// then be dispatched to a worker that cannot run it, and its tasks fail. No
// authoring tool writes such a spelling, and only the submitter's own tasks
// are affected.
//
// Treating an unrecorded row as "declares nothing" instead would dispatch
// every such EXPR job to short workers, and parsing the document per
// candidate task per lease request costs more than the failure it prevents.
func jobMayUseEXPR(job store.Job) bool {
	return strings.Contains(job.RawTemplate, openjd.ExtensionEXPR) &&
		strings.Contains(job.RawTemplate, extensionsKey)
}

// extensionsKey is the top-level template key every extension declaration sits
// under, the one internal/openjd's parse.go reads (`getStringSlice(raw,
// "extensions")`). It is a literal because this file matches bytes in the raw
// document, not a decoded field.
const extensionsKey = "extensions"

// exprCapsBlock returns "" when a worker whose shortfall is workerShortfall may
// be given job, or the operator-facing reason it may not.
//
// workerShortfall is [exprCapShortfall] for that worker, taken as a parameter
// rather than recomputed because it depends only on the worker and this
// server's configuration, constant for a whole lease batch, while this
// function is called once per candidate task. On a misconfigured farm it
// formats up to five sentences, so [Scheduler.selectLeaseBatch] computes it
// once outside the candidate loop. The template scan runs only when the
// shortfall is non-empty.
//
// It is reached through [workerCompatBlock], which both callers need:
// [Scheduler.leaseGatesPass] withholds the task, and
// [Scheduler.evaluateSchedulability] gives the operator the same reason,
// written onto the task by the unschedulable sweep.
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
