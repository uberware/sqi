// SPDX-License-Identifier: AGPL-3.0-or-later

package capabilities

import (
	"errors"
	"fmt"
	"regexp"
	"sort"

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
		if c.Env.Matches != "" {
			if _, err := regexp.Compile(c.Env.Matches); err != nil {
				return fmt.Errorf("detector %q check %d: bad env matches regex: %w", d.Tag, i, err)
			}
		}
	}
	if d.Version.From != "" {
		if _, err := regexp.Compile(d.Version.From); err != nil {
			return fmt.Errorf("detector %q: bad version regex: %w", d.Tag, err)
		}
	}
	return nil
}

// CheckEnv abstracts the host queries a detector needs, so tests inject fakes.
type CheckEnv interface {
	LookPath(file string) (string, bool)
	Glob(pattern string) []string
	Getenv(key string) (string, bool)
	RegistryExists(key string) bool
	GOOS() string
}

// Evaluate runs the detector's checks against env and returns the presence tags
// to advertise: the bare Tag if any check matched, plus Tag-<version> for each
// distinct version captured. Results are de-duplicated and sorted.
//
// Evaluate assumes d has already passed Validate (in particular that
// Version.From and every check's Env.Matches compile); callers (the
// capability-detection loaders) validate detectors before evaluating them.
func (d Detector) Evaluate(env CheckEnv) []string {
	matched := false
	versions := map[string]struct{}{}
	for _, c := range d.Checks {
		if c.OS != "" && c.OS != env.GOOS() {
			continue
		}
		for _, signal := range c.signals(env) {
			matched = true
			if v := d.extractVersion(signal); v != "" {
				versions[v] = struct{}{}
			}
		}
	}
	if !matched {
		return nil
	}
	out := []string{d.Tag}
	for v := range versions {
		out = append(out, d.Tag+"-"+v)
	}
	sort.Strings(out)
	return out
}

// signals returns the matched signal strings for a single check (paths for
// exe/path_glob, the value for env, the key for registry). Empty if no match.
func (c Check) signals(env CheckEnv) []string {
	switch {
	case c.Exe != "":
		if p, ok := env.LookPath(c.Exe); ok {
			return []string{p}
		}
	case c.PathGlob != "":
		return env.Glob(c.PathGlob)
	case c.Env.Name != "":
		if v, ok := env.Getenv(c.Env.Name); ok {
			if c.Env.Matches == "" {
				return []string{v}
			}
			if regexp.MustCompile(c.Env.Matches).MatchString(v) {
				return []string{v}
			}
		}
	case c.Registry != "":
		if env.RegistryExists(c.Registry) {
			return []string{c.Registry}
		}
	}
	return nil
}

func (d Detector) extractVersion(signal string) string {
	if d.Version.From == "" {
		return ""
	}
	re := regexp.MustCompile(d.Version.From)
	m := re.FindStringSubmatch(signal)
	if m == nil {
		return ""
	}
	if i := re.SubexpIndex("v"); i > 0 && i < len(m) {
		return m[i]
	}
	if len(m) > 1 {
		return m[1]
	}
	return ""
}
