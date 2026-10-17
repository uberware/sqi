// SPDX-License-Identifier: AGPL-3.0-or-later

package presettest

import (
	"errors"
	"fmt"
	"strconv"

	"gopkg.in/yaml.v3"
)

// ApplyChunkPatch returns rawTemplate with p applied. The ChunkPatch type is
// declared in snapshot.go.
//
// It edits the YAML node tree rather than the text, so that in a template with
// several steps only the named step's defaultTaskCount changes.
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

// StripOSFamilyGate returns rawTemplate with every attr.worker.os.family host
// requirement removed, so a worker of any OS family can lease its steps.
// Everything else, the embedded files included, is kept. A template with no
// such requirement is an error, so a caller never runs an unchanged template
// believing it stripped.
func StripOSFamilyGate(rawTemplate string) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(rawTemplate), &doc); err != nil {
		return "", fmt.Errorf("presettest: strip os.family: parse template: %w", err)
	}
	if len(doc.Content) == 0 {
		return "", errors.New("presettest: strip os.family: empty template")
	}
	removed := 0
	if steps := mappingValue(doc.Content[0], "steps"); steps != nil {
		for _, step := range steps.Content {
			removed += stripStepOSFamily(step)
		}
	}
	if removed == 0 {
		return "", errors.New("presettest: strip os.family: the template has no attr.worker.os.family requirement")
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return "", fmt.Errorf("presettest: strip os.family: re-serialize: %w", err)
	}
	return string(out), nil
}

// stripStepOSFamily removes the os.family attribute requirements from one step
// node and returns how many it removed. An attribute list left empty is
// dropped, and so is a hostRequirements left with nothing in it, because
// OpenJD rejects both empty.
func stripStepOSFamily(step *yaml.Node) int {
	hr := mappingValue(step, "hostRequirements")
	attrs := mappingValue(hr, "attributes")
	if attrs == nil {
		return 0
	}
	kept := attrs.Content[:0]
	removed := 0
	for _, attr := range attrs.Content {
		if name := mappingValue(attr, "name"); name != nil && name.Value == "attr.worker.os.family" {
			removed++
			continue
		}
		kept = append(kept, attr)
	}
	attrs.Content = kept
	if len(kept) == 0 {
		mappingDelete(hr, "attributes")
	}
	if len(hr.Content) == 0 {
		mappingDelete(step, "hostRequirements")
	}
	return removed
}

// mappingDelete removes key and its value from a mapping node.
func mappingDelete(node *yaml.Node, key string) {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			node.Content = append(node.Content[:i], node.Content[i+2:]...)
			return
		}
	}
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
