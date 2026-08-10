// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// EXPR sub-project E4d, Task 1: the four numbers that bound the server-side
// expression checker became operator configuration. These tests exist to catch
// the two failure modes that configuration introduces and that no existing
// test can see:
//
//  1. A DEFAULT that drifts from the constant it replaced, silently changing
//     what a fresh install accepts.
//  2. A knob that is READ but NOT USED -- parsed out of YAML, stored in a
//     struct, and never consulted by the code that enforces the bound. That is
//     invisible to any test that only checks the value round-trips, so every
//     knob below is mutation-tested: set it to something other than the
//     default, and observe the BOUND move.

// ── defaults ────────────────────────────────────────────────────────────────

// TestExprLimits_DefaultsMatchPreE4dConstants pins each default to the literal
// value this package hard-coded before E4d. Deliberately written as literals
// rather than as the constants themselves: comparing defaultTemplatePositions
// to defaultTemplatePositions would pass no matter what either became.
func TestExprLimits_DefaultsMatchPreE4dConstants(t *testing.T) {
	got := DefaultExprLimits()
	tests := []struct {
		name string
		got  int64
		want int64
	}{
		{"SubmissionOperations", got.SubmissionOperations, 10_000},
		{"SubmissionMemoryBytes", got.SubmissionMemoryBytes, 1_000_000},
		{"TemplatePositions", got.TemplatePositions, 10_000},
		{"TemplateRetainedBytes", got.TemplateRetainedBytes, 10_000_000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("DefaultExprLimits().%s = %d, want %d -- the pre-E4d constant. "+
					"A fresh install must behave exactly as every release before E4d did.",
					tc.name, tc.got, tc.want)
			}
		})
	}
}

