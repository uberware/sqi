// SPDX-License-Identifier: AGPL-3.0-or-later

package product_test

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/openjd/expr"
	"github.com/uberware/sqi/internal/product"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// sliceCeiling is the slice count ffmpeg-segment-transcode-expr's description
// promises can be submitted under the STOCK server EXPR limits, and the number
// this file pins.
//
// It is finite because that preset's join step generates its concat list from
// the template, and the expression references only job parameters -- so phase 2
// evaluates it AT SUBMISSION, against openjd.expr_operation_limit (default
// 10,000), not against the worker's far roomier expr.operation_limit. The bash
// and PowerShell variants have no such ceiling: their join list is written by a
// script at run time and costs the server nothing.
//
// MEASURED, not chosen: driving the real submission path
// (openjd.Submitter.Submit, which runs phase 1, binds the parameters and then
// re-checks every expression position with them concrete) the cost of the
// embedded-file expression is linear in the slice count at ~24.15 operations
// per slice plus ~5 fixed -- 246 operations at 10 slices, 2,419 at 100, 4,833
// at 200, 9,662 at 400, 19,320 at 800. The largest slice count that validates
// at the default 10,000 is therefore 414; 415 fails. 400 is documented and
// pinned instead of 414 so the promise is a round number a reader can size a
// job against, and so the last 3% of the budget stays headroom rather than
// being spent by the description.
const sliceCeiling = 400

// exprCostPreset is the preset whose submission cost this file measures.
const exprCostPreset = "ffmpeg-segment-transcode-expr"

// exprCostPresetPath is that preset's definition file.
func exprCostPresetPath() string {
	return filepath.Join("..", "..", "presets", "sqi", exprCostPreset+".yaml")
}

// TestExprPresetCostCeilingIsDocumented fails while the preset's description
// still carries the authoring marker, and if the documented ceiling and the
// constant above ever disagree. A reader sizing a job trusts that sentence.
func TestExprPresetCostCeilingIsDocumented(t *testing.T) {
	data, err := os.ReadFile(exprCostPresetPath())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(data)
	if strings.Contains(body, "DESCRIPTION-SLICE-CEILING") {
		t.Fatal("preset description still carries the authoring marker; substitute the measured ceiling")
	}
	if !strings.Contains(body, strconv.Itoa(sliceCeiling)) {
		t.Errorf("preset description does not state the measured ceiling of %d slices", sliceCeiling)
	}
}

// TestExprPresetSubmitsAtCeiling pins the measured headroom. A change that makes
// the portable preset cost more per slice fails HERE, at the preset, rather than
// silently shrinking the ceiling its description promises.
//
// It validates the template with concrete parameters at both the documented
// ceiling (must pass) and comfortably past it (must fail with a limit error) --
// one assertion alone proves nothing, since a validator that accepted
// everything would satisfy the first.
//
// The entry point is openjd.Submitter.Submit, the same call internal/api's
// submitJob makes, because the ceiling is a property of the PHASE-2 walk: phase
// 1 binds every Param.* symbol unresolved, so range(ceil(...)) cannot expand
// and the expression costs almost nothing. Only a submission with concrete
// parameters -- product.ValidateTemplate has none to offer -- charges the cost
// being pinned here. Submitting for real also proves the parameters produce the
// slice count claimed, which a direct call to the evaluator would not.
//
// Both subtests are fixed-N submissions rather than a search for the ceiling:
// the search that measured it took a few dozen submissions, which is not a
// price every run of the suite should pay.
func TestExprPresetSubmitsAtCeiling(t *testing.T) {
	tmpl, format := exprCostTemplate(t)

	t.Run("at the ceiling", func(t *testing.T) {
		res, err := submitSlices(t, tmpl, format, sliceCeiling)
		if err != nil {
			t.Fatalf("submitting %s with %d slices must validate under the stock EXPR limits, got: %v",
				exprCostPreset, sliceCeiling, err)
		}
		// One task per slice in Transcode, plus the single Join task. Asserting
		// it proves the parameters really produced sliceCeiling slices, so a
		// pass cannot come from a submission that quietly expanded to one.
		if want := sliceCeiling + 1; len(res.Tasks) != want {
			t.Errorf("expanded to %d tasks, want %d (%d transcode + 1 join)",
				len(res.Tasks), want, sliceCeiling)
		}
	})

	t.Run("well past the ceiling", func(t *testing.T) {
		past := sliceCeiling * 10
		_, err := submitSlices(t, tmpl, format, past)
		if err == nil {
			t.Fatalf("submitting %s with %d slices must exceed the stock EXPR operation limit, got no error",
				exprCostPreset, past)
		}
		// A LIMIT error specifically. The three failures this must not be
		// mistaken for all reach the caller as an error too:
		//   - a parse or type error, which would mean the template broke rather
		//     than grew expensive. Discriminated by the operation-limit text --
		//     nothing else in the checker produces it.
		//   - a deadline breach, which is the server giving up under load (a
		//     503) rather than a deterministic property of the template (a
		//     422). Matched structurally, as internal/api does.
		//   - a store or dependency failure, which is not the client's fault at
		//     all. Excluded by requiring the client-fault channel.
		if errors.Is(err, expr.ErrDeadlineExceeded) {
			t.Fatalf("failed on the wall-clock deadline, not the operation limit: %v", err)
		}
		var verr *openjd.SubmitValidationError
		if !errors.As(err, &verr) {
			t.Fatalf("want a *openjd.SubmitValidationError (the client-fault channel), got %T: %v", err, err)
		}
		if !strings.Contains(err.Error(), "operation limit exceeded") {
			t.Errorf("want the section 1.3.10 operation-limit failure, got: %v", err)
		}
	})
}

// exprCostTemplate parses the preset definition and returns its inline OpenJD
// template exactly as the catalog would store it.
func exprCostTemplate(t *testing.T) (string, store.TemplateFormat) {
	t.Helper()
	data, err := os.ReadFile(exprCostPresetPath())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	p, err := product.ParseDefinition(data, product.ValidateOptions{EnforceLimits: true})
	if err != nil {
		t.Fatalf("ParseDefinition: %v", err)
	}
	return p.Template, p.Format
}

// submitSlices submits tmpl through a stock Submitter -- openjd.NewSubmitter
// takes the default EXPR limits, which is what makes this a measurement of the
// SHIPPED configuration rather than of whatever this test chose -- with
// parameters that expand to exactly sliceCount slices: SegmentSeconds is 60 and
// DurationSeconds is sliceCount*60, so ceil(DurationSeconds/SegmentSeconds) is
// sliceCount exactly.
func submitSlices(t *testing.T, tmpl string, format store.TemplateFormat, sliceCount int) (*openjd.SubmitResult, error) {
	t.Helper()
	st := fake.New()
	farmID, queueID := seedFarmQueue(t, st)
	return openjd.NewSubmitter(st).Submit(t.Context(), tmpl, format, openjd.SubmitOptions{
		FarmID:  farmID,
		QueueID: queueID,
		Parameters: map[string]string{
			"SourceFile":      "/mnt/in/source.mov",
			"OutputFile":      "/mnt/out/result.mov",
			"DurationSeconds": strconv.Itoa(sliceCount * 60),
			"SegmentSeconds":  "60",
		},
	})
}
