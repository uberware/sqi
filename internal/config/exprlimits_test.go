// SPDX-License-Identifier: AGPL-3.0-or-later

package config_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/config"
)

// ── openjd.expr_* : defaults ────────────────────────────────────────────────

// TestDefaultConfig_ExprLimits pins that a fresh install carries exactly the
// four numbers internal/openjd enforced as constants before EXPR sub-project
// E4d made them configurable. The cross-package half of this claim -- that
// these equal internal/openjd's own DefaultExprLimits() -- is
// TestExprLimits_ConfigMatchesOpenJD, in internal/server, because
// internal/config must not import internal/openjd.
func TestDefaultConfig_ExprLimits(t *testing.T) {
	cfg := config.DefaultConfig()
	tests := []struct {
		name string
		got  int64
		want int64
	}{
		{"expr_operation_limit", cfg.OpenJD.ExprOperationLimit, 10_000},
		{"expr_memory_limit", cfg.OpenJD.ExprMemoryLimit, 1_000_000},
		{"expr_template_positions", cfg.OpenJD.ExprTemplatePositions, 10_000},
		{"expr_template_retained_bytes", cfg.OpenJD.ExprTemplateRetainedBytes, 10_000_000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("openjd.%s = %d, want %d (a fresh install must reproduce the pre-E4d constant exactly)",
					tc.name, tc.got, tc.want)
			}
		})
	}
}

// ── openjd.expr_* : validation range ────────────────────────────────────────

// TestValidate_ExprLimits walks every boundary of all four ranges.
//
// The BOUNDARY rows are the point: a test that only checks 1 and 10^9 passes
// with an off-by-one in the comparison. Each knob therefore gets min-1, min,
// max, max+1, plus zero and a negative -- zero especially, because the Go
// zero value of an int64 field is the value a config file that omits the key
// would leave behind if the defaults were ever dropped, and "0" must not
// silently mean "unlimited".
func TestValidate_ExprLimits(t *testing.T) {
	type knob struct {
		field string
		env   string
		set   func(*config.Config, int64)
		min   int64
		max   int64
	}
	knobs := []knob{
		{
			field: "openjd.expr_operation_limit",
			env:   "SQI_OPENJD_EXPR_OPERATION_LIMIT",
			set:   func(c *config.Config, v int64) { c.OpenJD.ExprOperationLimit = v },
			min:   config.MinOpenJDExprOperationLimit,
			max:   config.MaxOpenJDExprOperationLimit,
		},
		{
			field: "openjd.expr_memory_limit",
			env:   "SQI_OPENJD_EXPR_MEMORY_LIMIT",
			set:   func(c *config.Config, v int64) { c.OpenJD.ExprMemoryLimit = v },
			min:   config.MinOpenJDExprMemoryLimit,
			max:   config.MaxOpenJDExprMemoryLimit,
		},
		{
			field: "openjd.expr_template_positions",
			env:   "SQI_OPENJD_EXPR_TEMPLATE_POSITIONS",
			set:   func(c *config.Config, v int64) { c.OpenJD.ExprTemplatePositions = v },
			min:   config.MinOpenJDExprTemplatePositions,
			max:   config.MaxOpenJDExprTemplatePositions,
		},
		{
			field: "openjd.expr_template_retained_bytes",
			env:   "SQI_OPENJD_EXPR_TEMPLATE_RETAINED_BYTES",
			set:   func(c *config.Config, v int64) { c.OpenJD.ExprTemplateRetainedBytes = v },
			min:   config.MinOpenJDExprTemplateRetainedBytes,
			max:   config.MaxOpenJDExprTemplateRetainedBytes,
		},
	}

	for _, k := range knobs {
		t.Run(k.field, func(t *testing.T) {
			cases := []struct {
				name   string
				value  int64
				reject bool
			}{
				{"negative", -1, true},
				{"zero", 0, true},
				{"below floor", k.min - 1, true},
				{"at floor", k.min, false},
				{"at ceiling", k.max, false},
				{"above ceiling", k.max + 1, true},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					cfg := config.DefaultConfig()
					k.set(&cfg, tc.value)

					errs := config.Validate(cfg)
					var found *config.ValidationError
					for i := range errs {
						if errs[i].Field == k.field {
							found = &errs[i]
							break
						}
					}
					switch {
					case tc.reject && found == nil:
						t.Fatalf("%s = %d must be rejected", k.field, tc.value)
					case !tc.reject && found != nil:
						t.Fatalf("%s = %d must be accepted, got %v", k.field, tc.value, *found)
					case !tc.reject:
						return
					}

					// The message has to be actionable: the bound, the value
					// seen, and both ways an operator could have set it.
					msg := found.Message
					for _, want := range []string{
						strconv.FormatInt(k.min, 10),
						strconv.FormatInt(k.max, 10),
						strconv.FormatInt(tc.value, 10),
						k.env,
						k.field,
					} {
						if !strings.Contains(msg, want) {
							t.Errorf("%s message %q does not mention %q", k.field, msg, want)
						}
					}
				})
			}
		})
	}
}

