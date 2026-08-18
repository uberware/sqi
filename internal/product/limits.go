// SPDX-License-Identifier: AGPL-3.0-or-later

package product

import (
	"fmt"
	"unicode/utf8"

	"github.com/uberware/sqi/internal/store"
)

// Metadata length limits, in RUNES rather than bytes: 500 bytes of CJK is ~166
// characters, and a Japanese-language description must not be silently
// third-class.
//
// The two interesting caps differ in kind. MaxDescriptionLen is a design
// constraint -- description is rendered into an unclamped picker card and a
// native Blender EnumProperty tooltip, and 500 is 1.5x the longest description
// that survived commit a1e529e's hand-trim (329). It would have REJECTED the
// 940-, 629- and 617-rune descriptions that forced that commit, which is the
// test a cap should pass; a 1000 cap would have permitted all three.
// MaxReadmeLen is only an abuse guard -- readme is detail-page-only, so nothing
// downstream breaks; it simply should not be a novel.
//
// The rest have far more headroom than the shipped presets need (their maxima
// are name 35, title 37, category 11). They exist because capping description
// while leaving its neighbors unbounded would be incoherent once the helper
// exists.
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
// It is exported and deliberately called from BOTH doors into product data --
// ParseDefinition and the REST create/update handler. Those are separate entry
// points to the same data, and the comment on ValidateOptions records this exact
// trap biting once already: the preset routes silently kept validating on
// DefaultExprLimits() after the create/update route was fixed.
//
// It checks LENGTH only. The slug pattern stays in validateName on the
// definition path, because applying it to the REST route would tighten
// acceptance and strand products already stored under a pattern-invalid name.
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