// TestExprLimits_OrDefaults pins the "zero means unset, never unlimited" rule.
// A resource-exhaustion guard acquiring the meaning "unlimited" by accident is
// the one outcome this normalization exists to prevent.
func TestExprLimits_OrDefaults(t *testing.T) {
	d := DefaultExprLimits()
	tests := []struct {
		name string
		in   ExprLimits
		want ExprLimits
	}{
		{"zero value is entirely defaulted", ExprLimits{}, d},
		{
			name: "negatives are treated as unset, not as unlimited",
			in: ExprLimits{
				SubmissionOperations: -1, SubmissionMemoryBytes: -1,
				TemplatePositions: -1, TemplateRetainedBytes: -1,
			},
			want: d,
		},
		{
			name: "one set field leaves the other three at their defaults",
			in:   ExprLimits{TemplatePositions: 7},
			want: ExprLimits{
				SubmissionOperations:  d.SubmissionOperations,
				SubmissionMemoryBytes: d.SubmissionMemoryBytes,
				TemplatePositions:     7,
				TemplateRetainedBytes: d.TemplateRetainedBytes,
			},
		},
		{
			name: "a fully populated value passes through untouched",
			in:   ExprLimits{SubmissionOperations: 1, SubmissionMemoryBytes: 2, TemplatePositions: 3, TemplateRetainedBytes: 4},
			want: ExprLimits{SubmissionOperations: 1, SubmissionMemoryBytes: 2, TemplatePositions: 3, TemplateRetainedBytes: 4},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.orDefaults(); got != tc.want {
				t.Errorf("orDefaults() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// ── floors, measured against this repo's own templates ──────────────────────

// TestExprLimits_FloorsAcceptReferencePresets is where the FLOORS in
// exprlimits.go come from, and it re-derives them on every run rather than
// asking the reader to trust a comment.
//
// A limit set absurdly low does not fail safe: it rejects legitimate templates
// at submit. The cheapest available definition of "legitimate" is the six
// reference presets this repo itself ships (presets/sqi/*.yaml, the same files
// internal/product/dccpresets_test.go validates), so no floor may be tight
// enough to reject one of them.
//
// The presets do not declare EXPR -- the extension is still StatusInProgress
// -- so the walk is forced on by adding the declaration to the parsed
// template. That measures the right thing anyway: the cost is a property of
// the template's SHAPE (how many format-string-bearing fields it has and what
// they evaluate), not of which extension it declares.
//
// HOW IT ASSERTS, and why it changed in fix round 1: the first version looked
// for the string "template-wide expression budget exceeded", which only two of
// the four dimensions ever produce. A per-EVALUATION trip under the floors
// reports "operation limit exceeded" / "memory limit exceeded" instead and was
// silently ignored, so MinExprSubmissionOperations and
// MinExprSubmissionMemoryBytes were asserted by nothing -- a future preset
// gaining a comprehension or a case mapping would have been rejected by sqi's
// own floor with this test green. It now matches NO message at all. It
// compares the error set produced under the floors against the error set
// produced under the DEFAULTS: any error the floors introduce is a floor-induced
// rejection, whichever dimension caused it. The presets do produce a couple of
// unrelated errors of their own (a TASK_CHUNKING range_expr property access
// that EXPR's checker does not accept), which is exactly why the baseline is a
// diff rather than "expect zero errors".
func TestExprLimits_FloorsAcceptReferencePresets(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "presets", "sqi", "*.yaml"))
	if err != nil {
		t.Fatalf("glob presets: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no reference presets found; this test cannot size a floor against nothing")
	}

	floors := ExprLimits{
		SubmissionOperations:  MinExprSubmissionOperations,
		SubmissionMemoryBytes: MinExprSubmissionMemoryBytes,
		TemplatePositions:     MinExprTemplatePositions,
		TemplateRetainedBytes: MinExprTemplateRetainedBytes,
	}

	// One entry per configurable dimension, so adding a fifth knob without
	// extending this table is a compile-time-visible omission rather than a
	// silent coverage hole.
	dims := []struct {
		name  string
		floor int64
		set   func(*ExprLimits, int64)
	}{
		{"SubmissionOperations", floors.SubmissionOperations, func(l *ExprLimits, v int64) { l.SubmissionOperations = v }},
		{"SubmissionMemoryBytes", floors.SubmissionMemoryBytes, func(l *ExprLimits, v int64) { l.SubmissionMemoryBytes = v }},
		{"TemplatePositions", floors.TemplatePositions, func(l *ExprLimits, v int64) { l.TemplatePositions = v }},
		{"TemplateRetainedBytes", floors.TemplateRetainedBytes, func(l *ExprLimits, v int64) { l.TemplateRetainedBytes = v }},
	}

	for _, p := range paths {
		t.Run(filepath.Base(p), func(t *testing.T) {
			load := func() *JobTemplate {
				tmpl := parsePresetTemplate(t, p)
				tmpl.Extensions = append(tmpl.Extensions, "EXPR")
				return tmpl
			}
			walk := func(lim ExprLimits) (map[string]bool, *templateBudget) {
				b := newTemplateBudget(lim)
				return errorSet(checkTemplateExpressions(load(), nil, b)), b
			}

			// The baseline: whatever this preset reports with nothing
			// tightened. Its content is deliberately not asserted -- only that
			// the floors add nothing to it.
			baseline, defaultBudget := walk(DefaultExprLimits())

			t.Run("all four floors together", func(t *testing.T) {
				got, _ := walk(floors)
				for msg := range got {
					if !baseline[msg] {
						t.Errorf("the configurable FLOORS reject one of sqi's own reference presets: %s", msg)
					}
				}
			})

			// Per dimension, the smallest value at which this preset still
			// produces exactly its baseline errors -- the preset's real cost in
			// that dimension. Reported alongside the floor so a preset that
			// grows toward one is visible in the log, and asserted with 4x
			// headroom so it fails BEFORE a preset can reach the floor.
			for _, d := range dims {
				t.Run(d.name, func(t *testing.T) {
					need := minimalLimit(baseline, load, d.set, d.floor*4)
					if need > d.floor {
						t.Fatalf("this preset needs %s >= %d, above the configurable floor of %d: "+
							"the floor rejects sqi's own template", d.name, need, d.floor)
					}
					t.Logf("needs %d, floor %d (%.0fx headroom)", need, d.floor, float64(d.floor)/float64(max64(need, 1)))
					if need*4 > d.floor {
						t.Errorf("this preset needs %s >= %d against a floor of %d -- less than 4x headroom. "+
							"Raise Min%s%s rather than shipping a floor a real template can approach.",
							d.name, need, d.floor, "Expr", d.name)
					}
				})
			}

			t.Logf("budget cost at the defaults: positions=%d, retained bytes=%d",
				defaultBudget.positions, defaultBudget.retained)
		})
	}
}

// minimalLimit binary-searches the smallest value of ONE dimension at which
// the walk still produces exactly baseline -- i.e. the template's real cost in
// that dimension. The search is valid because every one of these limits is
// monotone: raising it can only remove errors, never add them. hi is an upper
// bound the caller believes is sufficient; if even hi is not, hi+1 is returned
// so the caller reports "needs more than the floor" rather than looping.
func minimalLimit(baseline map[string]bool, load func() *JobTemplate, set func(*ExprLimits, int64), hi int64) int64 {
	ok := func(v int64) bool {
		lim := DefaultExprLimits()
		set(&lim, v)
		got := errorSet(checkTemplateExpressions(load(), nil, newTemplateBudget(lim)))
		if len(got) != len(baseline) {
			return false
		}
		for msg := range got {
			if !baseline[msg] {
				return false
			}
		}
		return true
	}
	if !ok(hi) {
		return hi + 1
	}
	lo := int64(1)
	for lo < hi {
		mid := lo + (hi-lo)/2
		if ok(mid) {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}

func errorSet(errs ValidationErrors) map[string]bool {
	out := make(map[string]bool, len(errs))
	for _, e := range errs {
		out[e.Pointer+": "+e.Message] = true
	}
	return out
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// parsePresetTemplate reads one presets/sqi definition file and returns its
// embedded OpenJD job template. The definition wrapper (name/title/category)
// belongs to internal/product, which this package must not import, so only the
// `template:` node is decoded here.
func parsePresetTemplate(t *testing.T, path string) *JobTemplate {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var df struct {
		Template yaml.Node `yaml:"template"`
	}
	if err := yaml.Unmarshal(raw, &df); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	body, err := yaml.Marshal(&df.Template)
	if err != nil {
		t.Fatalf("re-marshal %s: %v", path, err)
	}
	tmpl, err := Parse(body, FormatYAML)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return tmpl
}

// ── mutation: each knob, observed separately ────────────────────────────────

// argsTemplate builds a template whose only expression positions are /name,
// the command, and n args entries -- so its total position cost is exactly
// n+2. Every entry is trivial, so nothing but the POSITION dimension can trip.
func argsTemplate(n int) *JobTemplate {
	return &JobTemplate{
		Name:       "{{ 'T' }}",
		Extensions: []string{"EXPR"},
		Steps: []StepTemplate{{
			Name: "Step1",
			Script: &StepScript{Actions: StepActions{OnRun: Action{
				Command: "echo",
				Args:    manyArgs(n),
				ArgsSet: true,
			}}},
		}},
	}
}

// TestExprLimits_TemplatePositionsIsObserved is the position knob's mutation
// test, and it is written at the exact boundary on purpose: chargePositions
// rejects when the running total EXCEEDS the limit, so a construction costing
// exactly N positions must be accepted at N and rejected at N-1. A test that
// only compared 10 against 10^9 would pass with an off-by-one.
func TestExprLimits_TemplatePositionsIsObserved(t *testing.T) {
	const args = 300
	const cost = args + 2 // /name + /command + args entries

	t.Run("cost is what this test assumes", func(t *testing.T) {
		b := newTemplateBudget(ExprLimits{})
		_ = checkTemplateExpressions(argsTemplate(args), nil, b) //nolint:errcheck // the verdict is irrelevant; this sub-test measures the budget's position count
		if b.positions != cost {
			t.Fatalf("this construction costs %d positions, not the %d the boundary rows below assume",
				b.positions, cost)
		}
	})

	tests := []struct {
		name   string
		limit  int64
		reject bool
	}{
		{"exactly at the cost", cost, false},
		{"one below the cost", cost - 1, true},
		{"far above the cost", cost * 10, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := newTemplateBudget(ExprLimits{TemplatePositions: tc.limit})
			errs := checkTemplateExpressions(argsTemplate(args), nil, b)
			msg, tripped := budgetError(errs)
			switch {
			case tc.reject && !tripped:
				t.Fatalf("TemplatePositions=%d must reject a %d-position template; got %v", tc.limit, cost, errs)
			case !tc.reject && tripped:
				t.Fatalf("TemplatePositions=%d must accept a %d-position template; got %q", tc.limit, cost, msg)
			case !tc.reject:
				return
			}
			if !strings.Contains(msg, "expression positions") ||
				!strings.Contains(msg, strconv.FormatInt(tc.limit, 10)) {
				t.Errorf("the error must name the positions dimension and the CONFIGURED limit %d; got %q",
					tc.limit, msg)
			}
		})
	}
}

// TestExprLimits_TemplateRetainedBytesIsObserved is the retained-bytes knob's
// mutation test. It moves the bound in BOTH directions around one fixed
// construction: a template the default accepts is rejected once the knob is
// lowered beneath it, and a template the default rejects is accepted once the
// knob is raised above it.
func TestExprLimits_TemplateRetainedBytesIsObserved(t *testing.T) {
	// Three bindings of 900,000 bytes each (+64-byte header, expr.SizeOf) =
	// 2,700,192 retained bytes -- the same shape
	// TestCheckTemplateExpressions_TemplateWideBudget_RetainedBytesDimension
	// uses, sized to sit between the two limits below.
	const bindings = 3
	const bytesPerBinding = 900_000
	const retained = bindings * (bytesPerBinding + 64)

	lets := make([]string, bindings)
	for i := range lets {
		lets[i] = fmt.Sprintf("a%d = \"x\" * %d", i, bytesPerBinding)
	}
	build := func() *JobTemplate {
		return &JobTemplate{
			Name:       "T",
			Extensions: []string{"EXPR"},
			Steps:      []StepTemplate{{Name: "Step1", Let: lets, LetSet: true}},
		}
	}

	tests := []struct {
		name   string
		limit  int64
		reject bool
	}{
		{"default accepts it", DefaultExprLimits().TemplateRetainedBytes, false},
		{"exactly at the retained size", retained, false},
		{"one byte below", retained - 1, true},
		{"tightened well below", 1_000_000, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := newTemplateBudget(ExprLimits{TemplateRetainedBytes: tc.limit})
			errs := checkTemplateExpressions(build(), nil, b)
			msg, tripped := budgetError(errs)
			switch {
			case tc.reject && !tripped:
				t.Fatalf("TemplateRetainedBytes=%d must reject a %d-byte template; got %v", tc.limit, retained, errs)
			case !tc.reject && tripped:
				t.Fatalf("TemplateRetainedBytes=%d must accept a %d-byte template; got %q", tc.limit, retained, msg)
			case !tc.reject:
				return
			}
			if !strings.Contains(msg, "let bindings may retain") ||
				!strings.Contains(msg, strconv.FormatInt(tc.limit, 10)) {
				t.Errorf("the error must name the retained-bytes dimension and the CONFIGURED limit %d; got %q",
					tc.limit, msg)
			}
		})
	}
}

// TestExprLimits_SubmissionMemoryBytesIsObserved is the per-evaluation memory
// knob's mutation test. The construction allocates ~1.5 MB in ONE expression:
// the default (1 MB) rejects it, a raised limit accepts it, and a lowered one
// still rejects it -- so the observed bound tracks the knob in both
// directions, which no "the value parses" test could show.
func TestExprLimits_SubmissionMemoryBytesIsObserved(t *testing.T) {
	build := func() *JobTemplate {
		return &JobTemplate{
			Name:       "{{ 'a' * 1500000 }}",
			Extensions: []string{"EXPR"},
			Steps:      []StepTemplate{{Name: "Step1"}},
		}
	}

	tests := []struct {
		name   string
		limit  int64
		reject bool
	}{
		{"default rejects 1.5MB", DefaultExprLimits().SubmissionMemoryBytes, true},
		{"raised to 5MB accepts it", 5_000_000, false},
		{"tightened to 100KB still rejects", 100_000, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := newTemplateBudget(ExprLimits{SubmissionMemoryBytes: tc.limit})
			errs := checkTemplateExpressions(build(), nil, b)
			hit := containsMessage(errs, "bytes of live values")
			if hit != tc.reject {
				t.Fatalf("SubmissionMemoryBytes=%d: memory limit hit = %v, want %v; errs=%v",
					tc.limit, hit, tc.reject, errs)
			}
			if tc.reject && !containsMessage(errs, strconv.FormatInt(tc.limit, 10)) {
				t.Errorf("the error must name the CONFIGURED limit %d; got %v", tc.limit, errs)
			}
		})
	}
}

// TestExprLimits_SubmissionOperationsIsObserved is the per-evaluation
// operation knob's mutation test. The construction is deliberately BYTE-CHEAP
// (a comprehension over a range, producing small integers) so that only the
// operation dimension can be what trips: if lowering this knob did nothing,
// the "tightened" rows below would pass validation.
func TestExprLimits_SubmissionOperationsIsObserved(t *testing.T) {
	build := func() *JobTemplate {
		return &JobTemplate{
			Name:       "{{ str(len([i for i in range(2000)])) }}",
			Extensions: []string{"EXPR"},
			Steps:      []StepTemplate{{Name: "Step1"}},
		}
	}

	tests := []struct {
		name   string
		limit  int64
		reject bool
	}{
		{"default accepts it", DefaultExprLimits().SubmissionOperations, false},
		{"tightened to the configurable floor rejects it", MinExprSubmissionOperations, true},
		{"tightened hard rejects it", 100, true},
		{"raised to the configurable ceiling accepts it", MaxExprSubmissionOperations, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := newTemplateBudget(ExprLimits{SubmissionOperations: tc.limit})
			errs := checkTemplateExpressions(build(), nil, b)
			hit := containsMessage(errs, "operation")
			if hit != tc.reject {
				t.Fatalf("SubmissionOperations=%d: operation limit hit = %v, want %v; errs=%v",
					tc.limit, hit, tc.reject, errs)
			}
			if tc.reject && !containsMessage(errs, strconv.FormatInt(tc.limit, 10)) {
				t.Errorf("the error must name the CONFIGURED limit %d; got %v", tc.limit, errs)
			}
		})
	}
}

// TestExprLimits_KnobsAreIndependent proves the four are four, not one read
// four times: tightening any one of them to its configurable floor must not
// change the verdict on a template bounded by a DIFFERENT dimension.
func TestExprLimits_KnobsAreIndependent(t *testing.T) {
	// Costs 302 positions, retains nothing, and every evaluation is trivial:
	// only TemplatePositions can reject it.
	const args = 300
	const cost = args + 2

	tests := []struct {
		name   string
		limits ExprLimits
		reject bool
	}{
		{"positions at the cost, everything else floored", ExprLimits{
			TemplatePositions:     cost,
			SubmissionOperations:  MinExprSubmissionOperations,
			SubmissionMemoryBytes: MinExprSubmissionMemoryBytes,
			TemplateRetainedBytes: MinExprTemplateRetainedBytes,
		}, false},
		{"only positions tightened", ExprLimits{TemplatePositions: cost - 1}, true},
		{"only operations floored", ExprLimits{SubmissionOperations: MinExprSubmissionOperations}, false},
		{"only memory floored", ExprLimits{SubmissionMemoryBytes: MinExprSubmissionMemoryBytes}, false},
		{"only retained bytes floored", ExprLimits{TemplateRetainedBytes: MinExprTemplateRetainedBytes}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := checkTemplateExpressions(argsTemplate(args), nil, newTemplateBudget(tc.limits))
			if got := len(errs) > 0; got != tc.reject {
				t.Fatalf("rejected = %v, want %v; errs=%v", got, tc.reject, errs)
			}
		})
	}
}

// ── the carriers ────────────────────────────────────────────────────────────

// TestValidateWithOptions_ExprLimitsReachTheWalk closes the gap between "the
// budget observes the limit" (every test above) and "the limit an OPERATOR set
// reaches the budget". ValidateWithOptions is the carrier this task chose --
// see the task report -- and this is the test that fails if it stops threading
// the value through.
//
// CheckEXPRExpressionsWhileUnsupported is required because EXPR is
// StatusInProgress: without it the walk does not run at all, and this test
// would pass on a completely unwired knob.
func TestValidateWithOptions_ExprLimitsReachTheWalk(t *testing.T) {
	const args = 300
	const cost = args + 2
	tmpl := argsTemplate(args)
	tmpl.SpecificationVersion = SpecVersion

	tests := []struct {
		name   string
		limits ExprLimits
		reject bool
	}{
		{"zero value behaves as the pre-E4d default", ExprLimits{}, false},
		{"explicit default", DefaultExprLimits(), false},
		{"tightened below the template's cost", ExprLimits{TemplatePositions: cost - 1}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := ValidateWithOptions(tmpl, ValidateOptions{
				EnforceLimits:                        true,
				CheckEXPRExpressionsWhileUnsupported: true,
				ExprLimits:                           tc.limits,
			})
			_, tripped := budgetError(errs)
			if tripped != tc.reject {
				t.Fatalf("ValidateOptions.ExprLimits=%+v: budget tripped = %v, want %v; errs=%v",
					tc.limits, tripped, tc.reject, errs)
			}
		})
	}
}

