// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"time"

	"github.com/uberware/sqi/internal/openjd/expr"
)

// ─── operator-configurable expression limits ────────────────────────────────

// ExprLimits bounds what one submitted template may spend inside the EXPR
// expression checker: four operator-configurable numbers, plus the
// per-request [ExprLimits.Deadline].
//
// Two of the four bound one expression evaluation (SubmissionOperations,
// section 1.3.10, and SubmissionMemoryBytes, section 1.3.9). The other two
// bound the whole template across one checkTemplateExpressions walk
// (TemplatePositions and TemplateRetainedBytes). Exceeding any of them makes
// the template invalid (422). Each field's comment names its config key, and
// the defaults are explained beside their constants in exprcheck.go.
//
// Deadline is not one of the four. It is set per request, measures wall-clock
// time, and a breach is a 503 (this server stopped working on the template)
// rather than a 422. It is carried here because this struct is already
// threaded to every evaluation.
//
// A zero or negative field means "unset, use the default" (see
// [ExprLimits.orDefaults]), never "unlimited". Callers with no operator
// configuration, such as internal/product's ValidateTemplate, therefore get
// the defaults without naming them.
//
// The ranges an operator may choose from (the Min/MaxExpr* constants below)
// are enforced at startup by internal/config, not here. This type accepts any
// positive value, so tests can set an extreme one to prove a limit is wired.
//
// The scheduler withholds EXPR work from any worker whose advertised caps are
// below these values (internal/scheduler/exprcaps.go), so raising a server
// limit past a worker's matching cap needs that worker's cap raised first.
type ExprLimits struct {
	// SubmissionOperations is the section 1.3.10 operation budget for one
	// expression evaluation at submission time (openjd.expr_operation_limit,
	// default 10,000; worker counterpart expr.operation_limit).
	//
	// Catastrophe bound. Together with TemplatePositions it bounds the
	// operations one request may spend (positions x operations, 10^8 at the
	// defaults). Operations do not measure time: section 1.3.10 prices 256
	// bytes of string work at one operation, so a byte-heavy expression costs
	// far more wall clock per operation than arithmetic does.
	// [ExprLimits.Deadline] is what bounds time. Raise this when a template's
	// expressions need more operations; it does not buy time.
	SubmissionOperations int64

	// SubmissionMemoryBytes is the section 1.3.9 live-byte budget for one
	// expression evaluation at submission time (openjd.expr_memory_limit,
	// default 1,000,000; worker counterpart expr.memory_limit).
	//
	// Catastrophe bound. It caps the sum of live values in one evaluation,
	// recursing into containers, and so how much byte-heavy, operation-cheap
	// work one position can do. internal/openjd/expr's fixed maxStringBytes
	// (10,000,000) caps only a single produced string, so it does not limit
	// this sum: raising this limit permits proportionally more live memory per
	// evaluation on an unauthenticated path.
	SubmissionMemoryBytes int64

	// TemplatePositions is how many expression positions one
	// checkTemplateExpressions walk may check (openjd.expr_template_positions,
	// default 10,000).
	//
	// Catastrophe bound, and the other factor in the per-request operation
	// bound. Without it the walk's cost grows with template size, limited only
	// by the 4 MiB request body.
	//
	// The worker's expr.assignment_positions (default 10,000) bounds the same
	// quantity at phase 3, and an assignment's positions are a subset of its
	// template's. A worker whose cap is below this value is never sent EXPR
	// work (the unschedulable sweep reports why), so raising this above 10,000
	// withholds EXPR work from every worker still at its default.
	// TestTemplateBudget_WorkerCapIsNotTighter compares the two defaults.
	TemplatePositions int64

	// TemplateRetainedBytes is how many bytes every let: block in the template
	// may retain in total across one walk (openjd.expr_template_retained_bytes,
	// default 10,000,000). It is summed over the whole walk, not reset per
	// block; let is the only construct that keeps a value, every other
	// position discards its result.
	//
	// Policy bound, the only one of the four. A single block is already
	// bounded by maxLetBindings x SubmissionMemoryBytes; this bounds the sum
	// across many individually compliant blocks, which a template with many
	// steps can legitimately grow, so its configurable range is wide.
	//
	// The scheduler compares it against the worker's expr.let_retained_bytes
	// (default 10,000,000) and expr.assignment_retained_bytes (default
	// 20,000,000). The default has no headroom against let_retained_bytes, so
	// raising this withholds EXPR work from every worker still at its default.
	TemplateRetainedBytes int64

	// Deadline, when non-zero, is an absolute wall-clock time after which
	// evaluation stops with [expr.ErrDeadlineExceeded], which the submission
	// path reports as a 503. It is computed at the start of each submission
	// from openjd.expr_submission_deadline (default 5s, range 1s to 60s). See
	// [ValidateWithBudget].
	//
	// Raising the four limits above without raising the deadline can turn
	// legitimate templates that the counters would accept into 503s.
	//
	// It is exempt from [ExprLimits.orDefaults]: the zero time means no
	// deadline, and that is the default.
	//
	// Never set it on a long-lived ExprLimits. [SubmitterOptions.ExprLimits]
	// is read once at server boot into a [Submitter] reused for every
	// submission, so a deadline stored there is one fixed instant, and once it
	// passes every later submission fails. See that field's comment for what
	// the submission path does instead.
	Deadline time.Time
}

