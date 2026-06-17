// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

// Tests for extension gating (validateExtensions):
//   - Every entry in JobTemplate.Extensions that is not in supportedExtensions
//     produces a ValidationError at /extensions/<i> (unconditional).
//   - CHUNK[INT] usage without declaring TASK_CHUNKING produces a
//     ValidationError (unconditional).
//   - Checks are unconditional: EnforceLimits=false does NOT suppress them.

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

// ── helper: template with a CHUNK[INT] step parameter ───────────────────────

// chunkTemplate builds a minimal valid template that has one step with a
// CHUNK[INT] task parameter.  extensions is set to the supplied slice.
func chunkTemplate(extensions []string) *openjd.JobTemplate {
	tmpl := &openjd.JobTemplate{
		SpecificationVersion: openjd.SpecVersion,
		Name:                 "ChunkJob",
		Extensions:           extensions,
		Steps: []openjd.StepTemplate{
			{
				Name: "Render",
				ParameterSpace: &openjd.StepParameterSpace{
					TaskParameterDefinitions: []openjd.TaskParamDefinition{
						{
							Name:      "Frames",
							Type:      openjd.TaskParamTypeChunkInt,
							RangeExpr: new("1-10"),
							Chunks:    &openjd.TaskChunks{DefaultTaskCount: 2},
						},
					},
				},
			},
		},
	}
	return tmpl
}

// ── Extension gating: supported / unsupported ─────────────────────────────────

// TestValidateExtensions_TaskChunkingDeclared_OK confirms that declaring
// TASK_CHUNKING while using CHUNK[INT] is valid.
func TestValidateExtensions_TaskChunkingDeclared_OK(t *testing.T) {
	tmpl := chunkTemplate([]string{"TASK_CHUNKING"})
	errs := openjd.Validate(tmpl)
	if len(errs) != 0 {
		t.Errorf("expected no errors; got: %v", errs)
	}
}

// TestValidateExtensions_ChunkIntWithoutDeclaration_Error confirms that a
// CHUNK[INT] parameter without TASK_CHUNKING in extensions is rejected.
func TestValidateExtensions_ChunkIntWithoutDeclaration_Error(t *testing.T) {
	tmpl := chunkTemplate(nil) // no extensions declared
	errs := openjd.Validate(tmpl)
	if !containsMessage(errs, "TASK_CHUNKING") {
		t.Errorf("expected TASK_CHUNKING-required error; got: %v", errs)
	}
}

// TestValidateExtensions_RedactedEnvVars_OK confirms that REDACTED_ENV_VARS is
// a supported extension (sqi implements openjd_redacted_env in the worker).
func TestValidateExtensions_RedactedEnvVars_OK(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	tmpl.Extensions = []string{"REDACTED_ENV_VARS"}
	errs := openjd.Validate(tmpl)
	if len(errs) != 0 {
		t.Errorf("expected no errors for REDACTED_ENV_VARS; got: %v", errs)
	}
}

// TestValidateExtensions_EXPR_Error confirms that the EXPR extension (not
// implemented by sqi) is rejected.
func TestValidateExtensions_EXPR_Error(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	tmpl.Extensions = []string{"EXPR"}
	errs := openjd.Validate(tmpl)
	if !containsPointer(errs, "/extensions/0") {
		t.Errorf("expected /extensions/0 error for unsupported EXPR; got: %v", errs)
	}
	if !containsMessage(errs, `unsupported extension`) {
		t.Errorf("expected 'unsupported extension' message; got: %v", errs)
	}
}

// TestValidateExtensions_UnknownExtension_Error confirms that an unrecognized
// extension name (FUTURE_THING) is rejected.
func TestValidateExtensions_UnknownExtension_Error(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	tmpl.Extensions = []string{"FUTURE_THING"}
	errs := openjd.Validate(tmpl)
	if !containsPointer(errs, "/extensions/0") {
		t.Errorf("expected /extensions/0 error for FUTURE_THING; got: %v", errs)
	}
	if !containsMessage(errs, `unsupported extension`) {
		t.Errorf("expected 'unsupported extension' message; got: %v", errs)
	}
}

// TestValidateExtensions_TaskChunkingWithoutUsage_OK confirms that declaring
// TASK_CHUNKING without actually using CHUNK[INT] is allowed.
func TestValidateExtensions_TaskChunkingWithoutUsage_OK(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	tmpl.Extensions = []string{"TASK_CHUNKING"}
	errs := openjd.Validate(tmpl)
	if len(errs) != 0 {
		t.Errorf("expected no errors for unused TASK_CHUNKING declaration; got: %v", errs)
	}
}

