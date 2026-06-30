// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"errors"
	"fmt"
)

// PathDeliveryKind identifies one path-delivery mechanism of the
// SQI_PATH_TRANSLATION extension.
type PathDeliveryKind string

const (
	// DeliverySwapInPlace substitutes concrete paths into the command/args.
	DeliverySwapInPlace PathDeliveryKind = "swap_in_place"
	// DeliveryTranslationFile writes the OpenJD pathmapping-1.0 file.
	DeliveryTranslationFile PathDeliveryKind = "translation_file"
	// DeliveryCommandFlags appends per-pair flags rendered from Pattern.
	DeliveryCommandFlags PathDeliveryKind = "command_flags"
	// DeliveryEnvironment sets Variable to the joined src=dest pairs.
	DeliveryEnvironment PathDeliveryKind = "environment"
	// DeliveryStageLocally copies inputs to worker-local scratch.
	DeliveryStageLocally PathDeliveryKind = "stage_locally"
)

// PathDelivery is one enabled delivery plus its optional settings.
type PathDelivery struct {
	Kind PathDeliveryKind
	// Pattern is the flag template for DeliveryCommandFlags (uses {src}/{dest}).
	Pattern string
	// Variable is the environment variable name for DeliveryEnvironment.
	Variable string
}

// PathTranslation is the parsed SQI_PATH_TRANSLATION block.
type PathTranslation struct {
	Deliveries []PathDelivery
}

// DefaultPathDeliveries is the implicit delivery set used when the extension is
// not declared: today's automatic behavior (swap in place + translation file).
func DefaultPathDeliveries() []PathDelivery {
	return []PathDelivery{
		{Kind: DeliverySwapInPlace},
		{Kind: DeliveryTranslationFile},
	}
}

// maybeDecodePathTranslation returns nil, nil when the SQI_PATH_TRANSLATION key
// is absent or nil in raw, delegating to [decodePathTranslation] otherwise.
// This keeps the caller's cyclomatic complexity low.
func maybeDecodePathTranslation(raw map[string]any) (*PathTranslation, error) {
	v, ok := raw["SQI_PATH_TRANSLATION"]
	if !ok || v == nil {
		return nil, nil
	}
	return decodePathTranslation(v)
}

// decodePathTranslation decodes the top-level SQI_PATH_TRANSLATION map.
// Each deliveries item is either a bare string (no settings) or a single-key
// map whose key is the delivery name and whose value holds the settings.
func decodePathTranslation(v any) (*PathTranslation, error) {
	m, err := toMap(v, "SQI_PATH_TRANSLATION")
	if err != nil {
		return nil, err
	}
	pt := &PathTranslation{}
	raw, ok := m["deliveries"]
	if !ok || raw == nil {
		return pt, nil // empty; validation rejects an empty set when extension declared
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, errors.New("SQI_PATH_TRANSLATION.deliveries must be a list")
	}
	for i, item := range items {
		d, err := decodeDelivery(item, i)
		if err != nil {
			return nil, err
		}
		pt.Deliveries = append(pt.Deliveries, d)
	}
	return pt, nil
}

func decodeDelivery(item any, i int) (PathDelivery, error) {
	switch t := item.(type) {
	case string:
		return PathDelivery{Kind: PathDeliveryKind(t)}, nil
	case map[string]any:
		if len(t) != 1 {
			return PathDelivery{}, fmt.Errorf("SQI_PATH_TRANSLATION.deliveries[%d] must have exactly one key", i)
		}
		for k, settings := range t {
			d := PathDelivery{Kind: PathDeliveryKind(k)}
			if sm, ok := settings.(map[string]any); ok {
				d.Pattern = getString(sm, "pattern")
				d.Variable = getString(sm, "variable")
			}
			return d, nil
		}
	}
	return PathDelivery{}, fmt.Errorf("SQI_PATH_TRANSLATION.deliveries[%d] must be a string or a single-key map", i)
}
