// SPDX-License-Identifier: AGPL-3.0-or-later

package fmtres_test

// Tests for exprlimits.go -- EXPR sub-project E4d's Task 2, the WORKER half
// of "nine fixed Go constants become operator configuration".
//
// Two kinds of test live here, and they are deliberately different in shape:
//
//   - DEFAULTS-ARE-UNCHANGED. Every default must equal the constant it
//     replaced, written as a LITERAL rather than as the constant itself --
//     comparing defaultWorkerOperationLimit to itself would pass whatever it
//     became.
//   - THE KNOB IS OBSERVED. Each of the five limits gets its own test that
//     changes ONLY that value and watches the enforced bound move. "Read but
//     not used" is this wave's characteristic failure and it is invisible to
//     any test that merely checks the value parses; the mutation target for
//     each is named on the test.
//
// The RANGES an operator may choose from are not enforced here (this type
// accepts any positive value, exactly so these tests can set absurd ones) --
// internal/worker/config rejects an out-of-range value at startup, and
// cmd/sqi-worker's TestExprLimitsBounds_MatchFmtres pins the two packages'
// copies of the bounds against each other.

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/uberware/sqi/internal/openjd/expr"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/protocol"

	"github.com/uberware/sqi/internal/fsutil"
)

// TestExprLimits_DefaultsMatchPreE4dConstants pins every default to the
// literal value the package constant carried before E4d. A fresh worker with
// no expr: block in its config file must meter exactly as every release
// before this wave did.
func TestExprLimits_DefaultsMatchPreE4dConstants(t *testing.T) {
	got := fmtres.DefaultExprLimits()
	want := fmtres.ExprLimits{
		OperationLimit:          1_000_000,
		MemoryLimit:             20_000_000,
		AssignmentPositions:     10_000,
		AssignmentRetainedBytes: 20_000_000,
		LetRetainedBytes:        10_000_000,
	}
	if got != want {
		t.Errorf("DefaultExprLimits() = %+v, want the pre-E4d constants %+v", got, want)
	}
	// DefaultAssignmentPositions is exported for internal/openjd's
	// TestTemplateBudget_WorkerCapIsNotTighter (it is the only package that
	// sees both sides of the cross-binary relation). It must agree with the
	// struct default or that test pins the wrong number.
	if fmtres.DefaultAssignmentPositions != want.AssignmentPositions {
		t.Errorf("DefaultAssignmentPositions = %d, want %d",
			fmtres.DefaultAssignmentPositions, want.AssignmentPositions)
	}
}

