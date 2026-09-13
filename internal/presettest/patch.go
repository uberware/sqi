// SPDX-License-Identifier: AGPL-3.0-or-later

package presettest

import (
	"errors"
	"fmt"
	"strconv"

	"gopkg.in/yaml.v3"
)

// ApplyChunkPatch returns rawTemplate with p applied. The ChunkPatch type is
// declared in snapshot.go -- see Task 2.
//
// It edits the YAML node tree rather than the text: a regex over
// "defaultTaskCount: 1" would hit the wrong step in any template with more than
// one, silently snapshotting a different expansion than the case declares.
func ApplyChunkPatch(rawTemplate string, p ChunkPatch) (string, error) {
	if p.ChunksDefaultTaskCount < 1 {
		return "", fmt.Errorf("presettest: chunk patch for step %q: defaultTaskCount must be >= 1, got %d",
			p.Step, p.ChunksDefaultTaskCount)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(rawTemplate), &doc); err != nil {
		return "", fmt.Errorf("presettest: chunk patch: parse template: %w", err)
	}
	if len(doc.Content) == 0 {
		return "", errors.New("presettest: chunk patch: empty template")
	}

	steps := mappingValue(doc.Content[0], "steps")
	if steps == nil {
		return "", errors.New("presettest: chunk patch: template declares no steps")
	}

	patched := 0
	for _, step := range steps.Content {
		name := mappingValue(step, "name")
		if name == nil || name.Value != p.Step {
			continue
		}
		var err error
		patched, err = applyPatchToStep(step, p)
		if err != nil {
			return "", err
		}
		// First match wins: duplicate step names are rejected by openjd
		// validation, so at most one step can ever match.
		break
	}
	if patched == 0 {
		return "", fmt.Errorf("presettest: chunk patch: no step named %q", p.Step)
	}

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return "", fmt.Errorf("presettest: chunk patch: re-serialize: %w", err)
	}
	return string(out), nil
}

// applyPatchToStep patches a single step node and returns the count of patches applied.
func applyPatchToStep(step *yaml.Node, p ChunkPatch) (int, error) {
	ps := mappingValue(step, "parameterSpace")
	if ps == nil {
		return 0, fmt.Errorf("presettest: chunk patch: step %q has no parameterSpace", p.Step)
	}
	defs := mappingValue(ps, "taskParameterDefinitions")
	if defs == nil {
		return 0, fmt.Errorf("presettest: chunk patch: step %q has no taskParameterDefinitions", p.Step)
	}

	patched := 0
	for _, def := range defs.Content {
		chunks := mappingValue(def, "chunks")
		if chunks == nil {
			continue
		}
		count := mappingValue(chunks, "defaultTaskCount")
		if count == nil {
			return 0, fmt.Errorf("presettest: chunk patch: step %q chunks block has no defaultTaskCount", p.Step)
		}
		count.Value = strconv.Itoa(p.ChunksDefaultTaskCount)
		count.Tag = "!!int"
		patched++
	}
	if patched == 0 {
		return 0, fmt.Errorf("presettest: chunk patch: step %q declares no chunks: block", p.Step)
	}
	return patched, nil
}

// mappingValue returns the value node for key in a mapping node, or nil.
func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}
