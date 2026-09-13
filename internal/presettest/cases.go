// SPDX-License-Identifier: AGPL-3.0-or-later

package presettest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"gopkg.in/yaml.v3"

	"github.com/uberware/sqi/internal/product"
)

// Source directories the tier registry and the harness know about.
//
// Both exist because "what job types does sqi have?" cannot be answered from
// presets/ alone: internal/product/builtins/ ships three compiled-in products
// (script, python, container) that are always present with no preset-library
// install. Phase 4's own preset survey surveyed presets/ only and produced a
// wrong headline finding.
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

	// ExpectTasks is the number of tasks this case must expand to. Stated in
	// the fixture rather than derived, so an expansion change is a review
	// event rather than a silently different golden.
	ExpectTasks int `yaml:"expect_tasks"`

	// ExpectInvocations maps a stubbed command's basename to the exact number
	// of times Tier 3 must observe it. Empty means Tier 3 asserts nothing
	// beyond job success.
	ExpectInvocations map[string]int `yaml:"expect_invocations"`
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
		seen[c.Name] = true
	}
	return cf.Cases, nil
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
