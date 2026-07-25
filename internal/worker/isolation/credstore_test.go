// SPDX-License-Identifier: AGPL-3.0-or-later

package isolation

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestCredFileName_NeverEscapesTheDirectory is the traversal guard.
// validateAccountArg (validate.go) rejects only empty, NUL/newline, and
// leading-hyphen names — it permits "\", "/", and "..", all legal in the
// DOMAIN\user forms normalizeAccountName already anticipates. Joining a
// username straight into a path would therefore let a queue configured with
// `..\..\..\Windows\Temp\x` read and write outside the worker data dir, so
// the on-disk name is a hex encoding rather than the name itself.
func TestCredFileName_NeverEscapesTheDirectory(t *testing.T) {
	dir := `C:\ProgramData\sqi\worker\isolation`
	tests := []struct {
		name string
		user string
	}{
		{"plain", "render-svc"},
		{"domain qualified", `CORP\render-svc`},
		{"dot backslash", `.\render-svc`},
		{"parent traversal", `..\..\..\Windows\Temp\x`},
		{"forward slash traversal", "../../etc/passwd"},
		{"absolute", `C:\Windows\System32\evil`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := filepath.Join(dir, credFileName(tc.user))

			if filepath.Dir(got) != dir {
				t.Errorf("credFileName(%q) resolved to %q, which is outside %q", tc.user, got, dir)
			}
		})
	}
}

// TestCredFileName_MatchesNormalizedIdentity proves the same account written
// in different qualified forms resolves to ONE file. Resolve() looks the
// secret up by whatever string the queue supplied, so if set-credential
// stored `CORP\render-svc` under a different name than a queue's plain
// `render-svc` looked up, provisioning would appear to succeed and every
// assignment would then fail with "empty secret".
func TestCredFileName_MatchesNormalizedIdentity(t *testing.T) {
	want := credFileName("render-svc")

	for _, variant := range []string{`CORP\render-svc`, `.\render-svc`, "RENDER-SVC", "  render-svc  "} {
		if got := credFileName(variant); got != want {
			t.Errorf("credFileName(%q) = %q, want %q", variant, got, want)
		}
	}
}

// TestCredFileName_HasCredExtension keeps the on-disk shape recognizable to
// an operator listing the directory, since the name itself is opaque hex.
func TestCredFileName_HasCredExtension(t *testing.T) {
	if got := credFileName("render-svc"); !strings.HasSuffix(got, ".cred") {
		t.Errorf("credFileName = %q, want a .cred suffix", got)
	}
}
