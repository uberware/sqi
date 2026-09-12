// SPDX-License-Identifier: AGPL-3.0-or-later

package presettest

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"gopkg.in/yaml.v3"
)

// RegistryPath is the tier registry's location, relative to the repo root.
const RegistryPath = "presets/validation-tiers.yaml"

// Tier1 records a preset's argv-snapshot coverage.
type Tier1 struct {
	// Cases are the case names in this preset's fixture file. Each must have a
	// golden on disk.
	Cases []string `yaml:"cases"`

	// Caveat states what this Tier-1 claim does NOT prove, specifically. A
	// Tier-1 golden proves the template expands to a reviewed command line; it
	// does not prove the vendor's application accepts it. Empty is an error:
	// the honesty requirement is machine-enforced rather than remembered.
	Caveat string `yaml:"caveat"`
}

// TierExec records an execution tier (2 or 3) as a named test case.
type TierExec struct {
	// Case is the full Go subtest name, e.g.
	// "TestPresetTier3/maya-layer-render/default".
	Case string `yaml:"case"`

	// RequiredOn lists the GOOS values on which that case MUST actually run. If
	// it skips on one of them, the registry verification fails. A target that
	// can silently pass while running nothing is worse than no target.
	RequiredOn []string `yaml:"required_on"`
}

// Entry is one preset's or built-in's record.
type Entry struct {
	Name   string    `yaml:"name"`
	Source string    `yaml:"source"`
	Tier   int       `yaml:"tier"`
	Tier1  *Tier1    `yaml:"tier1"`
	Tier2  *TierExec `yaml:"tier2"`
	Tier3  *TierExec `yaml:"tier3"`
}

// Registry is the whole file.
type Registry struct {
	Presets []Entry `yaml:"presets"`
}

// Entry returns the entry named name.
func (r Registry) Entry(name string) (Entry, bool) {
	for _, e := range r.Presets {
		if e.Name == name {
			return e, true
		}
	}
	return Entry{}, false
}

// LoadRegistry reads and validates the tier registry from the repo.
func LoadRegistry() (Registry, error) {
	root, err := RepoRoot()
	if err != nil {
		return Registry{}, err
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(RegistryPath)))
	if err != nil {
		return Registry{}, fmt.Errorf("presettest: read registry: %w", err)
	}
	return ParseRegistry(data)
}

// ParseRegistry parses and validates registry bytes. Exported so the loader's
// own rules can be tested without writing files.
func ParseRegistry(data []byte) (Registry, error) {
	var reg Registry
	if err := yaml.Unmarshal(data, &reg); err != nil {
		return Registry{}, fmt.Errorf("presettest: parse registry: %w", err)
	}
	seen := make(map[string]bool, len(reg.Presets))
	for i, e := range reg.Presets {
		if err := validateEntry(i, e, seen); err != nil {
			return Registry{}, err
		}
		seen[e.Name] = true
	}
	return reg, nil
}

// validateEntry checks one registry entry against the rules ParseRegistry
// enforces. Split out of ParseRegistry solely to keep cyclomatic complexity
// under the project's lint ceiling (cyclop); the checks, their order, and
// every error string are unchanged from the inline version.
func validateEntry(i int, e Entry, seen map[string]bool) error {
	switch {
	case e.Name == "":
		return fmt.Errorf("presettest: registry entry %d has no name", i)
	case seen[e.Name]:
		return fmt.Errorf("presettest: registry declares %q twice", e.Name)
	case e.Source != SourcePresets && e.Source != SourceBuiltins:
		return fmt.Errorf("presettest: %s: unknown source %q", e.Name, e.Source)
	case e.Tier < 1 || e.Tier > 3:
		return fmt.Errorf("presettest: %s: tier %d out of range 1-3", e.Name, e.Tier)
	case e.Tier1 == nil:
		return fmt.Errorf("presettest: %s: every preset needs a tier1 block", e.Name)
	case len(e.Tier1.Cases) == 0:
		return fmt.Errorf("presettest: %s: tier1 names no cases", e.Name)
	case e.Tier1.Caveat == "":
		return fmt.Errorf(
			"presettest: %s: tier1 has no caveat -- state what the argv snapshot does NOT prove", e.Name,
		)
	}
	if e.Tier3 != nil && e.Tier3.Case == "" {
		return fmt.Errorf("presettest: %s: tier3 block names no case", e.Name)
	}
	if e.Tier2 != nil && e.Tier2.Case == "" {
		return fmt.Errorf("presettest: %s: tier2 block names no case", e.Name)
	}
	return nil
}

// ── Case outcome sink ─────────────────────────────────────────────────────────

// Outcome is what happened to one execution-tier case in this test binary run.
type Outcome struct {
	// Ran is true once the case reported anything at all. A false Ran means the
	// case never executed, which is what a registry claim with no test looks
	// like.
	Ran bool
	// Skipped is true when the case reported that it declined to run.
	Skipped bool
	// Reason is the skip reason, for the failure message the registry test
	// prints.
	Reason string
}

var (
	outcomeMu sync.Mutex
	outcomes  = map[string]Outcome{}
)

// RecordOutcome reports that caseName executed (or skipped) in this run.
//
// Execution-tier tests call this as their first act, INCLUDING on the skip
// path, because "a skipped test verifies nothing": the registry test needs to
// tell "skipped here, legitimately" from "this claim is backed by nothing".
func RecordOutcome(caseName string, skipped bool, reason string) {
	outcomeMu.Lock()
	defer outcomeMu.Unlock()
	outcomes[caseName] = Outcome{Ran: true, Skipped: skipped, Reason: reason}
}

// LookupOutcome returns what happened to caseName; the zero value means it
// never reported.
func LookupOutcome(caseName string) Outcome {
	outcomeMu.Lock()
	defer outcomeMu.Unlock()
	return outcomes[caseName]
}
