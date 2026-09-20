// SPDX-License-Identifier: AGPL-3.0-or-later

package fsutil_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/uberware/sqi/internal/fsutil"
)

// TestWriteSecret_IsRestricted is the portable statement of the property that
// matters: whatever sqi writes as key material must not be readable by an
// ordinary local account.
//
// It asserts through fsutil.IsRestricted rather than Mode().Perm() on purpose.
// `perm == 0o600` is true on POSIX and UNSATISFIABLE on Windows — os.Chmod
// there maps only to the read-only attribute and cannot deny read access to
// anyone — and that mismatch is exactly how four packages shipped key material
// with no confidentiality on Windows while their tests passed on Linux CI.
func TestWriteSecret_IsRestricted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca.key")
	if err := fsutil.WriteSecret(path, []byte("PRIVATE KEY")); err != nil {
		t.Fatalf("WriteSecret: %v", err)
	}

	restricted, err := fsutil.IsRestricted(path)
	if err != nil {
		t.Fatalf("IsRestricted: %v", err)
	}
	if !restricted {
		t.Error("WriteSecret produced a file readable beyond its owner")
	}

	if got, err := os.ReadFile(path); err != nil || string(got) != "PRIVATE KEY" {
		t.Fatalf("content = %q err=%v; restricting must not corrupt the write", got, err)
	}

	// On POSIX the mode is still the concrete guarantee, so keep asserting it
	// directly — IsRestricted accepts anything with no group/other bits, which
	// is deliberately weaker than what WriteSecret actually produces.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("mode = %o, want 600", perm)
		}
	}
}

// TestIsRestricted_RejectsAWorldReadableFile is the negative control. Without
// it TestWriteSecret_IsRestricted could pass against an IsRestricted that
// returns true unconditionally — which on Windows is a real risk, since the
// DACL inspection is the only thing standing behind the claim.
func TestIsRestricted_RejectsAWorldReadableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "public.txt")
	if err := os.WriteFile(path, []byte("not a secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	restricted, err := fsutil.IsRestricted(path)
	if err != nil {
		t.Fatalf("IsRestricted: %v", err)
	}
	if restricted {
		t.Error("IsRestricted accepted a file created with ordinary inherited access; " +
			"it is not actually inspecting anything")
	}
}

// TestWriteSecret_ReplacesAnExistingFile pins the overwrite path: `tls issue`
// re-issues a leaf certificate over a previous one, and a create that refused
// an existing name would break that legitimate flow.
func TestWriteSecret_ReplacesAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.key")
	if err := os.WriteFile(path, []byte("OLD KEY MATERIAL, LONGER"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := fsutil.WriteSecret(path, []byte("NEW KEY")); err != nil {
		t.Fatalf("WriteSecret over an existing file: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "NEW KEY" {
		t.Errorf("content = %q, want %q — the old, longer content was not fully replaced", got, "NEW KEY")
	}
	restricted, err := fsutil.IsRestricted(path)
	if err != nil {
		t.Fatalf("IsRestricted: %v", err)
	}
	if !restricted {
		t.Error("replacing a permissive file left it permissive")
	}
}

// TestMkdirSecret_IsRestricted covers the directory half. internal/brokerauth
// creates its seed directory before writing into it, and on Windows the mode
// argument to os.MkdirAll is discarded outright.
func TestMkdirSecret_IsRestricted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "creds")
	if err := fsutil.MkdirSecret(dir); err != nil {
		t.Fatalf("MkdirSecret: %v", err)
	}

	restricted, err := fsutil.IsRestricted(dir)
	if err != nil {
		t.Fatalf("IsRestricted: %v", err)
	}
	if !restricted {
		t.Error("MkdirSecret produced a directory readable beyond its owner")
	}

	// The intermediate parent is deliberately NOT restricted — a secret's
	// confidentiality comes from its own ACL, and tightening a shared ancestor
	// is the widening/narrowing anti-pattern this codebase avoids.
	if _, err := os.Stat(filepath.Dir(dir)); err != nil {
		t.Errorf("intermediate parent was not created: %v", err)
	}
}

// TestMkdirSecret_ExistingDirIsLeftAlone proves an operator-provisioned
// directory is not silently re-permissioned. os.MkdirAll never revisits an
// existing directory's mode; neither may this.
func TestMkdirSecret_ExistingDirIsLeftAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "preexisting")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fsutil.MkdirSecret(dir); err != nil {
		t.Fatalf("MkdirSecret over an existing dir: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o755 {
			t.Errorf("mode = %o, want 755 — an existing directory must not be re-permissioned", perm)
		}
	}
}
