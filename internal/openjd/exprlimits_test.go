// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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
// template. That measures the right thing anyway: the position count is a
// property of the template's SHAPE (how many format-string-bearing fields it
// has), not of which extension it declares.
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

	for _, p := range paths {
		t.Run(filepath.Base(p), func(t *testing.T) {
			tmpl := parsePresetTemplate(t, p)
			tmpl.Extensions = append(tmpl.Extensions, "EXPR")

			b := newTemplateBudget(floors)
			errs := checkTemplateExpressions(tmpl, nil, b)

			for _, e := range errs {
				if strings.Contains(e.Message, "template-wide expression budget exceeded") {
					t.Fatalf("the configurable FLOOR rejects one of sqi's own reference presets: %v", e)
				}
			}
			// Recorded, not asserted against a magic number: this is the
			// measurement the floors were sized from, and printing it makes a
			// future preset that grows an order of magnitude visible in the
			// test log rather than only in a failure.
			t.Logf("positions=%d (floor %d), retained bytes=%d (floor %d)",
				b.positions, floors.TemplatePositions, b.retained, floors.TemplateRetainedBytes)

			if b.positions > floors.TemplatePositions/4 {
				t.Errorf("this preset costs %d positions against a floor of %d -- less than 4x headroom. "+
					"Raise MinExprTemplatePositions rather than shipping a floor a real template can approach.",
					b.positions, floors.TemplatePositions)
			}
		})
	}
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

// TestSubmitterOptions_ExprLimitsReachBothBudgets covers the submit pipeline's
// carrier.
//
// It is STRUCTURAL rather than behavioral, and the reason is worth stating
// plainly rather than hiding: the submit pipeline's EXPR path is unreachable
// today. Submit passes CheckEXPRExpressionsWhileUnsupported=false, and while
// EXPR is StatusInProgress validateExtensions rejects every EXPR-declaring
// template before the walk would run -- so there is no submission whose
// verdict this knob can change yet, and a test claiming otherwise would be
// testing nothing. What CAN be pinned now is that the value an operator
// configured is the value both of prepareTemplate's budgets and the phase-1
// ValidateOptions are built from. Sub-project H, which flips the status,
// should replace this with a real end-to-end submission.
func TestSubmitterOptions_ExprLimitsReachBothBudgets(t *testing.T) {
	want := ExprLimits{
		SubmissionOperations:  1_234,
		SubmissionMemoryBytes: 5_678,
		TemplatePositions:     910,
		TemplateRetainedBytes: 111_213,
	}
	s := NewSubmitterWithOptions(nil, SubmitterOptions{EnforceLimits: true, ExprLimits: want})
	if s.exprLimits != want {
		t.Fatalf("Submitter.exprLimits = %+v, want %+v", s.exprLimits, want)
	}
	if got := newTemplateBudget(s.exprLimits).limits; got != want {
		t.Fatalf("a budget built from the Submitter's limits = %+v, want %+v", got, want)
	}
	// NewSubmitter (no options) must still be the pre-E4d default.
	if got := newTemplateBudget(NewSubmitter(nil).exprLimits).limits; got != DefaultExprLimits() {
		t.Fatalf("NewSubmitter must default to %+v, got %+v", DefaultExprLimits(), got)
	}
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
