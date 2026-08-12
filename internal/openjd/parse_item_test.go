// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import "testing"

// TestDecodeJobParamConstraints_Item covers RFC 0007's nested item: structure,
// which mirrors the type nesting so each level reuses the same property names
// as the corresponding scalar type.
func TestDecodeJobParamConstraints_Item(t *testing.T) {
	p, err := decodeJobParameter(map[string]any{
		"name":      "Frames",
		"type":      "list[list[int]]",
		"minLength": 1,
		"maxLength": 4,
		"item": map[string]any{
			"minLength": 2,
			"maxLength": 3,
			"item": map[string]any{
				"minValue":      0,
				"maxValue":      100,
				"allowedValues": []any{1, 2, 3},
			},
		},
	}, true)
	if err != nil {
		t.Fatalf("decodeJobParameter: %v", err)
	}

	if p.MinLength == nil || *p.MinLength != 1 {
		t.Fatalf("outer MinLength = %v, want 1", p.MinLength)
	}
	if p.Item == nil {
		t.Fatal("item: was not decoded")
	}
	if p.Item.MinLength == nil || *p.Item.MinLength != 2 {
		t.Errorf("item.minLength = %v, want 2", p.Item.MinLength)
	}
	if p.Item.MaxLength == nil || *p.Item.MaxLength != 3 {
		t.Errorf("item.maxLength = %v, want 3", p.Item.MaxLength)
	}
	if p.Item.Item == nil {
		t.Fatal("item.item: was not decoded")
	}
	if p.Item.Item.MinValue == nil || *p.Item.Item.MinValue != "0" {
		t.Errorf("item.item.minValue = %v, want \"0\"", p.Item.Item.MinValue)
	}
	if p.Item.Item.MaxValue == nil || *p.Item.Item.MaxValue != "100" {
		t.Errorf("item.item.maxValue = %v, want \"100\"", p.Item.Item.MaxValue)
	}
	if got := p.Item.Item.AllowedValues; len(got) != 3 || got[0] != "1" || got[2] != "3" {
		t.Errorf("item.item.allowedValues = %v, want [1 2 3]", got)
	}
	if !p.Item.Item.AllowedValuesSet {
		t.Error("item.item.allowedValues presence was not tracked")
	}
}

// TestDecodeJobParamConstraints_ItemAbsent pins that a parameter with no item:
// key decodes to a nil Item — distinguishable from an empty one, the same way
// AllowedValuesSet distinguishes an absent list from a declared empty one.
func TestDecodeJobParamConstraints_ItemAbsent(t *testing.T) {
	p, err := decodeJobParameter(map[string]any{
		"name": "Names", "type": "list[string]",
	}, true)
	if err != nil {
		t.Fatalf("decodeJobParameter: %v", err)
	}
	if p.Item != nil {
		t.Errorf("Item = %+v, want nil when item: is absent", p.Item)
	}
}

// TestDecodeItemConstraint_Rejects covers the shapes the decoder must refuse.
// The depth cap is the load-bearing one: RFC 0007 allows list[list[T]] and no
// deeper, so a third item: level describes a list that cannot exist. Accepting
// it silently would leave a constraint nothing ever applies.
func TestDecodeItemConstraint_Rejects(t *testing.T) {
	tests := []struct {
		name string
		raw  map[string]any
	}{
		{
			name: "item is not a mapping",
			raw: map[string]any{
				"name": "L", "type": "list[int]",
				"item": []any{1, 2},
			},
		},
		{
			name: "item nests deeper than list[list[T]]",
			raw: map[string]any{
				"name": "L", "type": "list[list[int]]",
				"item": map[string]any{
					"item": map[string]any{
						"item": map[string]any{"minValue": 1},
					},
				},
			},
		},
		{
			name: "item.minLength is not an integer",
			raw: map[string]any{
				"name": "L", "type": "list[string]",
				"item": map[string]any{"minLength": "two"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := decodeJobParameter(tt.raw, true); err == nil {
				t.Fatal("decodeJobParameter accepted a shape it must reject")
			}
		})
	}
}

// TestDecodeItemConstraint_IgnoredWithoutEXPR pins that item: is decoded
// regardless of the extension gate, and that this is harmless. The gate lives
// on the TYPE (a base-spec template cannot declare a list type at all, so its
// item: block can never be reached by validation), which keeps the decoder
// simple: one fewer conditional, and no way for a gated-off branch to drift.
func TestDecodeItemConstraint_IgnoredWithoutEXPR(t *testing.T) {
	p, err := decodeJobParameter(map[string]any{
		"name": "L", "type": "LIST[INT]",
		"item": map[string]any{"minValue": 1},
	}, false)
	if err != nil {
		t.Fatalf("decodeJobParameter: %v", err)
	}
	if p.Type != JobParamType("LIST[INT]") {
		t.Errorf("Type = %q, want the verbatim spelling: the type gate, not the "+
			"item: decoder, is what rejects a list parameter without EXPR", p.Type)
	}
}
