// SPDX-License-Identifier: AGPL-3.0-or-later

package presettest

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteGolden writes content to path, creating parent directories.
func WriteGolden(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("presettest: create golden dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("presettest: write golden: %w", err)
	}
	return nil
}

// CompareGolden returns nil when the file at path equals got, and an error
// describing the difference otherwise.
//
// A missing golden is an error wrapping os.ErrNotExist. Treating an absent file
// as "nothing to compare" would turn a renamed preset, a mistyped case name or
// a lost testdata directory into a passing run.
func CompareGolden(path, got string) error {
	want, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("presettest: read golden %s (run with -preset-update to create it, "+
			"then review it): %w", path, err)
	}
	if got != string(want) {
		return fmt.Errorf("presettest: %s does not match:\n--- got ---\n%s\n--- want ---\n%s",
			path, got, want)
	}
	return nil
}
