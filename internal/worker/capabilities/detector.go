// SPDX-License-Identifier: AGPL-3.0-or-later

package capabilities

import (
	"errors"
	"fmt"
	"regexp"

	"gopkg.in/yaml.v3"
)

// EnvCheck matches an environment variable by presence and optional value regex.
type EnvCheck struct {
	Name    string `yaml:"name"`
	Matches string `yaml:"matches"`
}

// UnmarshalYAML accepts either a bare string (env: HFS) or a mapping
// (env: {name: HFS, matches: "..."}).
func (e *EnvCheck) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		e.Name = node.Value
		return nil
	}
	type raw EnvCheck
	return node.Decode((*raw)(e))
}

// Check is a single declarative probe. Exactly one of Exe/PathGlob/Env/Registry
// must be set. OS optionally gates the check to one platform.
type Check struct {
	Exe      string   `yaml:"exe"`
	PathGlob string   `yaml:"path_glob"`
	Env      EnvCheck `yaml:"env"`
	Registry string   `yaml:"registry"`
	OS       string   `yaml:"os"`
}

func (c Check) primitiveCount() int {
	n := 0
	for _, set := range []bool{c.Exe != "", c.PathGlob != "", c.Env.Name != "", c.Registry != ""} {
		if set {
			n++
		}
	}
	return n
}

// VersionSpec extracts a version string from a matched signal via a regex whose
// named group "v" (or first capturing group) is the version.
type VersionSpec struct {
	From string `yaml:"from"`
}

// Detector emits a presence tag (and versioned variants) when any check matches.
type Detector struct {
	Tag     string      `yaml:"tag"`
	Checks  []Check     `yaml:"checks"`
	Version VersionSpec `yaml:"version"`
	Origin  string      `yaml:"-"` // "builtin:<name>" or "custom"; for diagnostics
}

// CapabilitiesConfig is the worker's capability-detection configuration.
type CapabilitiesConfig struct {
	Detect  []Detector `yaml:"detect"`
	Disable []string   `yaml:"disable"`
}

var validOS = map[string]bool{"": true, "linux": true, "darwin": true, "windows": true}

// Validate reports the first structural problem with the detector.
func (d Detector) Validate() error {
	if d.Tag == "" {
		return errors.New("detector: tag is required")
	}
	if len(d.Checks) == 0 {
		return fmt.Errorf("detector %q: at least one check is required", d.Tag)
	}
	for i, c := range d.Checks {
		if c.primitiveCount() != 1 {
			return fmt.Errorf("detector %q check %d: exactly one of exe/path_glob/env/registry required", d.Tag, i)
		}
		if !validOS[c.OS] {
			return fmt.Errorf("detector %q check %d: invalid os %q", d.Tag, i, c.OS)
		}
	}
	if d.Version.From != "" {
		if _, err := regexp.Compile(d.Version.From); err != nil {
			return fmt.Errorf("detector %q: bad version regex: %w", d.Tag, err)
		}
	}
	return nil
}
