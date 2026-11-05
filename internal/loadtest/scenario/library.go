// SPDX-License-Identifier: AGPL-3.0-or-later

package scenario

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
)

//go:embed library/*.yaml
var libraryFS embed.FS

// libraryDir is where the embedded scenario files sit.
const libraryDir = "library"

// BuiltinNames lists the built-in scenarios, sorted.
func BuiltinNames() []string {
	entries, err := fs.ReadDir(libraryFS, libraryDir)
	if err != nil { // the directory is embedded at build time, so this cannot happen
		panic(fmt.Sprintf("scenario: read embedded library: %v", err))
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, strings.TrimSuffix(e.Name(), ".yaml"))
	}
	slices.Sort(names)
	return names
}

// Builtin parses the named built-in scenario, with defaults applied. Each call
// returns a fresh copy the caller may modify.
func Builtin(name string) (*Scenario, error) {
	data, err := fs.ReadFile(libraryFS, path.Join(libraryDir, name+".yaml"))
	if err != nil {
		return nil, fmt.Errorf("no built-in scenario %q (built-ins: %s)", name, strings.Join(BuiltinNames(), ", "))
	}
	s, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("built-in scenario %q: %w", name, err)
	}
	return s, nil
}
