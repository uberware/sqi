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
// The four numbers split into two per-EVALUATION bounds (SubmissionOperations,
// SubmissionMemoryBytes -- what ONE expression position may cost, section
// 1.3.10 and section 1.3.9 respectively) and two per-WALK bounds
// (TemplatePositions, TemplateRetainedBytes -- what the WHOLE template may
// cost across one checkTemplateExpressions call). See each field's own
// comment, and the rationale blocks in exprcheck.go for each field's
// DEFAULT.
//
// The fifth field is none of those things and is documented as such on itself:
// it is per-REQUEST rather than operator configuration, it measures TIME rather
// than counting anything, and it decides whether this server keeps working
// (503) rather than whether the template is valid (422). It rides on this
// struct because this struct is the bundle already threaded to every
// evaluation. Phrases like "the four" below mean the four numbers, and
// deliberately exclude it.
//
// A ZERO field means "unset, use the default" -- see [ExprLimits.orDefaults].
// That is deliberate: every ValidateOptions{EnforceLimits: ...} literal in
// this repo (and every caller that has no operator configuration to offer,
// e.g. internal/product's ValidateTemplate) gets the defaults without naming
// them. It also means a NEGATIVE value is not accepted as "unlimited":
// orDefaults treats anything <= 0 as unset, and internal/config's
// validateOpenJD rejects an out-of-range configured value at startup long
// before one could reach here.
//
// The ranges an OPERATOR may choose from are NOT enforced here -- they live in
// internal/config (MinOpenJDExpr*/MaxOpenJDExpr*) and are rejected at load,
// because internal/config is the layer that owns operator-facing policy and
// this package must stay importable from it-free code. This type accepts any
// positive value so that a test can deliberately set an absurd one to prove a
// knob is wired (which is how the mutation tests for these limits work).
type ExprLimits struct {
	// SubmissionOperations is the section 1.3.10 operation budget for ONE
	// expression evaluation at submission time.
	//
	// CATASTROPHE BOUND. It is one of the two multiplicands in the derived
	// cumulative-operation ceiling (TemplatePositions x SubmissionOperations)
	// and it scales the measured worst-case wall clock of a single request
	// directly: the worst single request measured is ~17 minutes of server
	// CPU at the default 10,000, because op-cheap byte-heavy work over the
	// ~900 KB SubmissionMemoryBytes allows to be live spends only a few
	// thousand of those 10,000 operations for tens of milliseconds --
	// measured, a regex (re_findall) 3,519 for ~50 ms, a case mapping
	// (.title()) 7,034 for ~57 ms, and that same regex twice inside a
	// comprehension 7,048 for ~103 ms. Doubling this number roughly doubles
	// that floor, which is why internal/config caps it at one order of
	// magnitude above the default rather than leaving it open-ended.
	//
	// No counter measures TIME; the wall-clock backstop
	// (openjd.expr_submission_deadline, default 5s, carried as
	// [ExprLimits.Deadline]) stops an evaluation directly, so RAISING THIS
	// NUMBER IS FAR LESS OF A LEVER ON ELAPSED TIME THAN IT LOOKS: at the
	// default deadline a request is cut off at 5s whatever the ~17-minute
	// figure permits. Raise it because a template's expressions need more
	// operations, not to buy time -- and read the deadline's own limits
	// before concluding the ceiling here is what bounds the worst case.
	SubmissionOperations int64

	// SubmissionMemoryBytes is the section 1.3.9 live-byte budget for ONE
	// expression evaluation at submission time.
	//
	// CATASTROPHE BOUND, with a ceiling of one order of magnitude above the
	// default -- the same shape, and the same reasoning, as
	// SubmissionOperations and TemplatePositions. It is what bounds the live
	// string a single evaluation can hold, and therefore how much byte-heavy,
	// operation-cheap work one position can be asked to do.
	//
	// The ceiling is a judgement, NOT derived from internal/openjd/expr's
	// fixed maxStringBytes (10,000,000) firing first above it: maxStringBytes
	// bounds ONE PRODUCED STRING, while section 1.3.9's meter bounds the SUM
	// of live values and recurses into containers.
	// `["a"*4000000, "b"*4000000, "c"*4000000]` holds 12,000,192 live bytes
	// with no fixed guard firing -- each string is 4 MB, three elements is
	// nowhere near maxElements -- so it is rejected at a 10 MB limit and
	// ACCEPTED at 20 MB. Raising this knob is not inert; it permits
	// proportionally more live memory per evaluation on an unauthenticated
	// path.
	SubmissionMemoryBytes int64

	// TemplatePositions is how many expression positions ONE
	// checkTemplateExpressions walk may check.
	//
	// CATASTROPHE BOUND. It is the other multiplicand in the derived
	// cumulative-operation ceiling, and it is the bound that makes the walk
	// safe to run at all: without it an 84 KB template of ~2,000 args
	// entries costs 11.3 s of CPU per anonymous request, with nothing but the
	// 4 MiB body size bounding it.
	//
	// It also carries a CROSS-BINARY relation the server cannot check on its
	// own: internal/worker/fmtres's assignment-position cap bounds the same
	// quantity on the host, an assignment's positions are a subset of its
	// template's, and a worker cap BELOW this value is reachable by a template
	// this package ACCEPTED -- failing every task in the job after submission
	// instead of failing the one request that could have reported it.
	// TestTemplateBudget_WorkerCapIsNotTighter compares the two DEFAULTS;
	// because both sides are configurable, the relation is also enforced at
	// RUNTIME: workers advertise their caps at registration and
	// internal/scheduler refuses to dispatch an EXPR job to one that is
	// tighter than the value this field carried at acceptance
	// (internal/scheduler/exprcaps.go). That gate does not make a tighter
	// worker safe -- it makes it visibly unused for EXPR work instead of
	// failing every task.
	TemplatePositions int64

	// TemplateRetainedBytes is how many bytes every let: block in the template
	// may cumulatively RETAIN across one walk -- summed over the whole call,
	// not reset per block. Every other position discards its result; let is
	// the only construct that keeps one.
	//
	// POLICY BOUND, and the only one of the four. Its absence does not
	// produce a measured catastrophe on its own: enforcing maxLetBindings at
	// the evaluator makes a single block's cost O(cap) (without it, one
	// construction reached 6.9 GB in 1.45 s), and a block's transient ceiling is
	// already maxLetBindings x SubmissionMemoryBytes regardless of what this
	// number says. What this bounds is the SUM across many
	// individually-compliant blocks, which a template with many steps can
	// legitimately grow -- so an operator with an unusual workload has a real
	// reason to move it, in either direction, and internal/config gives it a
	// wide but finite range rather than a ceiling tied to a measurement.
	TemplateRetainedBytes int64

	// Deadline, when non-zero, is an absolute wall-clock time after which
	// evaluation stops with [expr.ErrDeadlineExceeded].
	//
	// IT IS NOT ONE OF THE FOUR. Unlike every other field here it is
	// per-REQUEST rather than operator configuration -- it is computed at the
	// top of one submission from the configured duration, so two requests
	// arriving a second apart carry different values. It is also not a
	// VALIDITY bound: the four numbers above decide whether a template is
	// INVALID (a 422), while this decides only whether this server keeps
	// working on it (a 503). See [ValidateWithBudget].
	//
	// It lives on this struct because this struct is already the budget bundle
	// threaded to every evaluation, and adding a parameter to
	// [ExprLimits.evalOptions] would touch its ~25 call sites to no benefit.
	// It is deliberately EXEMPT from [ExprLimits.orDefaults]' "<= 0 means
	// unset, use the default" rule, which has no meaning for a time: the zero
	// time means NO deadline, and that is the default.
	//
	// BEING PER-REQUEST, IT MUST NEVER BE SET ON A LONG-LIVED ExprLimits.
	// [SubmitterOptions.ExprLimits] is the trap: it is read once at server boot
	// into a [Submitter] reused for every subsequent submission, so a deadline
	// stored there is a single absolute instant that every later request is
	// measured against -- accepting nothing at all once it passes. See that
	// field's own comment for what the submission path needs instead.
	Deadline time.Time
}