// TestValidate_ExprLimitsAreIndependent proves the four ranges do not share a
// comparison: one out-of-range knob produces exactly one error, naming itself.
func TestValidate_ExprLimitsAreIndependent(t *testing.T) {
	tests := []struct {
		field string
		set   func(*config.Config)
	}{
		{"openjd.expr_operation_limit", func(c *config.Config) { c.OpenJD.ExprOperationLimit = 1 }},
		{"openjd.expr_memory_limit", func(c *config.Config) { c.OpenJD.ExprMemoryLimit = 1 }},
		{"openjd.expr_template_positions", func(c *config.Config) { c.OpenJD.ExprTemplatePositions = 1 }},
		{"openjd.expr_template_retained_bytes", func(c *config.Config) { c.OpenJD.ExprTemplateRetainedBytes = 1 }},
	}
	for _, tc := range tests {
		t.Run(tc.field, func(t *testing.T) {
			cfg := config.DefaultConfig()
			tc.set(&cfg)
			var fields []string
			for _, e := range config.Validate(cfg) {
				fields = append(fields, e.Field)
			}
			if len(fields) != 1 || fields[0] != tc.field {
				t.Fatalf("one out-of-range knob must produce exactly one error naming %s, got %v", tc.field, fields)
			}
		})
	}
}

// ── openjd.expr_* : file and env layers ─────────────────────────────────────

func TestLoad_ExprLimitsFromEnv(t *testing.T) {
	t.Setenv("SQI_OPENJD_EXPR_OPERATION_LIMIT", "12345")
	t.Setenv("SQI_OPENJD_EXPR_MEMORY_LIMIT", "2000000")
	t.Setenv("SQI_OPENJD_EXPR_TEMPLATE_POSITIONS", "4321")
	t.Setenv("SQI_OPENJD_EXPR_TEMPLATE_RETAINED_BYTES", "20000000")

	cfg, err := config.Load("", config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OpenJD.ExprOperationLimit != 12345 {
		t.Errorf("expr_operation_limit = %d, want 12345", cfg.OpenJD.ExprOperationLimit)
	}
	if cfg.OpenJD.ExprMemoryLimit != 2_000_000 {
		t.Errorf("expr_memory_limit = %d, want 2000000", cfg.OpenJD.ExprMemoryLimit)
	}
	if cfg.OpenJD.ExprTemplatePositions != 4321 {
		t.Errorf("expr_template_positions = %d, want 4321", cfg.OpenJD.ExprTemplatePositions)
	}
	if cfg.OpenJD.ExprTemplateRetainedBytes != 20_000_000 {
		t.Errorf("expr_template_retained_bytes = %d, want 20000000", cfg.OpenJD.ExprTemplateRetainedBytes)
	}
}

func TestLoad_ExprLimitsFromFile(t *testing.T) {
	path := t.TempDir() + "/sqi.yaml"
	body := "openjd:\n" +
		"  expr_operation_limit: 2000\n" +
		"  expr_memory_limit: 500000\n" +
		"  expr_template_positions: 500\n" +
		"  expr_template_retained_bytes: 1000000\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OpenJD.ExprOperationLimit != 2000 || cfg.OpenJD.ExprMemoryLimit != 500_000 ||
		cfg.OpenJD.ExprTemplatePositions != 500 || cfg.OpenJD.ExprTemplateRetainedBytes != 1_000_000 {
		t.Fatalf("file overrides not applied: %+v", cfg.OpenJD)
	}
	// EnforceLimits was not in the file and must keep its default.
	if !cfg.OpenJD.EnforceLimits {
		t.Error("openjd.enforce_limits must stay true when the file does not mention it")
	}
}

// TestLoad_ExprLimitsOmittedKeepsDefaults is the "a fresh install changes
// nothing" assertion at the LOADER layer: a config file that mentions openjd
// but not the expr keys must leave all four at their defaults, not at zero.
func TestLoad_ExprLimitsOmittedKeepsDefaults(t *testing.T) {
	path := t.TempDir() + "/sqi.yaml"
	if err := os.WriteFile(path, []byte("openjd:\n  enforce_limits: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	def := config.DefaultConfig()
	if cfg.OpenJD.ExprOperationLimit != def.OpenJD.ExprOperationLimit ||
		cfg.OpenJD.ExprMemoryLimit != def.OpenJD.ExprMemoryLimit ||
		cfg.OpenJD.ExprTemplatePositions != def.OpenJD.ExprTemplatePositions ||
		cfg.OpenJD.ExprTemplateRetainedBytes != def.OpenJD.ExprTemplateRetainedBytes {
		t.Fatalf("omitted expr_* keys must keep defaults, got %+v", cfg.OpenJD)
	}
}

func TestLoad_ExprLimitMalformedEnvIsAnError(t *testing.T) {
	t.Setenv("SQI_OPENJD_EXPR_TEMPLATE_POSITIONS", "lots")
	if _, err := config.Load("", config.FlagOverrides{}); err == nil {
		t.Fatal("a non-numeric SQI_OPENJD_EXPR_TEMPLATE_POSITIONS must be a load error, not a silent default")
	}
}

// TestExampleConfig_ExprLimitsMatchDefaults keeps config/sqi-server.example.yaml
// honest. The example file is what an operator copies, so a value in it that
// drifts from the shipped default silently changes the behavior of anyone who
// starts from it -- and nothing else in this repo reads that file, so nothing
// else would notice. Only the openjd block is asserted; the rest of the example
// deliberately demonstrates non-default values.
func TestExampleConfig_ExprLimitsMatchDefaults(t *testing.T) {
	path := filepath.Join("..", "..", "config", "sqi-server.example.yaml")
	cfg, err := config.Load(path, config.FlagOverrides{})
	if err != nil {
		t.Fatalf("the shipped example config must load: %v", err)
	}
	if errs := config.Validate(cfg); len(errs) != 0 {
		t.Fatalf("the shipped example config must validate: %v", errs)
	}
	if got, want := cfg.OpenJD, config.DefaultConfig().OpenJD; got != want {
		t.Fatalf("config/sqi-server.example.yaml's openjd block = %+v, want the shipped defaults %+v", got, want)
	}
}
