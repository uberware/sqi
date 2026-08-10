// SPDX-License-Identifier: AGPL-3.0-or-later

package config

// Tests for the expr: section -- EXPR sub-project E4d's Task 2.
//
// Three properties, and the third is the one that is easy to get wrong:
//
//  1. the defaults are the pre-E4d constants, written as literals;
//  2. a value set in the file or the environment actually arrives;
//  3. an out-of-range value is REJECTED, boundary-exactly. A test that only
//     tries 1 and 10^9 passes with an off-by-one at either end, so every knob
//     is walked at negative / zero / floor-1 / floor / ceiling / ceiling+1.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// defaultExprConfig is the expr: block a worker with no configuration runs
// with, spelled as LITERALS rather than as the Default* constants -- comparing
// a constant to itself would pass whatever it became. These five numbers are
// the fixed limits every release before E4d compiled in.
var defaultExprConfig = ExprConfig{
	OperationLimit:          1_000_000,
	MemoryLimit:             20_000_000,
	AssignmentPositions:     10_000,
	AssignmentRetainedBytes: 20_000_000,
	LetRetainedBytes:        10_000_000,
}

func TestDefault_ExprLimits(t *testing.T) {
	if got := Default().Expr; got != defaultExprConfig {
		t.Errorf("Default().Expr = %+v, want the pre-E4d constants %+v", got, defaultExprConfig)
	}
	if errs := Validate(Default()); len(errs) != 0 {
		t.Errorf("the built-in defaults must be in range: %v", errs)
	}
}

// TestLoad_ExprLimitsFromFile checks that each key arrives from YAML with a
// DISTINCT value: five same-typed int64 fields is exactly the shape where a
// transposed yaml tag loads cleanly and enforces the wrong bound.
func TestLoad_ExprLimitsFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sqi-worker.yaml")
	body := `
nats:
  url: "nats://localhost:4222"
expr:
  operation_limit: 11111
  memory_limit: 2222222
  assignment_positions: 3333
  assignment_retained_bytes: 4444444
  let_retained_bytes: 5555555
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path, FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := ExprConfig{
		OperationLimit:          11111,
		MemoryLimit:             2222222,
		AssignmentPositions:     3333,
		AssignmentRetainedBytes: 4444444,
		LetRetainedBytes:        5555555,
	}
	if cfg.Expr != want {
		t.Errorf("loaded expr block = %+v, want %+v", cfg.Expr, want)
	}
}

// TestLoad_ExprLimitsOmittedKeepsDefaults pins the failure mode a partial
// YAML block produces if the overlay is written carelessly: mentioning expr:
// at all must not zero the keys it does not name.
func TestLoad_ExprLimitsOmittedKeepsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sqi-worker.yaml")
	body := `
nats:
  url: "nats://localhost:4222"
expr:
  operation_limit: 50000
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path, FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Expr.OperationLimit != 50_000 {
		t.Errorf("operation_limit = %d, want the configured 50000", cfg.Expr.OperationLimit)
	}
	want := defaultExprConfig
	want.OperationLimit = 50_000
	if cfg.Expr != want {
		t.Errorf("expr block = %+v, want the other four at their defaults %+v", cfg.Expr, want)
	}
}

func TestLoad_ExprLimitsFromEnv(t *testing.T) {
	tests := []struct {
		env  string
		val  string
		want int64
		get  func(ExprConfig) int64
	}{
		{
			env: "SQI_WORKER_EXPR_OPERATION_LIMIT", val: "12345", want: 12345,
			get: func(c ExprConfig) int64 { return c.OperationLimit },
		},
		{
			env: "SQI_WORKER_EXPR_MEMORY_LIMIT", val: "2345678", want: 2345678,
			get: func(c ExprConfig) int64 { return c.MemoryLimit },
		},
		{
			env: "SQI_WORKER_EXPR_ASSIGNMENT_POSITIONS", val: "3456", want: 3456,
			get: func(c ExprConfig) int64 { return c.AssignmentPositions },
		},
		{
			env: "SQI_WORKER_EXPR_ASSIGNMENT_RETAINED_BYTES", val: "4567890", want: 4567890,
			get: func(c ExprConfig) int64 { return c.AssignmentRetainedBytes },
		},
		{
			env: "SQI_WORKER_EXPR_LET_RETAINED_BYTES", val: "5678901", want: 5678901,
			get: func(c ExprConfig) int64 { return c.LetRetainedBytes },
		},
	}
	for _, tc := range tests {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv("SQI_WORKER_NATS_URL", "nats://localhost:4222")
			t.Setenv(tc.env, tc.val)
			cfg, err := Load("", FlagOverrides{})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := tc.get(cfg.Expr); got != tc.want {
				t.Errorf("%s=%s produced %d, want %d", tc.env, tc.val, got, tc.want)
			}
			// The other four must be untouched -- one env var must not move
			// a neighboring knob.
			for _, other := range tests {
				if other.env == tc.env {
					continue
				}
				if got, want := other.get(cfg.Expr), other.get(Default().Expr); got != want {
					t.Errorf("%s also changed the knob behind %s: %d, want %d",
						tc.env, other.env, got, want)
				}
			}
		})
	}
}

