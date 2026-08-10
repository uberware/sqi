// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import "github.com/uberware/sqi/internal/openjd/expr"

// ─── operator-configurable expression limits ────────────────────────────────

// ExprLimits is the server side of EXPR sub-project E4d: the four numbers that
// bound what one submitted template may spend inside the EXPR expression
// checker, lifted out of package constants so an operator can size them.
//
// The four split into two per-EVALUATION bounds (SubmissionOperations,
// SubmissionMemoryBytes -- what ONE expression position may cost, section
// 1.3.10 and section 1.3.9 respectively) and two per-WALK bounds
// (TemplatePositions, TemplateRetainedBytes -- what the WHOLE template may
// cost across one checkTemplateExpressions call). See each field's own
// comment, and the long rationale blocks in exprcheck.go that each field's
// DEFAULT still carries.
//
// A ZERO field means "unset, use the default" -- see [ExprLimits.orDefaults].
// That is deliberate and it is what keeps every existing
// ValidateOptions{EnforceLimits: ...} literal in this repo (and every caller
// that has no operator configuration to offer, e.g. internal/product's
// ValidateTemplate) behaving byte for byte as it did before this type
// existed. It also means a NEGATIVE value is not silently accepted as
// "unlimited": orDefaults treats anything <= 0 as unset, and
// internal/config's validateOpenJD rejects an out-of-range configured value
// at startup long before one could reach here.
//
// The ranges an OPERATOR may choose from are NOT enforced here -- they live in
// internal/config (MinOpenJDExpr*/MaxOpenJDExpr*) and are rejected at load,
// because internal/config is the layer that owns operator-facing policy and
// this package must stay importable from it-free code. This type accepts any
// positive value so that a test can deliberately set an absurd one to prove a
// knob is wired (which is exactly how this wave's mutation tests work).
type ExprLimits struct {
	// SubmissionOperations is the section 1.3.10 operation budget for ONE
	// expression evaluation at submission time.
	//
	// CATASTROPHE BOUND. It is one of the two multiplicands in the derived
	// cumulative-operation ceiling (TemplatePositions x SubmissionOperations)
	// and it scales the measured worst-case wall clock of a single request
	// directly: E4c measured ~9.5 minutes of server CPU for one 4 MiB body at
	// the default 10,000, because op-cheap byte-heavy work (a regex or a case
	// mapping over the ~900 KB SubmissionMemoryBytes allows to be live) spends
	// ~3,500 of those 10,000 operations for ~50 ms of CPU. Doubling this
	// number roughly doubles that floor. Nothing in this package measures
	// TIME, so the only lever on the worst case is this count and the position
	// count -- which is why internal/config caps it at one order of magnitude
	// above the default rather than leaving it open-ended.
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
	// AN EARLIER REVISION OF THIS COMMENT CLAIMED THE CEILING WAS NOT A
	// JUDGEMENT CALL -- that internal/openjd/expr's fixed maxStringBytes
	// (10,000,000) would fire first above it, making a larger value inert.
	// THAT IS FALSE, and it was disproved by measurement in E4d Task 1's
	// review: maxStringBytes bounds ONE PRODUCED STRING, while section
	// 1.3.9's meter bounds the SUM of live values and recurses into
	// containers. `["a"*4000000, "b"*4000000, "c"*4000000]` holds 12,000,192
	// live bytes with no fixed guard firing -- each string is 4 MB, three
	// elements is nowhere near maxElements -- so it is rejected at a 10 MB
	// limit and ACCEPTED at 20 MB. Raising this knob is not inert; it permits
	// proportionally more live memory per evaluation on an unauthenticated
	// path. The number is unchanged; only the reason for it is, and the
	// reason had to change because a false one invites exactly the raise it
	// claimed was harmless.
	SubmissionMemoryBytes int64

	// TemplatePositions is how many expression positions ONE
	// checkTemplateExpressions walk may check.
	//
	// CATASTROPHE BOUND. It is the other multiplicand in the derived
	// cumulative-operation ceiling, and it is the bound that made the walk
	// safe to run at all: before it existed an 84 KB template of ~2,000 args
	// entries cost 11.3 s of CPU per anonymous request, with nothing but the
	// 4 MiB body size bounding it.
	//
	// It also carries a CROSS-BINARY relation the server cannot check on its
	// own: internal/worker/fmtres's MaxAssignmentPositions bounds the same
	// quantity on the host, an assignment's positions are a subset of its
	// template's, and a worker cap BELOW this value is reachable by a template
	// this package ACCEPTED -- failing every task in the job after submission
	// instead of failing the one request that could have reported it. E4c
	// pinned that with TestTemplateBudget_WorkerCapIsNotTighter comparing two
	// CONSTANTS; making both sides configurable made the relation breakable
	// through YAML, which no compile-time test can see. E4d Task 3 closed that
	// at RUNTIME rather than here: workers advertise their caps at
	// registration and internal/scheduler refuses to dispatch an EXPR job to
	// one that is tighter than the value this field carried at acceptance
	// (internal/scheduler/exprcaps.go). Nothing about that gate makes a
	// tighter worker safe -- it makes it visibly unused for EXPR work instead
	// of silently failing every task.
	TemplatePositions int64

	// TemplateRetainedBytes is how many bytes every let: block in the template
	// may cumulatively RETAIN across one walk -- summed over the whole call,
	// not reset per block. Every other position discards its result; let is
	// the only construct that keeps one.
	//
	// POLICY BOUND, and the only one of the four. Its absence did not produce
	// a measured catastrophe on its own: E3's 6.9 GB / 1.45 s construction was
	// closed by enforcing maxLetBindings at the evaluator (making a single
	// block's cost O(cap)), and a block's transient ceiling is already
	// maxLetBindings x SubmissionMemoryBytes regardless of what this number
	// says. What this bounds is the SUM across many individually-compliant
	// blocks, which a template with many steps can legitimately grow -- so an
	// operator with an unusual workload has a real reason to move it, in
	// either direction, and internal/config gives it a wide but finite range
	// rather than a ceiling tied to a measurement.
	TemplateRetainedBytes int64
}

