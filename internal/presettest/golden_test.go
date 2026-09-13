// SPDX-License-Identifier: AGPL-3.0-or-later

package presettest_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/presettest"
)

func TestCompareGolden_DetectsAMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "demo.golden")
	if err := presettest.WriteGolden(path, "command: renderer\n  [0]  -f\n  [1]  1\n"); err != nil {
		t.Fatalf("WriteGolden: %v", err)
	}
	// One argument changed -- exactly the shape of a preset regression.
	err := presettest.CompareGolden(path, "command: renderer\n  [0]  -f\n  [1]  2\n")
	if err == nil {
		t.Fatal("CompareGolden: want an error for changed argv, got nil")
	}
	if !strings.Contains(err.Error(), "[1]") {
		t.Errorf("error should show the differing content: %v", err)
	}
}

func TestCompareGolden_MissingFileIsAFailureNotAPass(t *testing.T) {
	err := presettest.CompareGolden(filepath.Join(t.TempDir(), "absent.golden"), "anything")
	if err == nil {
		t.Fatal("CompareGolden: a missing golden must fail, never pass silently")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error should wrap os.ErrNotExist so a caller can tell it apart: %v", err)
	}
}

func TestCompareGolden_MatchingContentPasses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "demo.golden")
	const content = "preset: demo\ncase:   default\n"
	if err := presettest.WriteGolden(path, content); err != nil {
		t.Fatalf("WriteGolden: %v", err)
	}
	if err := presettest.CompareGolden(path, content); err != nil {
		t.Errorf("CompareGolden: %v", err)
	}
}