// TestLoad_ExprMalformedEnvKeepsLowerLayer pins the documented behavior of a
// non-numeric value: it is ignored, exactly as every other numeric env var in
// this package handles one, and Validate still judges whatever value results.
func TestLoad_ExprMalformedEnvKeepsLowerLayer(t *testing.T) {
	t.Setenv("SQI_WORKER_NATS_URL", "nats://localhost:4222")
	t.Setenv("SQI_WORKER_EXPR_OPERATION_LIMIT", "not-a-number")
	cfg, err := Load("", FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Expr.OperationLimit != DefaultWorkerExprOperationLimit {
		t.Errorf("a malformed env value must leave the default in place, got %d",
			cfg.Expr.OperationLimit)
	}
}

// TestValidate_ExprLimits walks six boundary rows per knob. The floor and the
// ceiling must be ACCEPTED and one step outside each REJECTED, or an
// off-by-one in the comparison goes unnoticed.
func TestValidate_ExprLimits(t *testing.T) {
	knobs := []struct {
		key      string
		env      string
		min, max int64
		set      func(*ExprConfig, int64)
	}{
		{
			key: "expr.operation_limit", env: "SQI_WORKER_EXPR_OPERATION_LIMIT",
			min: MinWorkerExprOperationLimit, max: MaxWorkerExprOperationLimit,
			set: func(c *ExprConfig, v int64) { c.OperationLimit = v },
		},
		{
			key: "expr.memory_limit", env: "SQI_WORKER_EXPR_MEMORY_LIMIT",
			min: MinWorkerExprMemoryLimit, max: MaxWorkerExprMemoryLimit,
			set: func(c *ExprConfig, v int64) { c.MemoryLimit = v },
		},
		{
			key: "expr.assignment_positions", env: "SQI_WORKER_EXPR_ASSIGNMENT_POSITIONS",
			min: MinWorkerExprAssignmentPositions, max: MaxWorkerExprAssignmentPositions,
			set: func(c *ExprConfig, v int64) { c.AssignmentPositions = v },
		},
		{
			key: "expr.assignment_retained_bytes", env: "SQI_WORKER_EXPR_ASSIGNMENT_RETAINED_BYTES",
			min: MinWorkerExprAssignmentRetainedBytes, max: MaxWorkerExprAssignmentRetainedBytes,
			set: func(c *ExprConfig, v int64) { c.AssignmentRetainedBytes = v },
		},
		{
			key: "expr.let_retained_bytes", env: "SQI_WORKER_EXPR_LET_RETAINED_BYTES",
			min: MinWorkerExprLetRetainedBytes, max: MaxWorkerExprLetRetainedBytes,
			set: func(c *ExprConfig, v int64) { c.LetRetainedBytes = v },
		},
	}

	for _, k := range knobs {
		rows := []struct {
			name       string
			value      int64
			wantReject bool
		}{
			{name: "negative", value: -1, wantReject: true},
			{name: "zero", value: 0, wantReject: true},
			{name: "one below the floor", value: k.min - 1, wantReject: true},
			{name: "the floor", value: k.min, wantReject: false},
			{name: "the ceiling", value: k.max, wantReject: false},
			{name: "one above the ceiling", value: k.max + 1, wantReject: true},
		}
		for _, r := range rows {
			t.Run(k.key+"/"+r.name, func(t *testing.T) {
				cfg := Default()
				cfg.NATS.URL = "nats://localhost:4222"
				k.set(&cfg.Expr, r.value)
				errs := Validate(cfg)

				var found *ValidationError
				for i := range errs {
					if errs[i].Field == k.key {
						found = &errs[i]
					}
				}
				if !r.wantReject {
					if found != nil {
						t.Fatalf("%s = %d must be accepted, got %v", k.key, r.value, *found)
					}
					return
				}
				if found == nil {
					t.Fatalf("%s = %d must be rejected; errors were %v", k.key, r.value, errs)
				}
				// The message has to be actionable on its own: the operator
				// needs the bound, what they set, and where to change it.
				for _, want := range []string{
					k.env, k.key,
					strconv.FormatInt(k.min, 10), strconv.FormatInt(k.max, 10), strconv.FormatInt(r.value, 10),
				} {
					if !strings.Contains(found.Message, want) {
						t.Errorf("message %q does not mention %q", found.Message, want)
					}
				}
			})
		}
	}
}

// TestValidate_ExprLimitsAreIndependent pins that one out-of-range knob
// produces exactly one error naming itself -- not a blanket "expr" error, and
// not four extra errors for the four valid knobs.
func TestValidate_ExprLimitsAreIndependent(t *testing.T) {
	cfg := Default()
	cfg.NATS.URL = "nats://localhost:4222"
	cfg.Expr.AssignmentPositions = 1

	errs := Validate(cfg)
	if len(errs) != 1 {
		t.Fatalf("one bad knob must produce exactly one error, got %d: %v", len(errs), errs)
	}
	if errs[0].Field != "expr.assignment_positions" {
		t.Errorf("error names %q, want expr.assignment_positions", errs[0].Field)
	}
}

// TestExampleConfig_ExprLimitsMatchDefaults keeps config/sqi-worker.example.yaml
// honest. The example is what an operator copies, so a value in it that drifts
// from the shipped default silently changes the behavior of everyone who
// starts from it -- and nothing else in this repo reads that file, so nothing
// else would notice.
func TestExampleConfig_ExprLimitsMatchDefaults(t *testing.T) {
	path := filepath.Join("..", "..", "..", "config", "sqi-worker.example.yaml")
	cfg, err := Load(path, FlagOverrides{})
	if err != nil {
		t.Fatalf("the shipped example config must load: %v", err)
	}
	if errs := validateExpr(cfg.Expr); len(errs) != 0 {
		t.Fatalf("the shipped example's expr block must validate: %v", errs)
	}
	if cfg.Expr != defaultExprConfig {
		t.Errorf("config/sqi-worker.example.yaml's expr block = %+v, want the shipped defaults %+v",
			cfg.Expr, defaultExprConfig)
	}
}