// The range an operator may configure each of the four to. Out-of-range values
// are rejected at startup by internal/config's validateOpenJD, which keeps its
// own copies of these numbers (internal/config must not import this package);
// internal/server's TestExprLimits_ConfigMatchesOpenJD keeps the copies in
// step.
//
// The three catastrophe bounds have a ceiling one order of magnitude above
// their default. These are judgements, not derived from a fixed guard firing
// first. The policy bound, TemplateRetainedBytes, has a wide range.
//
// Each floor leaves at least 4x headroom over the most any reference preset in
// presets/sqi/ needs, so no operator can tighten a limit far enough to reject
// sqi's own templates. TestExprLimits_FloorsAcceptReferencePresets measures
// this on every run. Currently the largest needs are 15 operations and 390
// live bytes in one evaluation (ffmpeg-segment-transcode-expr), and 38
// positions and 512 retained let bytes in one template
// (arnold-kick-tile-render).
const (
	// MinExprSubmissionOperations / MaxExprSubmissionOperations bound
	// [ExprLimits.SubmissionOperations].
	//
	// Ceiling: 10x the default. A raised value lengthens the worst-case CPU
	// cost of one request proportionally. With the counters alone, a request
	// can cost at least ~17 minutes of server CPU at the defaults and ~40
	// hours with this and MaxExprTemplatePositions both at their ceilings.
	// What actually stops a request is [ExprLimits.Deadline], whose 60s
	// ceiling is far below either figure.
	//
	// Floor: 10x below the default, ~67x the 15 operations the most expensive
	// reference preset evaluation needs.
	MinExprSubmissionOperations int64 = 1_000
	MaxExprSubmissionOperations int64 = 100_000

	// MinExprSubmissionMemoryBytes / MaxExprSubmissionMemoryBytes bound
	// [ExprLimits.SubmissionMemoryBytes].
	//
	// Ceiling: 10x the default. Host RAM must be sized against more than this
	// number. The TemplateRetainedBytes charge lands once per let: block, after
	// the block has finished evaluating, so one block can transiently hold up
	// to maxLetBindings (50, Template Schemas section 3.6) x this limit before
	// any counter sees it: 50 MB at the default and 500 MB at the ceiling, per
	// concurrently resolving request. The ceiling happens to equal
	// internal/openjd/expr's maxStringBytes but is not derived from it.
	//
	// Floor: 4 KB, ~10x the 390 live bytes of the largest reference preset
	// expression.
	MinExprSubmissionMemoryBytes int64 = 4_096
	MaxExprSubmissionMemoryBytes int64 = 10_000_000

	// MinExprTemplatePositions / MaxExprTemplatePositions bound
	// [ExprLimits.TemplatePositions].
	//
	// Ceiling: 10x the default. Raising both this and
	// MaxExprSubmissionOperations to their ceilings multiplies the per-request
	// operation bound by 100 (10^8 to 10^10).
	//
	// Floor: 256, ~6.7x the 38 positions of the largest reference preset and
	// ~40x below the default, so an operator running only small templates can
	// tighten hard.
	MinExprTemplatePositions int64 = 256
	MaxExprTemplatePositions int64 = 100_000

	// MinExprTemplateRetainedBytes / MaxExprTemplateRetainedBytes bound
	// [ExprLimits.TemplateRetainedBytes], the one policy bound, so the range
	// is wide: 64 KB (about a thousand small let bindings, 128x the 512 bytes
	// the largest reference preset retains) up to 100 MB of live values from
	// one request.
	MinExprTemplateRetainedBytes int64 = 65_536
	MaxExprTemplateRetainedBytes int64 = 100_000_000
)