// TestValidateExtensions_NoExtensions_NoChunk_OK confirms that a template with
// no extensions and no CHUNK[INT] usage validates cleanly.
func TestValidateExtensions_NoExtensions_NoChunk_OK(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	// No Extensions field set; no CHUNK[INT] in steps.
	errs := openjd.Validate(tmpl)
	if len(errs) != 0 {
		t.Errorf("expected no errors; got: %v", errs)
	}
}

// TestValidateExtensions_MultipleErrors confirms that each unsupported entry in
// the extensions list produces its own error at the correct pointer.
func TestValidateExtensions_MultipleErrors(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	tmpl.Extensions = []string{"FUTURE_THING", "EXPR", "TASK_CHUNKING"}
	errs := openjd.Validate(tmpl)
	if !containsPointer(errs, "/extensions/0") {
		t.Errorf("expected /extensions/0 error; got: %v", errs)
	}
	if !containsPointer(errs, "/extensions/1") {
		t.Errorf("expected /extensions/1 error; got: %v", errs)
	}
	// TASK_CHUNKING at index 2 is supported — no error for it.
	if containsPointer(errs, "/extensions/2") {
		t.Errorf("did not expect /extensions/2 error for supported TASK_CHUNKING; got: %v", errs)
	}
}

// ── Unconditional: EnforceLimits=false does NOT suppress extension errors ─────

// TestValidateExtensions_Unconditional_EXPR confirms that even with
// EnforceLimits=false the unsupported-extension check still fires. The
// extension gate is structural correctness, not a quantitative limit.
func TestValidateExtensions_Unconditional_EXPR(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	tmpl.Extensions = []string{"EXPR"}

	errs := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{EnforceLimits: false})
	if !containsPointer(errs, "/extensions/0") {
		t.Errorf("EnforceLimits=false: expected /extensions/0 error for EXPR; got: %v", errs)
	}
}

// TestValidateExtensions_Unconditional_ChunkNoDeclaration confirms that the
// missing-TASK_CHUNKING error fires regardless of EnforceLimits.
func TestValidateExtensions_Unconditional_ChunkNoDeclaration(t *testing.T) {
	tmpl := chunkTemplate(nil)

	errs := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{EnforceLimits: false})
	if !containsMessage(errs, "TASK_CHUNKING") {
		t.Errorf("EnforceLimits=false: expected TASK_CHUNKING-required error; got: %v", errs)
	}
}

// ── Fix 1: Extension name format validation ───────────────────────────────────

// TestValidateExtensions_FormatError_Lowercase confirms that a lowercase
// extension name ("expr" instead of "EXPR") is rejected with a format error.
func TestValidateExtensions_FormatError_Lowercase(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	tmpl.Extensions = []string{"expr"}
	errs := openjd.Validate(tmpl)
	if !containsPointer(errs, "/extensions/0") {
		t.Errorf("expected /extensions/0 error for lowercase extension; got: %v", errs)
	}
	if !containsMessage(errs, "invalid extension name") {
		t.Errorf("expected 'invalid extension name' error message; got: %v", errs)
	}
}

// TestValidateExtensions_FormatError_TooShort confirms that an extension name
// that is too short (less than 3 chars) is rejected with a format error.
func TestValidateExtensions_FormatError_TooShort(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	tmpl.Extensions = []string{"AB"}
	errs := openjd.Validate(tmpl)
	if !containsPointer(errs, "/extensions/0") {
		t.Errorf("expected /extensions/0 error for too-short extension; got: %v", errs)
	}
	if !containsMessage(errs, "invalid extension name") {
		t.Errorf("expected 'invalid extension name' error message; got: %v", errs)
	}
}

// TestValidateExtensions_FormatError_Empty confirms that an empty extension
// name is rejected with a format error.
func TestValidateExtensions_FormatError_Empty(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	tmpl.Extensions = []string{""}
	errs := openjd.Validate(tmpl)
	if !containsPointer(errs, "/extensions/0") {
		t.Errorf("expected /extensions/0 error for empty extension; got: %v", errs)
	}
	if !containsMessage(errs, "invalid extension name") {
		t.Errorf("expected 'invalid extension name' error message; got: %v", errs)
	}
}

// TestValidateExtensions_FormatOK_UnsupportedName confirms that a well-formed
// but unsupported extension name (like "EXPR") still gets the "unsupported"
// error, not a format error.
func TestValidateExtensions_FormatOK_UnsupportedName(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	tmpl.Extensions = []string{"EXPR"}
	errs := openjd.Validate(tmpl)
	if !containsPointer(errs, "/extensions/0") {
		t.Errorf("expected /extensions/0 error; got: %v", errs)
	}
	// Should be "unsupported" error, not "invalid extension name" error
	if !containsMessage(errs, "unsupported extension") {
		t.Errorf("expected 'unsupported extension' error message; got: %v", errs)
	}
	if containsMessage(errs, "invalid extension name") {
		t.Errorf("did not expect 'invalid extension name' error for well-formed name; got: %v", errs)
	}
}

