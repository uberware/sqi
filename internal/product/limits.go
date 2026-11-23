// SPDX-License-Identifier: AGPL-3.0-or-later

package product

import (
	"fmt"
	"unicode/utf8"

	"github.com/uberware/sqi/internal/store"
)

// Metadata length limits, counted in runes rather than bytes so that text in a
// multi-byte script gets the same room as ASCII (500 bytes of CJK is about 166
// characters).
//
// MaxDescriptionLen is a design limit: the description is rendered into an
// unclamped picker card and a native Blender EnumProperty tooltip.
// MaxReadmeLen is an abuse guard; the readme appears only on the detail page.
// The other limits leave ample room for any real product and keep every
// metadata field bounded.
const (
	MaxNameLen        = 128
	MaxTitleLen       = 200
	MaxDescriptionLen = 500
	MaxReadmeLen      = 8000
	MaxCategoryLen    = 64
	MaxVersionLen     = 32
)

// checkLen returns an error naming the field, the actual rune count and the cap
// when value is longer than maxRunes.
func checkLen(field, value string, maxRunes int) error {
	if n := utf8.RuneCountInString(value); n > maxRunes {
		return fmt.Errorf("product: %s is %d characters, limit is %d", field, n, maxRunes)
	}
	return nil
}

// ValidateMetadata enforces the length limits on a product's metadata fields.
//
// Both entry points into product data call it, ParseDefinition and the REST
// create/update handler, so the two enforce the same limits.
//
// It checks length only. The slug pattern is checked by validateName on the
// definition path alone, because applying it to the REST route would reject
// products already stored under a name that does not match it.
func ValidateMetadata(p store.Product) error {
	checks := []struct {
		field string
		value string
		max   int
	}{
		{"name", p.Name, MaxNameLen},
		{"title", p.Title, MaxTitleLen},
		{"description", p.Description, MaxDescriptionLen},
		{"readme", p.Readme, MaxReadmeLen},
		{"category", p.Category, MaxCategoryLen},
		{"version", p.Version, MaxVersionLen},
	}
	for _, c := range checks {
		if err := checkLen(c.field, c.value, c.max); err != nil {
			return err
		}
	}
	return nil
}
