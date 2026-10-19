// SPDX-License-Identifier: AGPL-3.0-or-later

package capabilities

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// CommandGlob locates the executable of detected software, optionally on one
// platform only.
type CommandGlob struct {
	PathGlob string `yaml:"path_glob"`
	OS       string `yaml:"os"`
}

// CommandSpec says how to run the software a detector finds: the command name
// the presets use, and where its executable lives.
//
// A tag only says the software is installed. The presets run a bare command
// ("nuke"), and an install often has no command of that name: Nuke is
// Nuke17.1.exe, and the installer adds nothing to PATH. A CommandSpec lets the
// worker fill that gap with the newest install it finds, unless the operator or
// the host already provides the command.
type CommandSpec struct {
	// Name is the command name a job runs, as written in the preset.
	Name string `yaml:"name"`
	// Globs match the executable itself, not the install directory: on macOS
	// that is inside the .app bundle.
	Globs []CommandGlob `yaml:"globs"`
}

// DetectedCommand is an executable a detector found for a command name.
type DetectedCommand struct {
	Name string
	Path string
	// Version is read from Path by the detector's version regex; empty when it
	// has none or Path does not carry one.
	Version string
}

func (s CommandSpec) validate(tag string) error {
	if s.Name == "" || strings.ContainsAny(s.Name, `/\`) || strings.TrimSpace(s.Name) != s.Name {
		return fmt.Errorf("detector %q: command name %q must be a bare command name", tag, s.Name)
	}
	if len(s.Globs) == 0 {
		return fmt.Errorf("detector %q: command %q needs at least one glob", tag, s.Name)
	}
	for i, g := range s.Globs {
		if g.PathGlob == "" {
			return fmt.Errorf("detector %q: command %q glob %d: path_glob is required", tag, s.Name, i)
		}
		if _, err := filepath.Match(g.PathGlob, ""); err != nil {
			return fmt.Errorf("detector %q: command %q glob %d: bad path_glob %q: %w", tag, s.Name, i, g.PathGlob, err)
		}
		if !validOS[g.OS] {
			return fmt.Errorf("detector %q: command %q glob %d: invalid os %q", tag, s.Name, i, g.OS)
		}
	}
	return nil
}

// FindCommand returns the executable to run for the detector's command: the
// newest version among the installs its globs match on env's platform. Two
// installs of one version (Nuke's 17.1v1 and 17.1v2 both run as Nuke17.1) are
// ordered by path, so the later release directory wins. It reports false when
// the detector has no command or nothing matches.
func (d Detector) FindCommand(env CheckEnv) (DetectedCommand, bool) {
	if d.Command == nil {
		return DetectedCommand{}, false
	}
	var paths []string
	for _, g := range d.Command.Globs {
		if g.OS != "" && g.OS != env.GOOS() {
			continue
		}
		for _, p := range env.Glob(g.PathGlob) {
			if !slices.Contains(paths, p) {
				paths = append(paths, p)
			}
		}
	}
	if len(paths) == 0 {
		return DetectedCommand{}, false
	}
	best := paths[0]
	bestVersion := d.extractVersion(best)
	for _, p := range paths[1:] {
		v := d.extractVersion(p)
		if c := compareVersions(v, bestVersion); c > 0 || (c == 0 && p > best) {
			best, bestVersion = p, v
		}
	}
	return DetectedCommand{Name: d.Command.Name, Path: best, Version: bestVersion}, true
}

// DetectCommands returns the commands the active detectors (see
// [LoadDetectors]) found on env, one per command name, sorted by name. When two
// detectors name the same command, the first (a built-in) wins.
func DetectCommands(cfg CapabilitiesConfig, env CheckEnv) ([]DetectedCommand, error) {
	detectors, err := LoadDetectors(cfg)
	if err != nil {
		return nil, err
	}
	var out []DetectedCommand
	seen := map[string]bool{}
	for _, d := range detectors {
		c, ok := d.FindCommand(env)
		if !ok || seen[c.Name] {
			continue
		}
		seen[c.Name] = true
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b DetectedCommand) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// compareVersions orders dotted numeric versions ("9.0" < "16.0"). A missing or
// unparsable version sorts before any parsed one; a missing component counts as
// zero, so "17" equals "17.0".
func compareVersions(a, b string) int {
	pa, oka := parseVersion(a)
	pb, okb := parseVersion(b)
	switch {
	case !oka && !okb:
		return 0
	case !oka:
		return -1
	case !okb:
		return 1
	}
	for i := range max(len(pa), len(pb)) {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func parseVersion(v string) ([]int, bool) {
	if v == "" {
		return nil, false
	}
	var out []int
	for part := range strings.SplitSeq(v, ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}
