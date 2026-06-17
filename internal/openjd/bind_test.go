// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

import (
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

func ptr[T any](v T) *T { return new(v) }

// ── BindJobParameters ─────────────────────────────────────────────────────────

func TestBindJobParameters(t *testing.T) {
	t.Run("provided value is used", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Scene", Type: openjd.JobParamTypeString},
		}
		got, errs := openjd.BindJobParameters(params, map[string]string{"Scene": "shot_010"})
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		if got["Scene"] != "shot_010" {
			t.Errorf("Scene = %q, want %q", got["Scene"], "shot_010")
		}
	})

	t.Run("default is applied when no value provided", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Quality", Type: openjd.JobParamTypeString, Default: new("medium")},
		}
		got, errs := openjd.BindJobParameters(params, nil)
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		if got["Quality"] != "medium" {
			t.Errorf("Quality = %q, want %q", got["Quality"], "medium")
		}
	})

	t.Run("provided value overrides default", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Quality", Type: openjd.JobParamTypeString, Default: new("medium")},
		}
		got, errs := openjd.BindJobParameters(params, map[string]string{"Quality": "high"})
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		if got["Quality"] != "high" {
			t.Errorf("Quality = %q, want %q", got["Quality"], "high")
		}
	})

	t.Run("required-but-missing returns error", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Scene", Type: openjd.JobParamTypeString},
		}
		_, errs := openjd.BindJobParameters(params, nil)
		if len(errs) == 0 {
			t.Fatal("expected error for missing required parameter, got none")
		}
		// Pointer must reference the definition by name.
		found := false
		for _, e := range errs {
			if e.Pointer == "/parameterDefinitions/Scene" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no error with pointer %q: got %v", "/parameterDefinitions/Scene", errs)
		}
	})

	t.Run("unknown parameter returns error", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Scene", Type: openjd.JobParamTypeString, Default: new("x")},
		}
		_, errs := openjd.BindJobParameters(params, map[string]string{
			"Scene":   "shot",
			"Unknown": "value",
		})
		if len(errs) == 0 {
			t.Fatal("expected error for unknown parameter, got none")
		}
		found := false
		for _, e := range errs {
			if e.Pointer == "/parameterValues/Unknown" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no error with pointer %q: got %v", "/parameterValues/Unknown", errs)
		}
	})

	t.Run("multiple errors accumulated", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "A", Type: openjd.JobParamTypeString},
			{Name: "B", Type: openjd.JobParamTypeInt},
		}
		_, errs := openjd.BindJobParameters(params, map[string]string{
			"Unknown": "x",
		})
		// Expect at least 3 errors: unknown "Unknown", missing "A", missing "B".
		if len(errs) < 3 {
			t.Errorf("expected ≥3 errors (2 missing + 1 unknown), got %d: %v", len(errs), errs)
		}
	})

	// ── INT ──────────────────────────────────────────────────────────────────

	t.Run("INT: valid integer accepted", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Frame", Type: openjd.JobParamTypeInt},
		}
		got, errs := openjd.BindJobParameters(params, map[string]string{"Frame": "42"})
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		if got["Frame"] != "42" {
			t.Errorf("Frame = %q, want %q", got["Frame"], "42")
		}
	})

	t.Run("INT: non-integer string is error", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Frame", Type: openjd.JobParamTypeInt},
		}
		_, errs := openjd.BindJobParameters(params, map[string]string{"Frame": "abc"})
		if len(errs) == 0 {
			t.Fatal("expected error for non-integer value")
		}
	})

	t.Run("INT: value below min is error", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Frame", Type: openjd.JobParamTypeInt, MinValue: ptr("1")},
		}
		_, errs := openjd.BindJobParameters(params, map[string]string{"Frame": "0"})
		if len(errs) == 0 {
			t.Fatal("expected error for value below min")
		}
		if errs[0].Pointer != "/parameterValues/Frame" {
			t.Errorf("error pointer = %q, want %q", errs[0].Pointer, "/parameterValues/Frame")
		}
	})

	t.Run("INT: value at min is OK", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Frame", Type: openjd.JobParamTypeInt, MinValue: ptr("1")},
		}
		_, errs := openjd.BindJobParameters(params, map[string]string{"Frame": "1"})
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
	})

	t.Run("INT: value above max is error", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Frame", Type: openjd.JobParamTypeInt, MaxValue: ptr("100")},
		}
		_, errs := openjd.BindJobParameters(params, map[string]string{"Frame": "101"})
		if len(errs) == 0 {
			t.Fatal("expected error for value above max")
		}
	})

	t.Run("INT: value at max is OK", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Frame", Type: openjd.JobParamTypeInt, MaxValue: ptr("100")},
		}
		_, errs := openjd.BindJobParameters(params, map[string]string{"Frame": "100"})
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
	})

	t.Run("INT: allowedValues hit", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Threads", Type: openjd.JobParamTypeInt, AllowedValues: []string{"1", "2", "4", "8"}},
		}
		_, errs := openjd.BindJobParameters(params, map[string]string{"Threads": "4"})
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
	})

	t.Run("INT: allowedValues hit with canonical comparison (leading zero)", func(t *testing.T) {
		params := []openjd.JobParameter{
			// "04" in allowedValues should match provided "4" (same int64 value).
			{Name: "Threads", Type: openjd.JobParamTypeInt, AllowedValues: []string{"01", "02", "04", "08"}},
		}
		_, errs := openjd.BindJobParameters(params, map[string]string{"Threads": "4"})
		if len(errs) != 0 {
			t.Fatalf("unexpected errors (canonical int compare): %v", errs)
		}
	})

	t.Run("INT: allowedValues miss", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Threads", Type: openjd.JobParamTypeInt, AllowedValues: []string{"1", "2", "4", "8"}},
		}
		_, errs := openjd.BindJobParameters(params, map[string]string{"Threads": "3"})
		if len(errs) == 0 {
			t.Fatal("expected error for value not in allowedValues")
		}
	})

	t.Run("INT: both min/max OK", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Frame", Type: openjd.JobParamTypeInt, MinValue: ptr("1"), MaxValue: ptr("100")},
		}
		_, errs := openjd.BindJobParameters(params, map[string]string{"Frame": "50"})
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
	})

	// ── FLOAT ────────────────────────────────────────────────────────────────

	t.Run("FLOAT: valid float accepted", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Scale", Type: openjd.JobParamTypeFloat},
		}
		got, errs := openjd.BindJobParameters(params, map[string]string{"Scale": "1.5"})
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		if got["Scale"] != "1.5" {
			t.Errorf("Scale = %q, want %q", got["Scale"], "1.5")
		}
	})

	t.Run("FLOAT: non-float string is error", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Scale", Type: openjd.JobParamTypeFloat},
		}
		_, errs := openjd.BindJobParameters(params, map[string]string{"Scale": "abc"})
		if len(errs) == 0 {
			t.Fatal("expected error for non-float value")
		}
	})

	t.Run("FLOAT: value below min is error", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Scale", Type: openjd.JobParamTypeFloat, MinValue: ptr("0.0")},
		}
		_, errs := openjd.BindJobParameters(params, map[string]string{"Scale": "-1.0"})
		if len(errs) == 0 {
			t.Fatal("expected error for value below min")
		}
	})

	t.Run("FLOAT: value above max is error", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Scale", Type: openjd.JobParamTypeFloat, MaxValue: ptr("1.0")},
		}
		_, errs := openjd.BindJobParameters(params, map[string]string{"Scale": "2.0"})
		if len(errs) == 0 {
			t.Fatal("expected error for value above max")
		}
	})

	t.Run("FLOAT: allowedValues hit", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Scale", Type: openjd.JobParamTypeFloat, AllowedValues: []string{"0.5", "1.0", "2.0"}},
		}
		_, errs := openjd.BindJobParameters(params, map[string]string{"Scale": "1.0"})
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
	})

	t.Run("FLOAT: allowedValues miss", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Scale", Type: openjd.JobParamTypeFloat, AllowedValues: []string{"0.5", "1.0", "2.0"}},
		}
		_, errs := openjd.BindJobParameters(params, map[string]string{"Scale": "1.5"})
		if len(errs) == 0 {
			t.Fatal("expected error for value not in allowedValues")
		}
	})

	t.Run("FLOAT: NaN value is error", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Scale", Type: openjd.JobParamTypeFloat},
		}
		_, errs := openjd.BindJobParameters(params, map[string]string{"Scale": "NaN"})
		if len(errs) == 0 {
			t.Fatal("expected error for NaN FLOAT value")
		}
		if errs[0].Pointer != "/parameterValues/Scale" {
			t.Errorf("error pointer = %q, want %q", errs[0].Pointer, "/parameterValues/Scale")
		}
	})

	t.Run("FLOAT: +Inf value is error", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Scale", Type: openjd.JobParamTypeFloat},
		}
		_, errs := openjd.BindJobParameters(params, map[string]string{"Scale": "+Inf"})
		if len(errs) == 0 {
			t.Fatal("expected error for +Inf FLOAT value")
		}
		if errs[0].Pointer != "/parameterValues/Scale" {
			t.Errorf("error pointer = %q, want %q", errs[0].Pointer, "/parameterValues/Scale")
		}
	})

	t.Run("FLOAT: -Inf value is error", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Scale", Type: openjd.JobParamTypeFloat},
		}
		_, errs := openjd.BindJobParameters(params, map[string]string{"Scale": "-Inf"})
		if len(errs) == 0 {
			t.Fatal("expected error for -Inf FLOAT value")
		}
		if errs[0].Pointer != "/parameterValues/Scale" {
			t.Errorf("error pointer = %q, want %q", errs[0].Pointer, "/parameterValues/Scale")
		}
	})

	// ── STRING ───────────────────────────────────────────────────────────────

	t.Run("STRING: value within length is OK", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Name", Type: openjd.JobParamTypeString, MinLength: ptr(2), MaxLength: ptr(10)},
		}
		_, errs := openjd.BindJobParameters(params, map[string]string{"Name": "hello"})
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
	})

	t.Run("STRING: value below min length is error", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Name", Type: openjd.JobParamTypeString, MinLength: ptr(5)},
		}
		_, errs := openjd.BindJobParameters(params, map[string]string{"Name": "hi"})
		if len(errs) == 0 {
			t.Fatal("expected error for value below min length")
		}
		if errs[0].Pointer != "/parameterValues/Name" {
			t.Errorf("error pointer = %q, want %q", errs[0].Pointer, "/parameterValues/Name")
		}
	})

	t.Run("STRING: value above max length is error", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Name", Type: openjd.JobParamTypeString, MaxLength: ptr(3)},
		}
		_, errs := openjd.BindJobParameters(params, map[string]string{"Name": "toolong"})
		if len(errs) == 0 {
			t.Fatal("expected error for value above max length")
		}
	})

	t.Run("STRING: allowedValues hit", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Mode", Type: openjd.JobParamTypeString, AllowedValues: []string{"fast", "normal", "slow"}},
		}
		_, errs := openjd.BindJobParameters(params, map[string]string{"Mode": "fast"})
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
	})

	t.Run("STRING: allowedValues miss", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Mode", Type: openjd.JobParamTypeString, AllowedValues: []string{"fast", "normal", "slow"}},
		}
		_, errs := openjd.BindJobParameters(params, map[string]string{"Mode": "turbo"})
		if len(errs) == 0 {
			t.Fatal("expected error for value not in allowedValues")
		}
	})

	// ── PATH ─────────────────────────────────────────────────────────────────

	t.Run("PATH: treated like STRING for constraints", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "OutputDir", Type: openjd.JobParamTypePath, MinLength: ptr(1), MaxLength: ptr(255)},
		}
		_, errs := openjd.BindJobParameters(params, map[string]string{"OutputDir": "/tmp/render"})
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
	})

	t.Run("PATH: allowedValues hit", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Root", Type: openjd.JobParamTypePath, AllowedValues: []string{"/mnt/a", "/mnt/b"}},
		}
		_, errs := openjd.BindJobParameters(params, map[string]string{"Root": "/mnt/a"})
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
	})

	t.Run("PATH: allowedValues miss", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Root", Type: openjd.JobParamTypePath, AllowedValues: []string{"/mnt/a", "/mnt/b"}},
		}
		_, errs := openjd.BindJobParameters(params, map[string]string{"Root": "/mnt/c"})
		if len(errs) == 0 {
			t.Fatal("expected error for path not in allowedValues")
		}
	})

	t.Run("PATH: max length exceeded is error", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Root", Type: openjd.JobParamTypePath, MaxLength: ptr(5)},
		}
		_, errs := openjd.BindJobParameters(params, map[string]string{"Root": "/very/long/path"})
		if len(errs) == 0 {
			t.Fatal("expected error for path exceeding max length")
		}
	})

	// ── Happy path: multiple params fully resolved ────────────────────────────

	t.Run("happy path: all params resolved correctly", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Scene", Type: openjd.JobParamTypeString, Default: ptr("main")},
			{Name: "Frame", Type: openjd.JobParamTypeInt},
			{Name: "Scale", Type: openjd.JobParamTypeFloat, Default: ptr("1.0")},
			{Name: "Out", Type: openjd.JobParamTypePath},
		}
		got, errs := openjd.BindJobParameters(params, map[string]string{
			"Frame": "42",
			"Out":   "/tmp/out",
		})
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		if got["Scene"] != "main" {
			t.Errorf("Scene = %q, want default %q", got["Scene"], "main")
		}
		if got["Frame"] != "42" {
			t.Errorf("Frame = %q, want %q", got["Frame"], "42")
		}
		if got["Scale"] != "1.0" {
			t.Errorf("Scale = %q, want default %q", got["Scale"], "1.0")
		}
		if got["Out"] != "/tmp/out" {
			t.Errorf("Out = %q, want %q", got["Out"], "/tmp/out")
		}
	})

	// ── Nil map for provided is treated as empty ──────────────────────────────

	t.Run("nil provided map treated as empty", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Scene", Type: openjd.JobParamTypeString, Default: ptr("x")},
		}
		got, errs := openjd.BindJobParameters(params, nil)
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		if got["Scene"] != "x" {
			t.Errorf("Scene = %q, want %q", got["Scene"], "x")
		}
	})

	// ── Empty declared params ─────────────────────────────────────────────────

	t.Run("no declared params, no provided values: empty map returned", func(t *testing.T) {
		got, errs := openjd.BindJobParameters(nil, nil)
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		if got == nil {
			t.Error("expected non-nil map, got nil")
		}
	})

	t.Run("no declared params, provided values: errors for each unknown", func(t *testing.T) {
		_, errs := openjd.BindJobParameters(nil, map[string]string{"Ghost": "val"})
		if len(errs) == 0 {
			t.Fatal("expected error for unknown parameter when no params declared")
		}
	})

	// ── Error on nil map when errors present ──────────────────────────────────

	t.Run("returns nil map on errors", func(t *testing.T) {
		params := []openjd.JobParameter{
			{Name: "Required", Type: openjd.JobParamTypeString},
		}
		got, errs := openjd.BindJobParameters(params, nil)
		if len(errs) == 0 {
			t.Fatal("expected errors")
		}
		if got != nil {
			t.Errorf("expected nil map on error, got %v", got)
		}
	})
}
