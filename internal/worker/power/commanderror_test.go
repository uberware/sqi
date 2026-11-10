// SPDX-License-Identifier: AGPL-3.0-or-later

package power

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCommandError(t *testing.T) {
	base := errors.New("exit status 1")
	long := strings.Repeat("x", maxCommandOutput+10)
	// An invalid byte at the start must not make the trim loop eat the whole
	// message.
	invalidFirst := "\xff" + strings.Repeat("y", maxCommandOutput+10)
	// A multi-byte rune straddling the cap is dropped whole.
	straddling := strings.Repeat("z", maxCommandOutput-1) + "é" + "tail"

	tests := []struct {
		name string
		out  string
		want string
	}{
		{"no output", "  \n", "exit status 1"},
		{"short output", " permission denied\n", "exit status 1: permission denied"},
		{"long output", long, "exit status 1: " + long[:maxCommandOutput] + "..."},
		{"invalid byte first", invalidFirst, "exit status 1: " + invalidFirst[:maxCommandOutput] + "..."},
		{"rune at the cap", straddling, "exit status 1: " + straddling[:maxCommandOutput-1] + "..."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := commandError(base, []byte(tc.out))
			if !errors.Is(err, base) {
				t.Fatalf("commandError(...) = %v, want it to wrap %v", err, base)
			}
			if got := err.Error(); got != tc.want {
				t.Errorf("commandError(...) = %q (%d bytes), want %q (%d bytes)", got, len(got), tc.want, len(tc.want))
			}
		})
	}
	if got := commandError(base, []byte(straddling)).Error(); !utf8.ValidString(got) {
		t.Errorf("a cut through a rune left invalid UTF-8: %q", got)
	}
}
