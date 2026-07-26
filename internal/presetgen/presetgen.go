// SPDX-License-Identifier: AGPL-3.0-or-later

// Package presetgen generates a preset-library index and definition files from
// the repo's authored presets (presets/sqi/*.yaml) and publishes them into a
// library checkout. It is the publisher counterpart to internal/presetlib (the
// server-side client): both share presetlib.IndexEntry so the index format
// never drifts between what is written and what is read. Not compiled into the
// server or worker — it is a release-time tool (cmd/presetgen).
package presetgen

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/uberware/sqi/internal/fsutil"
	"github.com/uberware/sqi/internal/presetlib"
	"github.com/uberware/sqi/internal/product"
)

// Generated is one published preset: its index entry, the path it is written to
// under the library root (e.g. "sqi/blender-batch-render.yaml"), and the raw
// definition bytes served at that path.
type Generated struct {
	Entry presetlib.IndexEntry
	Path  string
	Data  []byte
}

// Index is the library index document. It mirrors presetlib's internal index
// shape ({"presets": [...]}) using the exported IndexEntry.
type Index struct {
	Presets []presetlib.IndexEntry `json:"presets"`
}

// Build reads every *.yaml in presetsDir, validates it against the product
// schema (a malformed preset is a hard error), and returns one Generated per
// preset sorted by name. definitionDir is the path prefix under the library
// root used for the index's definition field and the published file location.
// The sha256 is computed over the raw file bytes so the served file always
// matches the fingerprint the index vouches for.
func Build(presetsDir, definitionDir string) ([]Generated, error) {
	matches, err := filepath.Glob(filepath.Join(presetsDir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("glob presets: %w", err)
	}
	// A "*.yaml" glob also matches macOS AppleDouble companions ("._foo.yaml"),
	// which are binary and fail to parse as a product definition.
	matches = fsutil.FilterPaths(matches)
	out := make([]Generated, 0, len(matches))
	for _, p := range matches {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		def, err := product.ParseDefinition(data)
		if err != nil {
			return nil, fmt.Errorf("invalid preset %s: %w", p, err)
		}
		definition := path.Join(definitionDir, filepath.Base(p))
		sum := sha256.Sum256(data)
		out = append(out, Generated{
			Entry: presetlib.IndexEntry{
				Name:        def.Name,
				Title:       def.Title,
				Description: def.Description,
				Category:    def.Category,
				Version:     def.Version,
				Definition:  definition,
				Sha256:      hex.EncodeToString(sum[:]),
			},
			Path: definition,
			Data: data,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Entry.Name < out[j].Entry.Name })
	return out, nil
}

// Merge combines the generated entries into an existing index, replacing every
// entry this repo owns (definition under definitionDir/) while preserving all
// others. existingIndex may be nil/empty. The result is sorted by name.
func Merge(existingIndex []byte, generated []Generated, definitionDir string) (Index, error) {
	var idx Index
	if len(existingIndex) > 0 {
		if err := json.Unmarshal(existingIndex, &idx); err != nil {
			return Index{}, fmt.Errorf("parse existing index: %w", err)
		}
	}
	prefix := definitionDir + "/"
	kept := make([]presetlib.IndexEntry, 0, len(idx.Presets)+len(generated))
	for _, e := range idx.Presets {
		if !strings.HasPrefix(e.Definition, prefix) {
			kept = append(kept, e)
		}
	}
	for _, g := range generated {
		kept = append(kept, g.Entry)
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Name < kept[j].Name })
	return Index{Presets: kept}, nil
}

// Publish generates the preset index + definition files from presetsDir and
// writes them into outDir (a checkout of the library repo). The owned
// definitionDir subtree is wiped and rewritten so removed/renamed presets are
// cleaned; the merged index preserves any foreign entries. Only outDir/index.json
// and outDir/<definitionDir>/ are touched.
func Publish(presetsDir, outDir, definitionDir string) error {
	generated, err := Build(presetsDir, definitionDir)
	if err != nil {
		return err
	}
	existing, err := os.ReadFile(filepath.Join(outDir, "index.json"))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read existing index: %w", err)
	}
	idx, err := Merge(existing, generated, definitionDir)
	if err != nil {
		return err
	}

	owned := filepath.Join(outDir, definitionDir)
	if err := os.RemoveAll(owned); err != nil {
		return fmt.Errorf("clean %s: %w", owned, err)
	}
	if err := os.MkdirAll(owned, 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", owned, err)
	}
	for _, g := range generated {
		dst := filepath.Join(outDir, filepath.FromSlash(g.Path))
		if err := os.WriteFile(dst, g.Data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", dst, err)
		}
	}

	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal index: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(outDir, "index.json"), data, 0o644); err != nil {
		return fmt.Errorf("write index: %w", err)
	}
	return nil
}