// DefaultExprLimits returns the default four values, which internal/config's
// OpenJDConfig also defaults to. A server started with no expression-limit
// configuration enforces exactly these.
func DefaultExprLimits() ExprLimits {
	return ExprLimits{
		SubmissionOperations:  defaultSubmissionOperations,
		SubmissionMemoryBytes: defaultSubmissionMemoryBytes,
		TemplatePositions:     defaultTemplatePositions,
		TemplateRetainedBytes: defaultTemplateRetainedBytes,
	}
}

// Normalized is the exported form of [ExprLimits.orDefaults]: the values a
// walk actually enforces. internal/scheduler compares them against each
// worker's advertised EXPR caps before dispatching an EXPR job; comparing an
// unset zero would report no shortfall in that dimension.
func (l ExprLimits) Normalized() ExprLimits { return l.orDefaults() }

// orDefaults returns l with every unset (<= 0) field replaced by its default.
//
// It patches a copy of l rather than building a new ExprLimits field by
// field, so [ExprLimits.Deadline], which has no default, passes through. A
// rebuild would drop the deadline silently. Any new field passes through the
// same way; a field that needs a default must be added below explicitly.
//
// [newTemplateBudget] applies it, so every consumption point reads normalized
// limits. Unset means default rather than unlimited because the zero value of
// a struct literal is the common case here, and a resource-exhaustion guard
// must never become unlimited by accident.
func (l ExprLimits) orDefaults() ExprLimits {
	d := DefaultExprLimits()
	if l.SubmissionOperations <= 0 {
		l.SubmissionOperations = d.SubmissionOperations
	}
	if l.SubmissionMemoryBytes <= 0 {
		l.SubmissionMemoryBytes = d.SubmissionMemoryBytes
	}
	if l.TemplatePositions <= 0 {
		l.TemplatePositions = d.TemplatePositions
	}
	if l.TemplateRetainedBytes <= 0 {
		l.TemplateRetainedBytes = d.TemplateRetainedBytes
	}
	return l
}

// evalOptions builds the expr.Option slice every submission-time evaluation
// runs under: checkFormatString's, checkLetBindings' and resolve.go's
// expr.Eval call sites. The values come from this walk's budget. It returns a
// new slice on each call.
//
// It also adds [ExprLimits.Deadline] when one is set, so the per-request
// deadline reaches every walk position without a signature change.
func (l ExprLimits) evalOptions() []expr.Option {
	opts := []expr.Option{
		expr.WithOperationLimit(l.SubmissionOperations),
		expr.WithMemoryLimit(l.SubmissionMemoryBytes),
	}
	if !l.Deadline.IsZero() {
		opts = append(opts, expr.WithDeadline(l.Deadline))
	}
	return opts
}