// The range an operator may configure each of the four to. Out-of-range
// values are rejected at STARTUP by internal/config's validateOpenJD, which
// carries its own copies of these numbers (internal/config must not import
// this package -- CLAUDE.md's dependency direction) and is pinned to them by
// internal/server's TestExprLimits_ConfigMatchesOpenJD, the one package that
// may import both.
//
// The CEILINGS encode design spec §3's distinction between a catastrophe
// bound (a tight ceiling, tied to a measurement) and a policy bound (wide but
// finite). Which of the four is which, and why, is on each [ExprLimits] field.
// All three catastrophe ceilings are one order of magnitude above their
// default, and all three are JUDGEMENTS -- none is derived from some fixed
// guard firing first, which is a derivation this file offered once and had to
// withdraw (see [ExprLimits.SubmissionMemoryBytes]).
//
// The FLOORS all come from measurement, not from taste. The six reference
// presets in presets/sqi/ -- the templates this repo itself ships -- were
// measured per dimension by binary search: they cost at most 15 expression
// positions, retain 0 let bytes, need at most 64 live bytes and at most 1
// operation in any single evaluation. Every floor below leaves at least 4x
// headroom over the worst of them (17x to 65536x in practice), so no operator
// can tighten a knob to a value that rejects sqi's own templates.
// TestExprLimits_FloorsAcceptReferencePresets re-measures that on every run
// rather than trusting this paragraph.
const (
	// MinExprSubmissionOperations / MaxExprSubmissionOperations bound
	// [ExprLimits.SubmissionOperations].
	//
	// Ceiling: exactly one order of magnitude above the default. E4c measured
	// the worst-case single request at ~9.5 minutes of server CPU with this
	// dimension at 10,000, and nothing in this package measures time, so the
	// only honest statement about a raised value is that it lengthens that
	// floor proportionally. Ten times is a deliberate limit on how much worse
	// an operator can make the worst request the farm can be asked to serve.
	//
	// Floor: two orders of magnitude above the handful of operations a
	// reference preset's expressions actually spend, and one order of
	// magnitude below the default so tightening remains meaningful.
	MinExprSubmissionOperations int64 = 1_000
	MaxExprSubmissionOperations int64 = 100_000

	// MinExprSubmissionMemoryBytes / MaxExprSubmissionMemoryBytes bound
	// [ExprLimits.SubmissionMemoryBytes].
	//
	// Ceiling: one order of magnitude above the default, a JUDGEMENT, and the
	// same shape as the other two catastrophe ceilings -- 10 MB of live values
	// per evaluation is as much as an operator may permit an unauthenticated
	// submission to hold.
	//
	// It coincides with internal/openjd/expr's fixed maxStringBytes, and an
	// earlier revision mistook the coincidence for a derivation ("above it the
	// fixed guard fires first, so a larger value is inert"). Measurement
	// disproved that -- see [ExprLimits.SubmissionMemoryBytes] for the
	// construction, which holds 12 MB live with no fixed guard firing. Do not
	// re-derive this ceiling from maxStringBytes; if the argument for 10x ever
	// changes, this number is free to move with it.
	//
	// Floor: 4 KB, three orders of magnitude above the few hundred live bytes
	// a reference preset's largest expression produces.
	MinExprSubmissionMemoryBytes int64 = 4_096
	MaxExprSubmissionMemoryBytes int64 = 10_000_000

	// MinExprTemplatePositions / MaxExprTemplatePositions bound
	// [ExprLimits.TemplatePositions].
	//
	// Ceiling: one order of magnitude above the default, for the same reason
	// as the operation ceiling — this is the other multiplicand in the
	// derived cumulative-operation ceiling, and raising BOTH to their maxima
	// multiplies that derived number by 100 (10^8 -> 10^10). Documentation
	// must say so in those words; see design spec §4 caveat 1.
	//
	// Floor: 256, roughly 17x the largest reference preset's measured 15
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

// DefaultExprLimits returns the four values this package used as fixed
// constants before E4d, and which internal/config's OpenJDConfig defaults to.
// A server started with no expression-limit configuration behaves exactly as
// every release before E4d did.
func DefaultExprLimits() ExprLimits {
	return ExprLimits{
		SubmissionOperations:  defaultSubmissionOperations,
		SubmissionMemoryBytes: defaultSubmissionMemoryBytes,
		TemplatePositions:     defaultTemplatePositions,
		TemplateRetainedBytes: defaultTemplateRetainedBytes,
	}
}

// Normalized is the exported form of [ExprLimits.orDefaults], for the one
// caller outside this package that needs the values a walk would ACTUALLY
// enforce rather than the ones a config literal happens to carry:
// internal/scheduler compares them against each worker's advertised EXPR caps
// before dispatching an EXPR job (EXPR sub-project E4d Task 3). Comparing an
// un-normalized zero would report a shortfall of "0", i.e. no shortfall at
// all, in every dimension the caller left unset.
func (l ExprLimits) Normalized() ExprLimits { return l.orDefaults() }

// orDefaults returns l with every unset (<= 0) field replaced by its default.
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
// resolve.go's three expr.Eval call sites, through the identical opts tail
// they all take. Before E4d this was a package-level submissionLimits()
// function returning two fixed constants; making it a METHOD is the change --
// the values now come from THIS walk's budget rather than from a constant. It
// stays a function rather than a package-level slice so each call gets its own
// slice header.
func (l ExprLimits) evalOptions() []expr.Option {
	return []expr.Option{
		expr.WithOperationLimit(l.SubmissionOperations),
		expr.WithMemoryLimit(l.SubmissionMemoryBytes),
	}
}
