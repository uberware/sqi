// SPDX-License-Identifier: AGPL-3.0-or-later

// Command presetgen publishes the repo's authored presets (presets/dcc/*.yaml)
// into a checkout of the preset-library repo: it writes the definition files
// under a namespaced subdir and merges their entries into index.json, leaving
// any foreign entries untouched. It is a release-time tool (see the
// publish-presets job in .github/workflows/release.yml), not a shipped binary.
//
// Usage:
//
//	presetgen -presets presets/dcc -out /path/to/sqi-presets [-subdir dcc]
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/uberware/sqi/internal/presetgen"
)

func main() {
	presetsDir := flag.String("presets", "presets/dcc", "directory of authored preset YAML files")
	outDir := flag.String("out", "", "preset-library checkout directory to write into (required)")
	subdir := flag.String("subdir", "dcc", "definition path prefix under the library root")
	flag.Parse()

	if *outDir == "" {
		fmt.Fprintln(os.Stderr, "presetgen: -out is required")
		flag.Usage()
		os.Exit(2)
	}
	if err := presetgen.Publish(*presetsDir, *outDir, *subdir); err != nil {
		fmt.Fprintf(os.Stderr, "presetgen: %v\n", err)
		os.Exit(1)
	}
}