// TestExprLimits_ZeroValueNormalizesToDefaults pins the property that keeps
// every pre-E4d call site in this package behaving unchanged: a zero
// ExprLimits means "unset, use the defaults", never "unlimited".
func TestExprLimits_ZeroValueNormalizesToDefaults(t *testing.T) {
	tests := []struct {
		name string
		in   fmtres.ExprLimits
		want fmtres.ExprLimits
	}{
		{name: "zero value", in: fmtres.ExprLimits{}, want: fmtres.DefaultExprLimits()},
		{
			name: "negative fields are unset, not unlimited",
			in: fmtres.ExprLimits{
				OperationLimit: -1, MemoryLimit: -1, AssignmentPositions: -1,
				AssignmentRetainedBytes: -1, LetRetainedBytes: -1,
			},
			want: fmtres.DefaultExprLimits(),
		},
		{
			name: "one field set, the rest defaulted",
			in:   fmtres.ExprLimits{AssignmentPositions: 7},
			want: func() fmtres.ExprLimits {
				l := fmtres.DefaultExprLimits()
				l.AssignmentPositions = 7
				return l
			}(),
		},
		{
			name: "all five set are all five kept",
			in: fmtres.ExprLimits{
				OperationLimit: 1, MemoryLimit: 2, AssignmentPositions: 3,
				AssignmentRetainedBytes: 4, LetRetainedBytes: 5,
			},
			want: fmtres.ExprLimits{
				OperationLimit: 1, MemoryLimit: 2, AssignmentPositions: 3,
				AssignmentRetainedBytes: 4, LetRetainedBytes: 5,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := fmtres.NewAssignmentBudget(tc.in).Limits(); got != tc.want {
				t.Errorf("NewAssignmentBudget(%+v).Limits() = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

// ── the five knobs, one test each ───────────────────────────────────────────

// TestExprLimits_AssignmentPositionsIsObserved changes ONLY
// AssignmentPositions and watches the enforced position cap move with it,
// boundary-exactly: the configured number is accepted and the next one is
// not. A test that only tried "tiny" and "huge" would pass with an off-by-one
// in ChargePositions' own comparison.
//
// Mutation target: making ChargePositions compare against
// DefaultAssignmentPositions instead of b.limits.AssignmentPositions.
func TestExprLimits_AssignmentPositionsIsObserved(t *testing.T) {
	const configured = 12

	b := fmtres.NewAssignmentBudget(fmtres.ExprLimits{AssignmentPositions: configured})
	if err := b.ChargePositions(configured, "up to the configured cap"); err != nil {
		t.Fatalf("exactly %d positions must be accepted: %v", configured, err)
	}
	err := b.ChargePositions(1, "one past it")
	if err == nil {
		t.Fatalf("position %d must be rejected at a configured cap of %d", configured+1, configured)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(configured)) {
		t.Errorf("error = %v, want it to name the CONFIGURED cap %d, not the default", err, configured)
	}
	if strings.Contains(err.Error(), strconv.FormatInt(fmtres.DefaultAssignmentPositions, 10)) {
		t.Errorf("error = %v, want it to name the configured cap, not the default %d",
			err, fmtres.DefaultAssignmentPositions)
	}

	// The same charge sequence under the DEFAULT limits is accepted -- so the
	// rejection above is the configuration's doing and nothing else.
	d := fmtres.NewAssignmentBudget(fmtres.ExprLimits{})
	if err := d.ChargePositions(configured+1, "same charge, default limits"); err != nil {
		t.Errorf("%d positions must be accepted under the defaults: %v", configured+1, err)
	}

	// Raised above the default: the cap moves in BOTH directions.
	r := fmtres.NewAssignmentBudget(fmtres.ExprLimits{
		AssignmentPositions: fmtres.DefaultAssignmentPositions + 1,
	})
	if err := r.ChargePositions(fmtres.DefaultAssignmentPositions+1, "one past the default"); err != nil {
		t.Errorf("a raised cap must accept one position past the default: %v", err)
	}
}

// TestExprLimits_AssignmentRetainedBytesIsObserved is the retained-bytes
// counterpart of the test above, isolating that dimension: tightening this
// knob must not change the positions verdict and vice versa (see
// TestExprLimits_KnobsAreIndependent).
//
// Mutation target: making ChargeRetainedBytes compare against a constant
// instead of b.limits.AssignmentRetainedBytes.
func TestExprLimits_AssignmentRetainedBytesIsObserved(t *testing.T) {
	const configured = 4_096

	b := fmtres.NewAssignmentBudget(fmtres.ExprLimits{AssignmentRetainedBytes: configured})
	if err := b.ChargeRetainedBytes(configured, "up to the configured cap"); err != nil {
		t.Fatalf("exactly %d bytes must be accepted: %v", configured, err)
	}
	err := b.ChargeRetainedBytes(1, "one past it")
	if err == nil {
		t.Fatalf("byte %d must be rejected at a configured cap of %d", configured+1, configured)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(configured)) {
		t.Errorf("error = %v, want it to name the CONFIGURED cap %d", err, configured)
	}
	if !strings.Contains(err.Error(), "retain") {
		t.Errorf("error = %v, want it to name the retained-bytes dimension", err)
	}

	d := fmtres.NewAssignmentBudget(fmtres.ExprLimits{})
	if err := d.ChargeRetainedBytes(configured+1, "same charge, default limits"); err != nil {
		t.Errorf("%d bytes must be accepted under the defaults: %v", configured+1, err)
	}
}

// TestExprLimits_KnobsAreIndependent proves the two assignment-wide knobs are
// not secretly the same number: flooring the OTHER one must not change a
// verdict that belongs to the first. Five same-typed int64 fields is exactly
// the shape where a transposed assignment compiles and silently enforces the
// wrong bound.
func TestExprLimits_KnobsAreIndependent(t *testing.T) {
	// A positions charge that the default accepts, under limits whose OTHER
	// four fields are set absurdly tight.
	tight := fmtres.ExprLimits{
		OperationLimit:          fmtres.MinExprOperationLimit,
		MemoryLimit:             fmtres.MinExprMemoryLimit,
		AssignmentRetainedBytes: fmtres.MinExprAssignmentRetainedBytes,
		LetRetainedBytes:        fmtres.MinExprLetRetainedBytes,
	}
	b := fmtres.NewAssignmentBudget(tight)
	if err := b.ChargePositions(fmtres.DefaultAssignmentPositions, "positions only"); err != nil {
		t.Errorf("tightening the other four knobs must not change the positions verdict: %v", err)
	}

	// ... and the mirror image: everything tight except retained bytes.
	tight2 := fmtres.ExprLimits{
		OperationLimit:      fmtres.MinExprOperationLimit,
		MemoryLimit:         fmtres.MinExprMemoryLimit,
		AssignmentPositions: fmtres.MinExprAssignmentPositions,
		LetRetainedBytes:    fmtres.MinExprLetRetainedBytes,
	}
	b2 := fmtres.NewAssignmentBudget(tight2)
	if err := b2.ChargeRetainedBytes(fmtres.MinExprAssignmentRetainedBytes+1, "retained only"); err != nil {
		t.Errorf("tightening the other four knobs must not change the retained-bytes verdict: %v", err)
	}
}

// TestExprLimits_OperationLimitIsObserved drives the section 1.3.10 knob
// through a REAL phase-3 resolution (ResolveActionExpr), because that is the
// only way to see it: the number reaches the evaluator through
// ExprEvalOptions, and an options slice that is built but never passed is
// exactly the defect this wave must not ship.
//
// The construction is byte-CHEAP and operation-EXPENSIVE on purpose, so the
// memory knob cannot be what produces the verdict.
//
// Mutation target: reverting exprEvalOptionsFor's expr.WithOperationLimit
// argument to defaultWorkerOperationLimit.
func TestExprLimits_OperationLimitIsObserved(t *testing.T) {
	const src = `{{ string(len([i for i in range(2000)])) }}`
	action := &protocol.Action{Command: src}

	tests := []struct {
		name       string
		limits     fmtres.ExprLimits
		wantReject bool
	}{
		{name: "default accepts it", limits: fmtres.ExprLimits{}, wantReject: false},
		{name: "tightened hard rejects it", limits: fmtres.ExprLimits{OperationLimit: 100}, wantReject: true},
		{
			name:       "tightened to the configurable floor still accepts it",
			limits:     fmtres.ExprLimits{OperationLimit: fmtres.MinExprOperationLimit},
			wantReject: false,
		},
		{
			name:       "raised to the ceiling accepts it",
			limits:     fmtres.ExprLimits{OperationLimit: fmtres.MaxExprOperationLimit},
			wantReject: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := fmtres.NewAssignmentBudget(tc.limits)
			_, err := fmtres.ResolveActionExpr(action, expr.MapSymbols{}, nil, b)
			if tc.wantReject {
				if err == nil {
					t.Fatalf("limits %+v must reject %s", tc.limits, src)
				}
				if !strings.Contains(err.Error(), "operation limit") {
					t.Errorf("error = %v, want it to name the operation limit", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("limits %+v must accept %s: %v", tc.limits, src, err)
			}
		})
	}
}

// TestExprLimits_MemoryLimitIsObserved is the section 1.3.9 counterpart:
// operation-cheap, byte-heavy, so the operation knob cannot be what decides
// it. The bound must move in BOTH directions -- rejected when tightened below
// the construction's live bytes, accepted when raised above them.
//
// Mutation target: reverting exprEvalOptionsFor's expr.WithMemoryLimit
// argument to defaultWorkerMemoryLimit.
func TestExprLimits_MemoryLimitIsObserved(t *testing.T) {
	// ~3 MB of live string, far under the 20 MB default and far over a
	// tightened 100 KB.
	const src = `{{ "x" * 3000000 }}`
	action := &protocol.Action{Command: src}

	tests := []struct {
		name       string
		limits     fmtres.ExprLimits
		wantReject bool
	}{
		{name: "default (20 MB) accepts it", limits: fmtres.ExprLimits{}, wantReject: false},
		{name: "tightened to 100 KB rejects it", limits: fmtres.ExprLimits{MemoryLimit: 100_000}, wantReject: true},
		{
			name:       "tightened to just under the cost rejects it",
			limits:     fmtres.ExprLimits{MemoryLimit: 2_999_999},
			wantReject: true,
		},
		{
			name:       "raised well above it accepts it",
			limits:     fmtres.ExprLimits{MemoryLimit: 10_000_000},
			wantReject: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := fmtres.NewAssignmentBudget(tc.limits)
			_, err := fmtres.ResolveActionExpr(action, expr.MapSymbols{}, nil, b)
			if tc.wantReject {
				if err == nil {
					t.Fatalf("limits %+v must reject a ~3 MB string", tc.limits)
				}
				if !strings.Contains(err.Error(), "memory limit") {
					t.Errorf("error = %v, want it to name the memory limit", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("limits %+v must accept a ~3 MB string: %v", tc.limits, err)
			}
		})
	}
}

// TestExprLimits_LetRetainedBytesIsObserved drives the PER-TABLE retention
// knob through the real ApplyTaskLet path. It is a different bound from
// AssignmentRetainedBytes (which sums across every table one assignment
// builds), and this test keeps the assignment-wide dimension out of the way
// by leaving it at its default while moving only the per-table one.
//
// Mutation target: reverting evalLetBindings' comparison to
// defaultLetRetainedBytes.
func TestExprLimits_LetRetainedBytesIsObserved(t *testing.T) {
	// One binding retaining ~2 MB: under the 10 MB per-table default, over a
	// tightened 1 MB.
	msg := &protocol.AssignMsg{
		JobName:       "J",
		StepName:      "S",
		StepScriptLet: []string{`big = "x" * 2000000`},
	}

	tests := []struct {
		name       string
		limits     fmtres.ExprLimits
		wantReject bool
	}{
		{name: "default (10 MB) accepts it", limits: fmtres.ExprLimits{}, wantReject: false},
		{name: "tightened to 1 MB rejects it", limits: fmtres.ExprLimits{LetRetainedBytes: 1_000_000}, wantReject: true},
		{
			name:       "raised above the cost accepts it",
			limits:     fmtres.ExprLimits{LetRetainedBytes: 5_000_000},
			wantReject: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := fmtres.NewAssignmentBudget(tc.limits)
			syms, err := fmtres.TaskSymbols(msg, t.TempDir(), "", false, b)
			if err != nil {
				t.Fatalf("TaskSymbols: %v", err)
			}
			err = fmtres.ApplyTaskLet(msg, syms, nil, b)
			if tc.wantReject {
				if err == nil {
					t.Fatalf("limits %+v must reject a ~2 MB let binding", tc.limits)
				}
				if !strings.Contains(err.Error(), "symbol table's") {
					t.Errorf("error = %v, want it to name the per-table retention limit", err)
				}
				if !strings.Contains(err.Error(), strconv.FormatInt(tc.limits.LetRetainedBytes, 10)) {
					t.Errorf("error = %v, want it to name the CONFIGURED limit %d",
						err, tc.limits.LetRetainedBytes)
				}
				return
			}
			if err != nil {
				t.Fatalf("limits %+v must accept a ~2 MB let binding: %v", tc.limits, err)
			}
		})
	}
}

// TestExprLimits_ReachSymbolBuilding pins the one phase-3 evaluation that is
// NOT a format-string resolution: mapPathParamValue, the apply_path_mapping
// call TaskSymbols/EnvSymbols make for every PATH-declared parameter. It is
// metered by the same per-evaluation knobs, and before E4d it was the only
// evaluator in this package a budget never reached at all -- so an operator
// tightening memory would have seen every other position honor it and this
// one silently keep the compiled-in default.
func TestExprLimits_ReachSymbolBuilding(t *testing.T) {
	msg := &protocol.AssignMsg{
		JobName:           "J",
		StepName:          "S",
		JobParameters:     map[string]string{"Scene": "/mnt/show/shot/scene.ma"},
		JobParameterTypes: map[string]string{"Scene": "PATH"},
		Parameters:        map[string]string{},
		ParameterTypes:    map[string]string{},
	}

	// The default accepts it; a 16-byte memory allowance cannot even hold the
	// mapped path, so symbol building itself must fail.
	if _, err := fmtres.TaskSymbols(msg, t.TempDir(), "", false,
		fmtres.NewAssignmentBudget(fmtres.ExprLimits{})); err != nil {
		t.Fatalf("the defaults must build the symbol table: %v", err)
	}
	_, err := fmtres.TaskSymbols(msg, t.TempDir(), "", false,
		fmtres.NewAssignmentBudget(fmtres.ExprLimits{MemoryLimit: 16}))
	if err == nil {
		t.Fatal("a 16-byte memory limit must reach mapPathParamValue and reject the mapping")
	}
	if !strings.Contains(err.Error(), "memory limit") {
		t.Errorf("error = %v, want it to name the memory limit", err)
	}
}

// ── floors, measured against this repo's own reference presets ──────────────

// presetTemplate is the slice of a presets/sqi/*.yaml file this test needs:
// the embedded OpenJD job template's parameters, its steps' script actions,
// embedded files and task parameters. It is decoded HERE rather than through
// internal/openjd because the worker binary must never import that package
// (it pulls internal/store) -- see exprsyms.go's own note. Nothing about the
// measurement needs the full model: a phase-3 assignment is built from an
// AssignMsg, and every field below is one the scheduler copies into it.
type presetTemplate struct {
	Template struct {
		Name                 string `yaml:"name"`
		ParameterDefinitions []struct {
			Name string `yaml:"name"`
			Type string `yaml:"type"`
		} `yaml:"parameterDefinitions"`
		Steps []struct {
			Name           string `yaml:"name"`
			ParameterSpace struct {
				TaskParameterDefinitions []struct {
					Name string `yaml:"name"`
					Type string `yaml:"type"`
				} `yaml:"taskParameterDefinitions"`
			} `yaml:"parameterSpace"`
			Script struct {
				Actions struct {
					OnRun struct {
						Command string   `yaml:"command"`
						Args    []string `yaml:"args"`
					} `yaml:"onRun"`
				} `yaml:"actions"`
				EmbeddedFiles []struct {
					Name string `yaml:"name"`
					Data string `yaml:"data"`
				} `yaml:"embeddedFiles"`
			} `yaml:"script"`
		} `yaml:"steps"`
	} `yaml:"template"`
}

// presetAssignments turns one preset file into the AssignMsg values a worker
// would actually be handed for it -- one per step. Parameter VALUES are
// synthesized (the preset declares parameters, it does not bind them), which
// is the same substitution internal/openjd's own preset test makes: what is
// being measured is the template's SHAPE, not any particular submission.
func presetAssignments(t *testing.T, path string) []*protocol.AssignMsg {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var p presetTemplate
	if err := yaml.Unmarshal(data, &p); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	jobParams := map[string]string{}
	jobTypes := map[string]string{}
	for _, pd := range p.Template.ParameterDefinitions {
		jobParams[pd.Name] = presetParamValue(pd.Type)
		jobTypes[pd.Name] = pd.Type
	}

	var out []*protocol.AssignMsg
	for _, s := range p.Template.Steps {
		taskParams := map[string]string{}
		taskTypes := map[string]string{}
		for _, td := range s.ParameterSpace.TaskParameterDefinitions {
			taskParams[td.Name] = presetParamValue(td.Type)
			taskTypes[td.Name] = td.Type
		}
		files := make([]protocol.EmbeddedFile, 0, len(s.Script.EmbeddedFiles))
		for _, f := range s.Script.EmbeddedFiles {
			files = append(files, protocol.EmbeddedFile{Name: f.Name, Filename: f.Name, Data: f.Data})
		}
		out = append(out, &protocol.AssignMsg{
			JobName:           p.Template.Name,
			StepName:          s.Name,
			JobParameters:     jobParams,
			JobParameterTypes: jobTypes,
			Parameters:        taskParams,
			ParameterTypes:    taskTypes,
			EmbeddedFiles:     files,
			OnRun: &protocol.Action{
				Command: s.Script.Actions.OnRun.Command,
				Args:    s.Script.Actions.OnRun.Args,
			},
		})
	}
	return out
}

// presetParamValue is a plausible submitted value for a declared type --
// realistic in SHAPE (a path looks like a path) but small, because the point
// of the measurement is what the TEMPLATE costs, not what an operator's own
// parameter values cost. A worker's own budget is spent on both; the floors
// only have to leave room for the first.
func presetParamValue(declared string) string {
	switch {
	case strings.HasPrefix(declared, "PATH"):
		return "/mnt/show/seq/shot/scene_file_v012.ext"
	case strings.HasPrefix(declared, "INT"), strings.HasPrefix(declared, "CHUNK[INT]"):
		return "1-10"
	case strings.HasPrefix(declared, "FLOAT"):
		return "1.5"
	default:
		return "1-10"
	}
}

// resolvePresetErrors runs one preset assignment through the REAL phase-3
// pipeline under lim and returns the set of errors it produced. It is the
// measurement primitive both preset tests below are built from.
//
// Every stage a worker actually runs for a task is included -- symbol table,
// let bindings, command and args, embedded file data -- because a floor that
// only protects one stage is not a floor.
func resolvePresetErrors(msg *protocol.AssignMsg, workDir string, lim fmtres.ExprLimits) []string {
	var errs []string
	b := fmtres.NewAssignmentBudget(lim)
	syms, err := fmtres.TaskSymbols(msg, workDir, "", false, b)
	if err != nil {
		return []string{"symbols: " + err.Error()}
	}
	if err := fmtres.ApplyTaskLet(msg, syms, nil, b); err != nil {
		errs = append(errs, "let: "+err.Error())
	}
	if _, err := fmtres.ResolveActionExpr(msg.OnRun, syms, nil, b); err != nil {
		errs = append(errs, "action: "+err.Error())
	}
	if _, err := fmtres.ResolveEmbeddedFilesExpr(msg.EmbeddedFiles, syms, nil, b); err != nil {
		errs = append(errs, "files: "+err.Error())
	}
	return errs
}

// presetFiles lists the reference presets this repo ships.
func presetFiles(t *testing.T) []string {
	t.Helper()
	paths, err := fsutil.Glob(filepath.Join("..", "..", "..", "presets", "sqi", "*.yaml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no reference presets found (glob err=%v): the floors are sized against them", err)
	}
	return paths
}

// TestExprLimits_FloorsAcceptReferencePresets is the floors' justification,
// re-measured on every run rather than trusted from a comment.
//
// It DELIBERATELY MATCHES NO MESSAGE TEXT. It computes each preset's error
// set under the DEFAULTS as a baseline and fails on any error the FLOORS add.
// A message-matching version of this test (which is what E4d Task 1 shipped
// first, and had to fix) silently ignores every dimension whose error it does
// not happen to match; a set difference cannot.
//
// The baseline is needed rather than "expect zero errors" because these
// presets are base-spec templates being pushed through the EXPR evaluator:
// six of the fourteen reference a chunked task parameter's .Start property
// ("{{Task.Param.Frame.Start}}", a TASK_CHUNKING spelling the EXPR evaluator
// has no property for), which is an unrelated error and not this test's
// business. It does mean those six stop resolving at the offending args
// entry, so their measured POSITION cost below is a lower bound on what the
// whole step would charge -- which does not weaken the conclusion, because the
// floor is sized from E4c's 1,841-position worked session rather than from
// these numbers.
func TestExprLimits_FloorsAcceptReferencePresets(t *testing.T) {
	floors := fmtres.ExprLimits{
		OperationLimit:          fmtres.MinExprOperationLimit,
		MemoryLimit:             fmtres.MinExprMemoryLimit,
		AssignmentPositions:     fmtres.MinExprAssignmentPositions,
		AssignmentRetainedBytes: fmtres.MinExprAssignmentRetainedBytes,
		LetRetainedBytes:        fmtres.MinExprLetRetainedBytes,
	}
	defaults := fmtres.DefaultExprLimits()

	for _, path := range presetFiles(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			for _, msg := range presetAssignments(t, path) {
				base := resolvePresetErrors(msg, t.TempDir(), defaults)
				tightened := resolvePresetErrors(msg, t.TempDir(), floors)
				for _, e := range tightened {
					if !slices.Contains(base, e) {
						t.Errorf("step %q: the configurable FLOORS reject work the defaults accept: %s",
							msg.StepName, e)
					}
				}
			}
		})
	}
}

// TestExprLimits_PresetCostsLeaveFloorHeadroom is the other half: it measures,
// per dimension and per preset, the SMALLEST value at which the preset still
// produces exactly its baseline error set (binary search), and fails if any
// floor is within 4x of a measured cost. That turns the floors from asserted
// numbers into measured ones and makes a future preset that grows toward a
// floor a build failure rather than a surprise.
//
// The table is one row per knob, so adding a sixth knob without extending it
// is visible rather than silent.
func TestExprLimits_PresetCostsLeaveFloorHeadroom(t *testing.T) {
	const headroom = 4

	dims := []struct {
		name  string
		floor int64
		set   func(*fmtres.ExprLimits, int64)
	}{
		{
			name: "OperationLimit", floor: fmtres.MinExprOperationLimit,
			set: func(l *fmtres.ExprLimits, v int64) { l.OperationLimit = v },
		},
		{
			name: "MemoryLimit", floor: fmtres.MinExprMemoryLimit,
			set: func(l *fmtres.ExprLimits, v int64) { l.MemoryLimit = v },
		},
		{
			name: "AssignmentPositions", floor: fmtres.MinExprAssignmentPositions,
			set: func(l *fmtres.ExprLimits, v int64) { l.AssignmentPositions = v },
		},
		{
			name: "AssignmentRetainedBytes", floor: fmtres.MinExprAssignmentRetainedBytes,
			set: func(l *fmtres.ExprLimits, v int64) { l.AssignmentRetainedBytes = v },
		},
		{
			name: "LetRetainedBytes", floor: fmtres.MinExprLetRetainedBytes,
			set: func(l *fmtres.ExprLimits, v int64) { l.LetRetainedBytes = v },
		},
	}

	for _, path := range presetFiles(t) {
		for _, msg := range presetAssignments(t, path) {
			base := resolvePresetErrors(msg, t.TempDir(), fmtres.DefaultExprLimits())
			for _, d := range dims {
				need := minAcceptable(t, msg, base, d.set)
				t.Logf("%s step %q: %s costs %d (floor %d)",
					filepath.Base(path), msg.StepName, d.name, need, d.floor)
				if need*headroom > d.floor {
					t.Errorf("%s step %q: %s costs %d, within %dx of the floor %d -- "+
						"raise the floor or shrink the preset",
						filepath.Base(path), msg.StepName, d.name, need, headroom, d.floor)
				}
			}
		}
	}
}

// minAcceptable binary-searches the smallest value of ONE dimension at which
// msg still resolves to exactly its baseline error set. Every other dimension
// stays at its default, so the number returned belongs to this dimension
// alone.
func minAcceptable(
	t *testing.T, msg *protocol.AssignMsg, base []string, set func(*fmtres.ExprLimits, int64),
) int64 {
	t.Helper()
	ok := func(v int64) bool {
		lim := fmtres.DefaultExprLimits()
		set(&lim, v)
		got := resolvePresetErrors(msg, t.TempDir(), lim)
		if len(got) != len(base) {
			return false
		}
		for _, e := range got {
			if !slices.Contains(base, e) {
				return false
			}
		}
		return true
	}
	lo, hi := int64(1), int64(64_000_000)
	if !ok(hi) {
		t.Fatalf("step %q does not resolve to its baseline even at %d", msg.StepName, hi)
	}
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