// TestSubmit_ExprLimitsAreEnforcedThroughTheSubmitter is the submit
// pipeline's carrier test, end to end through the real [Submitter.Submit].
//
// FIX ROUND 1 REPLACED A MUCH WEAKER TEST HERE, and the mistake is worth
// recording because this very file's sibling (submit_exprcheck_test.go) warns
// against it in its own header. The first version asserted only that
// NewSubmitterWithOptions stored the struct and that newTemplateBudget
// normalized it; it never called prepareTemplate, so reverting either budget
// in submit.go to newTemplateBudget(ExprLimits{}) -- or dropping ExprLimits
// from the phase-1 ValidateOptions -- left it green. The report described it
// as pinning something it did not pin. It also claimed a real end-to-end test
// was impossible while EXPR is StatusInProgress; that is false, and
// TestSubmit_PhaseDistinction_ThroughRealSubmit had already shown the way --
// temporarily flip the registry entry to StatusSupported, exactly as it does.
//
// The three wiring points and what covers each:
//
//   - PHASE 1 (submit.go's ValidateOptions.ExprLimits): the "rejects at phase 1,
//     before parameter binding" sub-test. The plain "positions tightened"
//     sub-test does NOT pin it -- phase 2 re-walks the identical positions
//     under the identical limits and masks it -- so the isolating sub-test
//     submits with a required parameter missing, putting the binding failure
//     between phase 1 and phase 2.
//   - PHASE 2 (prepareTemplate's checkerBudget): the "memory at phase 2 only"
//     sub-test. It uses the same phase-distinction mechanism as
//     TestSubmit_PhaseDistinction_ThroughRealSubmit: an unresolved Param
//     placeholder costs almost nothing at phase 1, while the same expression
//     over a CONCRETE 300,000-byte value costs 300,064 live bytes at phase 2
//     (measured -- the meter does not hold the bound value and the .upper()
//     copy simultaneously, so it is not the doubling an earlier draft of this
//     comment assumed). Tightened to 200,000, phase 1 passes and phase 2
//     rejects -- so only the phase-2 budget can be what refused it.
//   - The RESOLVER budget (prepareTemplate's resolverBudget): the
//     "resolver budget carries them" sub-test, which calls prepareTemplate
//     directly and inspects the budget it returns. This one is NOT observable
//     through Submit's verdict, and that is a property of the design rather
//     than a gap in the test: the checker walks the same range positions
//     first, under the same limits, so anything that would exhaust the
//     resolver's allowance exhausts the checker's one walk earlier. Inspecting
//     the returned budget is the strongest available observation, and it does
//     fail if submit.go stops passing s.exprLimits there.
func TestSubmit_ExprLimitsAreEnforcedThroughTheSubmitter(t *testing.T) {
	prevEXPR := registry["EXPR"]
	supported := prevEXPR
	supported.Status = StatusSupported
	registry["EXPR"] = supported
	t.Cleanup(func() { registry["EXPR"] = prevEXPR })

	const tmplYAML = `
specificationVersion: jobtemplate-2023-09
extensions:
- EXPR
name: ExprLimitsJob
parameterDefinitions:
- name: S
  type: STRING
steps:
- name: Step1
  script:
    actions:
      onRun:
        command: echo
        args:
        - "{{ Param.S.upper() }}"
`
	// /name + /command + one args entry.
	const positionCost = 3
	bigParam := map[string]string{"S": strings.Repeat("a", 300_000)}

	submitWith := func(t *testing.T, lim ExprLimits, params map[string]string) error {
		t.Helper()
		ctx := context.Background()
		st := fake.New()
		farm, err := st.CreateFarm(ctx, store.Farm{ID: uuid.NewString(), Name: "expr-limits-farm"})
		if err != nil {
			t.Fatalf("CreateFarm: %v", err)
		}
		queue, err := st.CreateQueue(ctx, store.Queue{ID: uuid.NewString(), FarmID: farm.ID, Name: "expr-limits-queue"})
		if err != nil {
			t.Fatalf("CreateQueue: %v", err)
		}
		sub := NewSubmitterWithOptions(st, SubmitterOptions{EnforceLimits: true, ExprLimits: lim})
		_, err = sub.Submit(ctx, tmplYAML, store.TemplateFormatYAML, SubmitOptions{
			FarmID: farm.ID, QueueID: queue.ID, Parameters: params,
		})
		return err
	}

	t.Run("defaults accept it", func(t *testing.T) {
		if err := submitWith(t, ExprLimits{}, bigParam); err != nil {
			t.Fatalf("the pre-E4d defaults must accept this submission: %v", err)
		}
	})

	t.Run("positions tightened below the template's cost", func(t *testing.T) {
		err := submitWith(t, ExprLimits{TemplatePositions: positionCost - 1}, bigParam)
		if err == nil {
			t.Fatal("TemplatePositions below the template's cost must make Submit fail")
		}
		if !strings.Contains(err.Error(), "template-wide expression budget exceeded") {
			t.Fatalf("want the budget's own error; got %v", err)
		}
	})

	// PHASE 1 IN ISOLATION. The sub-test above does not actually pin phase 1:
	// phase 2 re-walks the identical positions under the identical limits, so
	// dropping ExprLimits from prepareTemplate's ValidateOptions leaves it
	// green (verified by mutation). The distinguisher is ORDER -- prepareTemplate
	// runs phase 1, then storage-location checks, then PARAMETER BINDING, then
	// phase 2. Submitting with no value for a required parameter makes binding
	// fail, so phase 2 is never reached: if the rejection still names the
	// budget, only phase 1 can have produced it, and if phase 1 is running at
	// the defaults the error is the binding failure instead.
	t.Run("positions tightened rejects at phase 1, before parameter binding", func(t *testing.T) {
		err := submitWith(t, ExprLimits{TemplatePositions: positionCost - 1}, nil)
		if err == nil {
			t.Fatal("expected a rejection")
		}
		if !strings.Contains(err.Error(), "template-wide expression budget exceeded") {
			t.Fatalf("phase 1 must reject on the CONFIGURED budget before parameter binding is attempted; got %v", err)
		}
		if strings.Contains(err.Error(), "parameter binding") {
			t.Fatalf("the walk reached parameter binding, so phase 1 was not running under the configured limit: %v", err)
		}
	})

	t.Run("memory tightened trips at phase 2 only", func(t *testing.T) {
		lim := ExprLimits{SubmissionMemoryBytes: 200_000}

		// Phase 1 sees Param.S as an unresolved placeholder and costs almost
		// nothing, so a submission with a SMALL value must still succeed --
		// this is what proves the rejection below comes from phase 2 and not
		// from the phase-1 walk under the same tightened limit.
		if err := submitWith(t, lim, map[string]string{"S": "small"}); err != nil {
			t.Fatalf("a small parameter must still pass under the tightened limit: %v", err)
		}

		err := submitWith(t, lim, bigParam)
		if err == nil {
			t.Fatal("a 300,000-byte parameter must exceed a 200,000-byte per-evaluation limit at phase 2")
		}
		if !strings.Contains(err.Error(), "bytes of live values") ||
			!strings.Contains(err.Error(), "200000") {
			t.Fatalf("want the memory limit's own error naming the CONFIGURED 200000; got %v", err)
		}
	})

	t.Run("the resolver budget carries them", func(t *testing.T) {
		want := ExprLimits{
			SubmissionOperations:  1_234,
			SubmissionMemoryBytes: 5_678_000,
			TemplatePositions:     910,
			TemplateRetainedBytes: 111_213,
		}
		sub := NewSubmitterWithOptions(fake.New(), SubmitterOptions{EnforceLimits: true, ExprLimits: want})
		_, _, resolverBudget, err := sub.prepareTemplate(
			context.Background(), tmplYAML, store.TemplateFormatYAML, map[string]string{"S": "small"},
		)
		if err != nil {
			t.Fatalf("prepareTemplate: %v", err)
		}
		if resolverBudget.limits != want {
			t.Fatalf("prepareTemplate's resolver budget carries %+v, want %+v", resolverBudget.limits, want)
		}
	})

	t.Run("NewSubmitter defaults", func(t *testing.T) {
		sub := NewSubmitter(fake.New())
		_, _, resolverBudget, err := sub.prepareTemplate(
			context.Background(), tmplYAML, store.TemplateFormatYAML, map[string]string{"S": "small"},
		)
		if err != nil {
			t.Fatalf("prepareTemplate: %v", err)
		}
		if resolverBudget.limits != DefaultExprLimits() {
			t.Fatalf("NewSubmitter must default to %+v, got %+v", DefaultExprLimits(), resolverBudget.limits)
		}
	})
}

// ── helpers ─────────────────────────────────────────────────────────────────

// budgetError returns the message of the first template-wide budget error in
// errs, and whether there was one. Other validation errors (an unsupported
// extension, a scope failure) are deliberately not counted: these tests are
// about the budget, and a template that is rejected for an unrelated reason
// must not read as a budget trip.
func budgetError(errs ValidationErrors) (string, bool) {
	for _, e := range errs {
		if strings.Contains(e.Message, "template-wide expression budget exceeded") {
			return e.Message, true
		}
	}
	return "", false
}

func containsMessage(errs ValidationErrors, substr string) bool {
	for _, e := range errs {
		if strings.Contains(e.Message, substr) {
			return true
		}
	}
	return false
}
