// SPDX-License-Identifier: AGPL-3.0-or-later

package capabilities

import (
	"embed"
	"fmt"
	"io/fs"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed builtins/*.yaml
var builtinFS embed.FS

// BuiltinDetectors parses and validates the embedded built-in detectors.
func BuiltinDetectors() ([]Detector, error) {
	entries, err := fs.ReadDir(builtinFS, "builtins")
	if err != nil {
		return nil, err
	}
	var out []Detector
	for _, e := range entries {
		if e.Name()[0] == '.' || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, err := builtinFS.ReadFile("builtins/" + e.Name())
		if err != nil {
			return nil, err
		}
		var d Detector
		if err := yaml.Unmarshal(data, &d); err != nil {
			return nil, fmt.Errorf("builtin %s: %w", e.Name(), err)
		}
		if err := d.Validate(); err != nil {
			return nil, fmt.Errorf("builtin %s: %w", e.Name(), err)
		}
		d.Origin = "builtin:" + d.Tag
		out = append(out, d)
	}
	return out, nil
}