// ── Fix 2: Two steps with CHUNK[INT] without TASK_CHUNKING → two errors ──────

// TestValidateExtensions_TwoChunkSteps_NoDeclaration confirms that when two
// steps each use CHUNK[INT] without declaring TASK_CHUNKING, we get two errors
// (one per step at the respective parameterSpace pointer).
func TestValidateExtensions_TwoChunkSteps_NoDeclaration(t *testing.T) {
	tmpl := &openjd.JobTemplate{
		SpecificationVersion: openjd.SpecVersion,
		Name:                 "TwoChunkJob",
		Extensions:           []string{}, // no TASK_CHUNKING
		Steps: []openjd.StepTemplate{
			{
				Name: "RenderA",
				ParameterSpace: &openjd.StepParameterSpace{
					TaskParameterDefinitions: []openjd.TaskParamDefinition{
						{
							Name:      "FramesA",
							Type:      openjd.TaskParamTypeChunkInt,
							RangeExpr: new("1-10"),
							Chunks:    &openjd.TaskChunks{DefaultTaskCount: 2},
						},
					},
				},
			},
			{
				Name: "RenderB",
				ParameterSpace: &openjd.StepParameterSpace{
					TaskParameterDefinitions: []openjd.TaskParamDefinition{
						{
							Name:      "FramesB",
							Type:      openjd.TaskParamTypeChunkInt,
							RangeExpr: new("11-20"),
							Chunks:    &openjd.TaskChunks{DefaultTaskCount: 2},
						},
					},
				},
			},
		},
	}
	errs := openjd.Validate(tmpl)
	if !containsPointer(errs, "/steps/0/parameterSpace") {
		t.Errorf("expected /steps/0/parameterSpace error; got: %v", errs)
	}
	if !containsPointer(errs, "/steps/1/parameterSpace") {
		t.Errorf("expected /steps/1/parameterSpace error; got: %v", errs)
	}
	// Count TASK_CHUNKING errors
	var chunkingErrors int
	for _, err := range errs {
		if err.Pointer == "/steps/0/parameterSpace" || err.Pointer == "/steps/1/parameterSpace" {
			if containsMessage(openjd.ValidationErrors{err}, "TASK_CHUNKING") {
				chunkingErrors++
			}
		}
	}
	if chunkingErrors != 2 {
		t.Errorf("expected 2 TASK_CHUNKING-required errors; got %d errors: %v", chunkingErrors, errs)
	}
}

// ── Fix 3: Unsupported extension + CHUNK[INT] used + TASK_CHUNKING declared ──

// TestValidateExtensions_UnsupportedPlusDeclaredChunking confirms that when an
// unsupported extension is present alongside TASK_CHUNKING, and CHUNK[INT] is
// used, we get exactly one error (for the unsupported extension) and no
// TASK_CHUNKING-required error. This confirms that the declared-set is
// populated even for unsupported entries.
func TestValidateExtensions_UnsupportedPlusDeclaredChunking(t *testing.T) {
	tmpl := &openjd.JobTemplate{
		SpecificationVersion: openjd.SpecVersion,
		Name:                 "MixedExtensionsJob",
		Extensions:           []string{"UNSUPPORTED", "TASK_CHUNKING"},
		Steps: []openjd.StepTemplate{
			{
				Name: "Render",
				ParameterSpace: &openjd.StepParameterSpace{
					TaskParameterDefinitions: []openjd.TaskParamDefinition{
						{
							Name:      "Frames",
							Type:      openjd.TaskParamTypeChunkInt,
							RangeExpr: new("1-10"),
							Chunks:    &openjd.TaskChunks{DefaultTaskCount: 2},
						},
					},
				},
			},
		},
	}
	errs := openjd.Validate(tmpl)

	// Should have exactly one error: the unsupported extension at /extensions/0
	if !containsPointer(errs, "/extensions/0") {
		t.Errorf("expected /extensions/0 error for unsupported extension; got: %v", errs)
	}

	// Should NOT have a TASK_CHUNKING-required error (since it IS declared)
	if containsMessage(errs, "TASK_CHUNKING") {
		hasChunkingRequired := false
		for _, err := range errs {
			if strings.Contains(err.Message, "CHUNK[INT] parameters require declaring") {
				hasChunkingRequired = true
				break
			}
		}
		if hasChunkingRequired {
			t.Errorf("did not expect TASK_CHUNKING-required error when TASK_CHUNKING is declared; got: %v", errs)
		}
	}

	// Count total errors related to extensions (should be exactly 1)
	var extErrors int
	for _, err := range errs {
		if err.Pointer == "/extensions/0" {
			extErrors++
		}
	}
	if extErrors != 1 {
		t.Errorf("expected exactly 1 extension error; got %d: %v", extErrors, errs)
	}
}