// The range an operator may configure each of the four to. Out-of-range
// values are rejected at STARTUP by internal/config's validateOpenJD, which
// carries its own copies of these numbers (internal/config must not import
// this package) and is pinned to them by internal/server's
// TestExprLimits_ConfigMatchesOpenJD, the one package that may import both.
//
// The CEILINGS distinguish a catastrophe bound (a tight ceiling, tied to a
// measurement) from a policy bound (wide but finite). Which of the four is
// which, and why, is on each [ExprLimits] field. All three catastrophe
// ceilings are one order of magnitude above their default, and all three are
// JUDGEMENTS -- none is derived from some fixed guard firing first (see
// [ExprLimits.SubmissionMemoryBytes]).
//
// The FLOORS all come from measurement, not from taste. The fourteen reference
// presets in presets/sqi/ -- the templates this repo itself ships -- are
// measured per dimension by binary search on every run, by
// TestExprLimits_FloorsAcceptReferencePresets: at most 31 expression positions
// template-wide, and 390 live bytes with 15 operations in any single
// evaluation. Those three maxima are set by ffmpeg-segment-transcode-expr, the
// only preset doing real slice arithmetic; the render presets cost an order of
// magnitude less. Retained bytes are effectively zero for all fourteen -- the
// binary search floors that dimension at 1, so a reported "1" means "nothing
// measurable", not a real cost. Every floor below leaves at least 4x headroom
// over the worst of them (8x to 65536x in practice), so no operator can
// tighten a knob to a value that rejects sqi's own templates.
// TestExprLimits_FloorsAcceptReferencePresets re-measures that on every run
// rather than trusting this paragraph; the figures move as presets are added.
const (
	// MinExprSubmissionOperations / MaxExprSubmissionOperations bound
	// [ExprLimits.SubmissionOperations].
	//
	// Ceiling: exactly one order of magnitude above the default. The
	// worst-case single request measured with this dimension at 10,000 is
	// ~17 minutes of server CPU (~24 if a position spends its whole budget at
	// the worst measured rate), and no COUNTER can see that, so the only
	// accurate statement about a raised value is that it lengthens that floor
	// proportionally -- at both maxima the same construction measures ~40
	// hours. Ten times is a deliberate limit on how much worse an operator can
	// make the worst request the farm can be asked to serve.
	//
	// Those figures are the counter-only cost this ceiling was chosen
	// against; they are not the wall-clock outcome. [ExprLimits.Deadline]
	// stops an evaluation at openjd.expr_submission_deadline (default 5s,
	// ceiling 60s) whatever they permit. The two are NOT co-sized -- 10,000
	// positions x 10,000 operations nominally permits ~17 minutes against a
	// 60s ceiling -- so at any legal configuration the deadline, not this
	// number, is what a request actually stops at.
	//
	// Floor: one order of magnitude below the default so tightening remains
	// meaningful, and ~67x above what a reference preset's expressions
	// actually spend: the most expensive single evaluation in any of the
	// fourteen presets costs 15 operations (see the floors paragraph above).
	MinExprSubmissionOperations int64 = 1_000
	MaxExprSubmissionOperations int64 = 100_000

	// MinExprSubmissionMemoryBytes / MaxExprSubmissionMemoryBytes bound
	// [ExprLimits.SubmissionMemoryBytes].
	//
	// Ceiling: one order of magnitude above the default, a JUDGEMENT, and the
	// same shape as the other two catastrophe ceilings.
	//
	// WHAT THAT CEILING REALLY PERMITS is not 10 MB of live values per
	// evaluation but 50x that. The charge against TemplateRetainedBytes
	// lands ONCE PER let: BLOCK, after that block has finished evaluating,
	// so a single block transiently holds up to maxLetBindings (50, Template
	// Schemas section 3.6) x THIS number before any counter sees it: 50 MB
	// at the default and 500 MB at this ceiling, per concurrently-resolving
	// request. That product, not this number, is what a host's RAM has to be
	// sized against, and it is the reason this ceiling is a tight multiple
	// rather than an open range. See [ExprLimits.TemplateRetainedBytes] and
	// defaultTemplateRetainedBytes' own comment in exprcheck.go, which state
	// the same product from the other end.
	//
	// It coincides with internal/openjd/expr's fixed maxStringBytes, but is
	// not derived from it -- see [ExprLimits.SubmissionMemoryBytes] for the
	// construction that holds 12 MB live with no fixed guard firing. Do not
	// re-derive this ceiling from maxStringBytes; if the argument for 10x ever
	// changes, this number is free to move with it.
	//
	// Floor: 4 KB, ~10x the live bytes a reference preset's largest expression
	// produces (390 bytes; see the floors paragraph above).
	MinExprSubmissionMemoryBytes int64 = 4_096
	MaxExprSubmissionMemoryBytes int64 = 10_000_000

	// MinExprTemplatePositions / MaxExprTemplatePositions bound
	// [ExprLimits.TemplatePositions].
	//
	// Ceiling: one order of magnitude above the default, for the same reason
	// as the operation ceiling — this is the other multiplicand in the
	// derived cumulative-operation ceiling, and raising BOTH to their maxima
	// multiplies that derived number by 100 (10^8 -> 10^10). Documentation
	// must say so in those words.
	//
	// Floor: 256, roughly 8x the largest reference preset's measured 31
	// positions, and still ~40x below the default so an operator running only
	// small templates can tighten hard.
	MinExprTemplatePositions int64 = 256
	MaxExprTemplatePositions int64 = 100_000

	// MinExprTemplateRetainedBytes / MaxExprTemplateRetainedBytes bound
	// [ExprLimits.TemplateRetainedBytes] — the one POLICY bound of the four,
	// so the range is wide rather than tied to a measurement: 64 KB (about a
	// thousand small let bindings; the reference presets use no let: at all
	// and retain zero) up to 100 MB of live values from one request.
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

// Normalized is the exported form of [ExprLimits.orDefaults], for the one
// production caller outside this package that needs the values a walk would ACTUALLY
// enforce rather than the ones a config literal happens to carry:
// internal/scheduler compares them against each worker's advertised EXPR caps
// before dispatching an EXPR job. Comparing an un-normalized zero would
// report a shortfall of "0", i.e. no shortfall at all, in every dimension the
// caller left unset.
func (l ExprLimits) Normalized() ExprLimits { return l.orDefaults() }

// orDefaults returns l with every unset (<= 0) field replaced by its default.
//
// It PATCHES A COPY of l (a value receiver, mutated in place and returned)
// rather than building a fresh ExprLimits field by field, and that is
// deliberate: [ExprLimits.Deadline] has no default and is not a number, so a
// rebuild-style normalization would drop it and leave every deadline test
// passing against templates that happen to finish quickly. Any field added
// here inherits the same pass-through for free; a field that needs a default
// must be added to the list below explicitly.
//
// Applied ONCE, at [newTemplateBudget], so the budget every consumption point
// reads through is already normalized and no call site has to remember to do
// it. A field is treated as unset rather than as "unlimited" because the zero
// value of a struct literal is the overwhelmingly common case here (every
// ValidateOptions{EnforceLimits: ...} in this repo) and "unlimited" is the one
// meaning a resource-exhaustion guard must never acquire by accident.
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
// runs under -- checkFormatString's and checkLetBindings' alike, plus
// resolve.go's expr.Eval call sites. The values come from THIS walk's budget
// rather than from a constant. It is a function rather than a package-level
// slice so each call gets its own slice header.
//
// It also carries [ExprLimits.Deadline] when one is set, which is why that
// field lives on this struct at all: this method is the single point every
// walk position gets its options from, so a per-request backstop threaded here
// reaches all ~25 of them without a signature change.
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
