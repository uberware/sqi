// SPDX-License-Identifier: AGPL-3.0-or-later

package product_test

import (
	"encoding/json"
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

// sliceCeiling is the slice count ffmpeg-segment-transcode-expr's readme says
// can be submitted under the default server EXPR limits, and the number this
// file pins.
//
// It is finite because that preset's join step generates its concat list from
// the template, and the expression references only job parameters, so phase 2
// evaluates it at submission against openjd.expr_operation_limit (default
// 10,000), not against the worker's larger expr.operation_limit. The Bash and
// PowerShell variants have no such ceiling: their join list is written by a
// script at run time and costs the server nothing.
//
// On the submission path (openjd.Submitter.Submit, which runs phase 1, binds
// the parameters and then re-checks every expression position with them
// concrete) the embedded-file expression costs about 24.15 operations per
// slice plus about 5 fixed: 246 operations at 10 slices, 2,419 at 100, 4,833
// at 200, 9,662 at 400, 19,320 at 800. The largest slice count that validates
// at the default 10,000 is 414. The readme states 400, a round number that
// leaves about 3% of the budget as headroom.
const sliceCeiling = 400

// exprCostPreset is the preset whose submission cost this file measures.
const exprCostPreset = "ffmpeg-segment-transcode-expr"

// exprCostPresetPath is that preset's definition file.
func exprCostPresetPath() string {
	return filepath.Join("..", "..", "presets", "sqi", exprCostPreset+".yaml")
}

// TestExprPresetCostCeilingIsDocumented fails while the preset still carries
// the authoring marker, and when the documented ceiling and the constant above
// disagree.
func TestExprPresetCostCeilingIsDocumented(t *testing.T) {
	data, err := os.ReadFile(exprCostPresetPath())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(data)
	if strings.Contains(body, "DESCRIPTION-SLICE-CEILING") {
		t.Fatal("preset still carries the authoring marker; substitute the slice ceiling")
	}
	if !strings.Contains(body, strconv.Itoa(sliceCeiling)) {
		t.Errorf("preset does not state the ceiling of %d slices", sliceCeiling)
	}
}

// TestExprPresetSubmitsAtCeiling pins the headroom. A change that makes the
// preset cost more per slice fails here, at the preset, instead of shrinking
// the ceiling its readme states.
//
// It validates the template with concrete parameters at the documented
// ceiling (must pass) and well past it (must fail with a limit error). Either
// assertion alone would pass a validator that accepted everything or nothing.
//
// The entry point is openjd.Submitter.Submit, the same call internal/api's
// submitJob makes, because the ceiling is a property of the phase-2 walk:
// phase 1 binds every Param.* symbol unresolved, so range(ceil(...)) cannot
// expand and the expression costs almost nothing. Only a submission with
// concrete parameters (product.ValidateTemplate has none) charges the cost
// pinned here. Submitting also proves the parameters produce the slice count
// claimed.
//
// Both subtests submit a fixed slice count; searching for the ceiling would
// take dozens of submissions per run.
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
		// A limit error specifically. The three failures this must not be
		// mistaken for also reach the caller as an error:
		//   - a parse or type error, which would mean the template broke rather
		//     than grew expensive. Told apart by the operation-limit text,
		//     which nothing else in the checker produces.
		//   - a deadline breach, which is the server giving up under load (a
		//     503) rather than a deterministic property of the template (a
		//     422). Matched structurally, as internal/api does.
		//   - a store or dependency failure, which is not the client's fault.
		//     Excluded by requiring the client-fault channel.
		if errors.Is(err, expr.ErrDeadlineExceeded) {
			t.Fatalf("failed on the wall-clock deadline, not the operation limit: %v", err)
		}
		if _, ok := errors.AsType[*openjd.SubmitValidationError](err); !ok {
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

// submitSlices submits tmpl through a default Submitter (openjd.NewSubmitter
// takes the default EXPR limits, so this tests the shipped configuration) with
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

// TestOIIOToolOperationsCeilingFitsTheStockMemoryLimit pins what the largest
// Operations list oiiotool-process accepts (64 items of 1024 characters, the
// parameter's own bounds) costs at submission.
//
// Operations is the one input of that preset that grows: the other expressions
// cost a fixed ~24 operations and ~285 retained bytes whatever the job. A full
// list costs the same 24 operations, but its strings are live values while the
// argument list is built, and that is charged against
// openjd.expr_memory_limit: 69,696 bytes against the stock 1,000,000 (14x
// headroom), and 20,544 for 64 items of 256. An operator who lowers the memory
// limit toward its 4,096-byte floor rejects a long list well before the floor,
// as with ffmpeg-segment-transcode-expr's slice count.
//
// The table submits under fixed memory limits: a full list must fit in 100,000
// bytes, and must not fit in 50,000, so the first cannot pass because the limit
// never applied.
func TestOIIOToolOperationsCeilingFitsTheStockMemoryLimit(t *testing.T) {
	const items, itemLen = 64, 1024
	tmpl := presetTemplate(t, "oiiotool-process")
	ops := make([]string, items)
	for i := range ops {
		ops[i] = strings.Repeat("x", itemLen)
	}
	list, err := json.Marshal(ops)
	if err != nil {
		t.Fatal(err)
	}
	submit := func(limits openjd.ExprLimits) (*openjd.SubmitResult, error) {
		st := fake.New()
		farmID, queueID := seedFarmQueue(t, st)
		return openjd.NewSubmitterWithOptions(st, openjd.SubmitterOptions{EnforceLimits: true, ExprLimits: limits}).
			Submit(t.Context(), tmpl, store.TemplateFormatYAML, openjd.SubmitOptions{
				FarmID: farmID, QueueID: queueID,
				Parameters: map[string]string{
					"InputFile": "/mnt/i/in.####.exr", "OutputFile": "/mnt/o/out.####.png",
					"Frames": "1-2", "FramesPerTask": "1", "Operations": string(list),
				},
			})
	}

	for _, tc := range []struct {
		name        string
		memoryLimit int64
		wantErr     bool
	}{
		{"fits in a tenth of the stock limit", 100_000, false},
		{"the memory limit applies", 50_000, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limits := openjd.DefaultExprLimits()
			limits.SubmissionMemoryBytes = tc.memoryLimit
			res, err := submit(limits)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("a full Operations list was accepted under a %d-byte memory limit; the cost this file pins is not being charged", tc.memoryLimit)
				}
				if _, ok := errors.AsType[*openjd.SubmitValidationError](err); !ok {
					t.Fatalf("want a *openjd.SubmitValidationError, got %T: %v", err, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("a full Operations list must fit in %d bytes of expression memory: %v", tc.memoryLimit, err)
			}
			if len(res.Tasks) != 2 {
				t.Errorf("expanded to %d tasks, want 2", len(res.Tasks))
			}
		})
	}
}
