// SPDX-License-Identifier: AGPL-3.0-or-later

package presettest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/uberware/sqi/internal/product"
	"github.com/uberware/sqi/internal/store"
)

// Source directories the tier registry and the harness know about.
//
// Both exist because "what job types does sqi have?" cannot be answered from
// presets/ alone: internal/product/builtins/ ships seven compiled-in products
// (script, script-powershell, python, container, and the three sequence
// built-ins) that are always present with no preset-library install.
const (
	SourcePresets  = "presets/sqi"
	SourceBuiltins = "internal/product/builtins"
)

// Case is one parameter set a preset is snapshotted under.
type Case struct {
	// Name identifies the case; it is part of the golden filename.
	Name string `yaml:"name"`

	// Params are the job parameter values. Defaults need not be restated.
	Params map[string]string `yaml:"params"`

	// Patch optionally raises one step's chunk task count. See [ChunkPatch]:
	// it is the only permitted deviation from the shipped preset, and the
	// golden header records it.
	Patch *ChunkPatch `yaml:"template_patch"`

	// StubInner selects which commands Tier 3 shadows on PATH.
	//
	// False (the default) shadows the task's own command, so the observed argv
	// has a computed counterpart and assertObservedMatchesComputed applies.
	//
	// True shadows what that command invokes INSIDE instead -- the shape of the
	// `script` built-in, whose /bin/sh is an absolute path the worker execs
	// directly without ever consulting PATH, and of `script-powershell`, where
	// shadowing powershell itself would prove argv delivery and nothing else.
	// For these the computed side is the shell's own argv, so the cross-check
	// cannot apply and ExpectInvocations carries the whole claim.
	//
	// Declared rather than inferred: the previous filepath.IsAbs(command)
	// inference returns FALSE on Windows for "/bin/sh", because a POSIX path
	// carries no volume name.
	StubInner bool `yaml:"stub_inner"`

	// ExpectTasks is the number of tasks this case must expand to. Stated in
	// the fixture rather than derived, so an expansion change is a review
	// event rather than a silently different golden.
	ExpectTasks int `yaml:"expect_tasks"`

	// ExpectInvocations maps a stubbed command's basename to the exact number
	// of times Tier 3 must observe it. Empty means Tier 3 asserts nothing
	// beyond job success.
	ExpectInvocations map[string]int `yaml:"expect_invocations"`

	// ExpectInnerArgv is, for a StubInner case, the exact set of argument
	// vectors the stub must record (order ignored). ExpectInvocations only
	// counts invocations, which for a stub reached through a shell cannot show
	// that a value -- an environment variable the shell expanded -- arrived.
	// Legal only with StubInner: otherwise the observed-vs-computed cross-check
	// already compares argv.
	ExpectInnerArgv [][]string `yaml:"expect_inner_argv"`

	// StubEnv is extra environment for the worker process, and so for the stub
	// every task it runs: the case says what the "vendor application" does
	// (SQI_STUB_EXIT, SQI_STUB_STDOUT, ...). Every key must start SQI_STUB_, so
	// a fixture can configure the stub and nothing else (PATH, for one, is the
	// harness's own).
	StubEnv map[string]string `yaml:"stub_env"`

	// ExpectStatus is the job status Tier 3 must observe: "completed" (the
	// default when empty) or "failed". A case that sets it is an OUTCOME case,
	// which TestPresetOutcomes runs; it says what the job does, not what its
	// argv is.
	ExpectStatus string `yaml:"expect_status"`

	// ExpectReason is a substring of the first task failure reason. Legal only
	// with ExpectStatus "failed".
	ExpectReason string `yaml:"expect_reason"`
}

type caseFile struct {
	Cases []Case `yaml:"cases"`
}

// LoadCases reads a case fixture file.
func LoadCases(path string) ([]Case, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("presettest: read cases %s: %w", path, err)
	}
	var cf caseFile
	if err := yaml.Unmarshal(data, &cf); err != nil {
		return nil, fmt.Errorf("presettest: parse cases %s: %w", path, err)
	}
	if len(cf.Cases) == 0 {
		return nil, fmt.Errorf("presettest: %s declares no cases", path)
	}
	seen := make(map[string]bool, len(cf.Cases))
	for i, c := range cf.Cases {
		if c.Name == "" {
			return nil, fmt.Errorf("presettest: %s case %d has no name", path, i)
		}
		if seen[c.Name] {
			return nil, fmt.Errorf("presettest: %s declares case %q twice", path, c.Name)
		}
		if len(c.ExpectInnerArgv) > 0 && !c.StubInner {
			return nil, fmt.Errorf("presettest: %s case %q declares expect_inner_argv without stub_inner", path, c.Name)
		}
		if err := validateOutcomeFields(c); err != nil {
			return nil, fmt.Errorf("presettest: %s case %q: %w", path, c.Name, err)
		}
		seen[c.Name] = true
	}
	return cf.Cases, nil
}

// Job statuses an outcome case may expect.
const (
	StatusCompleted = string(store.JobStatusCompleted)
	StatusFailed    = string(store.JobStatusFailed)
)

// validateOutcomeFields rejects the outcome keys that cannot mean what they say.
func validateOutcomeFields(c Case) error {
	switch c.ExpectStatus {
	case "", StatusCompleted, StatusFailed:
	default:
		return fmt.Errorf("expect_status %q is not %q or %q", c.ExpectStatus, StatusCompleted, StatusFailed)
	}
	if c.ExpectReason != "" && c.ExpectStatus != StatusFailed {
		return errors.New("expect_reason is legal only with expect_status: failed")
	}
	for k := range c.StubEnv {
		if !strings.HasPrefix(k, "SQI_STUB_") {
			return fmt.Errorf("stub_env key %q must start with SQI_STUB_", k)
		}
	}
	return nil
}

// PresetTemplate returns the raw OpenJD template of one preset or built-in
// product. source must be [SourcePresets] or [SourceBuiltins].
//
// Both go through product.ParseDefinition, which is the same parse the server
// performs, so a preset that would be rejected on upload cannot be snapshotted
// as if it were valid.
func PresetTemplate(source, name string) (string, error) {
	switch source {
	case SourceBuiltins:
		for _, p := range product.Builtins() {
			if p.Name == name {
				return p.Template, nil
			}
		}
		return "", fmt.Errorf("presettest: no built-in product named %q", name)
	case SourcePresets:
		root, err := RepoRoot()
		if err != nil {
			return "", err
		}
		path := filepath.Join(root, filepath.FromSlash(source), name+".yaml")
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("presettest: read preset %s: %w", path, err)
		}
		p, err := product.ParseDefinition(data, product.ValidateOptions{EnforceLimits: true})
		if err != nil {
			return "", fmt.Errorf("presettest: parse preset %s: %w", path, err)
		}
		return p.Template, nil
	default:
		return "", fmt.Errorf("presettest: unknown preset source %q", source)
	}
}

// RepoRoot returns the repository root, derived from this file's compile-time
// path. Tests run with the package directory as the working directory, so a
// relative path would be different for every caller package.
func RepoRoot() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("presettest: runtime.Caller(0) returned no file path")
	}
	// internal/presettest/cases.go → repo root is two directories up.
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..")), nil
}
