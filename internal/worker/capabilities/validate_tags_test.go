// SPDX-License-Identifier: AGPL-3.0-or-later

package capabilities

import (
	"strings"
	"testing"
)

// Capability tag keys are part of a case-insensitive capability name (OpenJD
// jobtemplate-2023-09), so two keys differing only in case are the same
// capability and must be rejected at registration rather than matched ambiguously.
func TestValidateTagKeys(t *testing.T) {
	t.Run("distinct keys ok", func(t *testing.T) {
		if err := ValidateTagKeys(map[string]string{"gpu": "", "maya": "2025"}); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("case-only collision rejected", func(t *testing.T) {
		err := ValidateTagKeys(map[string]string{"GPU": "", "gpu": ""})
		if err == nil {
			t.Fatal("expected error for case-colliding tag keys, got nil")
		}
		if !strings.Contains(err.Error(), "case") {
			t.Errorf("error should mention case-insensitivity, got %v", err)
		}
	})

	t.Run("empty ok", func(t *testing.T) {
		if err := ValidateTagKeys(nil); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
}
