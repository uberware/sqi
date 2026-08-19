// SPDX-License-Identifier: AGPL-3.0-or-later

package product

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/store"
)

func TestValidateMetadata_Boundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		build func(s string) store.Product
		max   int
		field string
	}{
		{"title", func(s string) store.Product { return store.Product{Title: s} }, MaxTitleLen, "title"},
		{"description", func(s string) store.Product { return store.Product{Description: s} }, MaxDescriptionLen, "description"},
		{"readme", func(s string) store.Product { return store.Product{Readme: s} }, MaxReadmeLen, "readme"},
		{"category", func(s string) store.Product { return store.Product{Category: s} }, MaxCategoryLen, "category"},
		{"version", func(s string) store.Product { return store.Product{Version: s} }, MaxVersionLen, "version"},
		{"name", func(s string) store.Product { return store.Product{Name: s} }, MaxNameLen, "name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := ValidateMetadata(tt.build(strings.Repeat("a", tt.max))); err != nil {
				t.Errorf("at the cap: unexpected error %v", err)
			}
			err := ValidateMetadata(tt.build(strings.Repeat("a", tt.max+1)))
			if err == nil {
				t.Fatal("at cap+1: want an error, got nil")
			}
			if !strings.Contains(err.Error(), tt.field) {
				t.Errorf("error %q does not name the field %q", err, tt.field)
			}
		})
	}
}

// The caps count runes, not bytes. A CJK description of exactly
// MaxDescriptionLen characters is ~3x that many bytes and must still be
// accepted -- otherwise a Japanese-language description is silently
// third-class.
func TestValidateMetadata_CountsRunesNotBytes(t *testing.T) {
	t.Parallel()
	desc := strings.Repeat("日", MaxDescriptionLen)
	if len(desc) <= MaxDescriptionLen {
		t.Fatalf("test is not exercising multi-byte input: %d bytes for %d runes", len(desc), MaxDescriptionLen)
	}
	if err := ValidateMetadata(store.Product{Description: desc}); err != nil {
		t.Errorf("CJK description at the rune cap: unexpected error %v", err)
	}
	if err := ValidateMetadata(store.Product{Description: desc + "日"}); err == nil {
		t.Error("CJK description at cap+1: want an error, got nil")
	}
}

// The message carries the actual length as well as the cap, so an author can
// see how much to cut without counting by hand.
func TestValidateMetadata_ErrorNamesCapAndActual(t *testing.T) {
	t.Parallel()
	err := ValidateMetadata(store.Product{Description: strings.Repeat("a", MaxDescriptionLen+7)})
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"description", "507", "500"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q is missing %q", msg, want)
		}
	}
}

// The length cap applies to name, but the slug PATTERN stays on the definition
// path only -- see the spec's post-approval finding. A pattern-invalid name is
// therefore not ValidateMetadata's business.
func TestValidateMetadata_IgnoresSlugPattern(t *testing.T) {
	t.Parallel()
	if err := ValidateMetadata(store.Product{Name: "Not A Slug!!"}); err != nil {
		t.Errorf("unexpected error %v", err)
	}
}

func TestValidateName_RejectsOverlongSlug(t *testing.T) {
	t.Parallel()
	if err := validateName(strings.Repeat("a", MaxNameLen+1)); err == nil {
		t.Fatal("want an error for a pattern-valid but over-long slug, got nil")
	}
	if err := validateName(strings.Repeat("a", MaxNameLen)); err != nil {
		t.Errorf("at the cap: unexpected error %v", err)
	}
}
