// SPDX-License-Identifier: AGPL-3.0-or-later

package capabilities

import (
	"fmt"
	"path/filepath"
	"regexp"
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
	// Version, when set, is used instead of the detector's own version regex
	// to read a version from each match. It only chooses the newest install and
	// labels the result: it never adds a version tag. A detector whose tag is
	// presence only (Blender) uses it to pick between installs.
	Version VersionSpec `yaml:"version"`
	// Args are arguments the application's own invocation requires on an OS,
	// keyed by GOOS (linux, darwin or windows), put before the arguments a job
	// supplies: a flag without which the program opens a window or waits for
	// input on a headless worker. They are literal strings, not format strings.
	// They must never be a license flag: which license a worker holds is the
	// operator's to state in worker.commands.
	Args map[string][]string `yaml:"args"`
}

// DetectedCommand is an executable a detector found for a command name.
type DetectedCommand struct {
	Name string
	// Path is empty when the detector has arguments for this OS but no install
	// was found: the arguments still apply to a command the host provides.
	Path string
	// Args are the detector's arguments for this OS; see [CommandSpec.Args].
	Args []string
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
	if s.Version.From != "" {
		if _, err := regexp.Compile(s.Version.From); err != nil {
			return fmt.Errorf("detector %q: command %q: bad version regex: %w", tag, s.Name, err)
		}
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
	return s.validateArgs(tag)
}

// validateArgs checks Args the way an operator's worker.commands arguments are
// meant to read: keyed by a real platform, and no empty argument, which would
// reach the program as an empty string.
func (s CommandSpec) validateArgs(tag string) error {
	for goos, args := range s.Args {
		if goos == "" || !validOS[goos] {
			return fmt.Errorf("detector %q: command %q args: invalid os %q (want linux, darwin or windows)", tag, s.Name, goos)
		}
		for i, a := range args {
			if a == "" {
				return fmt.Errorf("detector %q: command %q args for %s: argument %d is empty", tag, s.Name, goos, i)
			}
		}
	}
	return nil
}

// FindCommand returns the executable to run for the detector's command: the
// newest version among the installs its globs match on env's platform. Two
// installs of one version (Nuke's 17.1v1 and 17.1v2 both run as Nuke17.1) are
// ordered by path, so the later release directory wins. It reports false when
// the detector has no command, or nothing matches and it has no arguments for
// this platform. With arguments and no match it reports them with an empty Path.
func (d Detector) FindCommand(env CheckEnv) (DetectedCommand, bool) {
	if d.Command == nil {
		return DetectedCommand{}, false
	}
	args := slices.Clone(d.Command.Args[env.GOOS()])
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
		if len(args) > 0 {
			return DetectedCommand{Name: d.Command.Name, Args: args}, true
		}
		return DetectedCommand{}, false
	}
	versionOf := d.extractVersion
	if from := d.Command.Version.From; from != "" {
		versionOf = func(p string) string { return extractVersionFrom(from, p) }
	}
	best := paths[0]
	bestVersion := versionOf(best)
	for _, p := range paths[1:] {
		v := versionOf(p)
		if c := compareVersions(v, bestVersion); c > 0 || (c == 0 && p > best) {
			best, bestVersion = p, v
		}
	}
	return DetectedCommand{Name: d.Command.Name, Path: best, Args: args, Version: bestVersion}, true
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

// ApplyConfiguredCommands advertises the tag of every detector whose command
// the operator mapped in worker.commands, and returns the tags it added, sorted.
//
// A worker.commands entry says where the software is, so it is as good a
// signal as a glob match: without it, an operator whose install the globs do
// not search would have to add the tag by hand as well. mapped resolves a
// command name to the path the operator gave, and reports false when there is
// no entry or its target does not exist on this host — a mapping to something
// missing advertises nothing.
//
// A tag already present, whatever its value, is left alone, and the detector's
// version tag with it: capability_tags: ["maya=false"] is how an operator turns
// a tag off, and a mapping must not turn it back on.
func (c *Capabilities) ApplyConfiguredCommands(detectors []Detector, mapped func(name string) (path string, ok bool)) []string {
	if c.Tags == nil {
		c.Tags = make(map[string]string)
	}
	var added []string
	for _, d := range detectors {
		if d.Command == nil {
			continue
		}
		path, ok := mapped(d.Command.Name)
		if !ok {
			continue
		}
		if _, exists := c.Tags[d.Tag]; exists {
			continue
		}
		tags := []string{d.Tag}
		if v := d.extractVersion(path); v != "" {
			tags = append(tags, versionTag(d.Tag, v))
		}
		for _, tag := range tags {
			if _, exists := c.Tags[tag]; !exists {
				c.Tags[tag] = "true"
				added = append(added, tag)
			}
		}
	}
	slices.Sort(added)
	return added
}
