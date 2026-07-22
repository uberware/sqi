// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package isolation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// ── ValidateTraversable ──────────────────────────────────────────────────────

// statPerm returns path's permission bits.
func statPerm(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	return info.Mode().Perm()
}

// makeAncestorsTraversableForTest climbs from dir up through every ancestor,
// best-effort chmod'ing +x for others, stopping at the first one this test
// process does not own (a Chmod error there is expected, not a failure: an
// ancestor outside this process's control is either already traversable —
// true of every real filesystem root above a per-user temp directory — or
// this test cannot fix it anyway, which is exactly the situation
// ValidateTraversable itself is designed for in production).
//
// This exists because t.TempDir() sits under MULTIPLE testing-owned and
// OS-owned layers that are 0700 by construction and have nothing to do with
// the behavior under test: the per-package scratch root Go's testing package
// creates via os.MkdirTemp (0700), and — on macOS specifically — the
// per-user private $TMPDIR itself (e.g. /var/folders/<x>/<y>/T, also 0700 by
// OS design). Neither is something session.Manager or staging.Stager would
// ever be asked to create in production (a real deployment's session root
// sits under /var/lib or an operator-chosen path — see
// cmd/sqi-worker's effectiveSessionRoot), so climbing and fixing them up here
// is a test-fixture-only accommodation, not something ValidateTraversable
// itself should ever do (see its own doc: it NEVER modifies anything).
func makeAncestorsTraversableForTest(t *testing.T, dir string) {
	t.Helper()
	// Capped at a handful of levels — enough to clear every testing/OS-owned
	// layer under a temp directory (see doc above) without ever climbing
	// arbitrarily far up the real filesystem, even if this test process
	// happens to run privileged enough that os.Chmod would otherwise keep
	// succeeding all the way to "/".
	const maxLevels = 6
	for range maxLevels {
		if err := os.Chmod(dir, 0o711); err != nil {
			return // reached a directory this test process does not own; stop climbing
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return // reached the filesystem root
		}
		dir = parent
	}
}

// skipIfEnvironmentBlocksTraversalFixture reports (via t.Skip) when err came
// from an ancestor OUTSIDE the self-contained chain this test built
// (self is the set of directories the test itself created and owns). Some
// sandboxed environments restrict chmod on OS-managed temp-directory
// ancestors regardless of Unix ownership, which no test-fixture workaround
// can reliably clear — that is an environment restriction, not evidence
// ValidateTraversable's own logic (exercised fully by
// TestValidateTraversable_FailsOnNarrowAncestor, which only ever needs a
// SELF-created narrow ancestor) is wrong.
func skipIfEnvironmentBlocksTraversalFixture(t *testing.T, err error, self map[string]bool) {
	t.Helper()
	if err == nil {
		return
	}
	for path := range self {
		if strings.Contains(err.Error(), path) {
			return // failure is about a directory THIS test created; a real failure
		}
	}
	t.Skipf("environment appears to block making a temp-dir ancestor traversable "+
		"(chmod likely restricted by a sandbox regardless of Unix ownership), "+
		"which this fixture cannot work around: %v", err)
}

// TestValidateTraversable_TraversableChainPasses is the "happy path": every
// ancestor already has the execute bit for others, so validation succeeds
// and — critically — nothing is chmod'd, which this test verifies directly
// by re-checking every mode afterward.
func TestValidateTraversable_TraversableChainPasses(t *testing.T) {
	root := t.TempDir()
	mid := filepath.Join(root, "mid")
	leaf := filepath.Join(mid, "leaf")
	if err := os.MkdirAll(leaf, 0o711); err != nil {
		t.Fatalf("set up traversable chain: %v", err)
	}
	// See makeAncestorsTraversableForTest's doc: t.TempDir() sits under
	// multiple testing/OS-owned 0700 layers that have nothing to do with the
	// behavior under test, so this is a genuinely end-to-end traversable
	// chain, not just a self-contained assertion about the "mid"/"leaf"
	// levels this test itself created.
	makeAncestorsTraversableForTest(t, root)

	before := map[string]os.FileMode{
		root: statPerm(t, root),
		mid:  statPerm(t, mid),
		leaf: statPerm(t, leaf),
	}

	err := ValidateTraversable(leaf)
	skipIfEnvironmentBlocksTraversalFixture(t, err, map[string]bool{root: true, mid: true, leaf: true})
	if err != nil {
		t.Errorf("ValidateTraversable(%q) = %v, want nil for an already-traversable chain", leaf, err)
	}

	for path, mode := range before {
		if got := statPerm(t, path); got != mode {
			t.Errorf("mode of %q changed from %o to %o — ValidateTraversable must NEVER modify anything", path, mode, got)
		}
	}
}

// TestValidateTraversable_FailsOnNarrowAncestor is the guard: an ancestor
// missing the execute bit for others must fail with an error naming that
// exact directory, and — just as importantly — must not have been chmod'd to
// "fix" it.
func TestValidateTraversable_FailsOnNarrowAncestor(t *testing.T) {
	root := t.TempDir()
	narrow := filepath.Join(root, "narrow")
	leaf := filepath.Join(narrow, "leaf")
	if err := os.Mkdir(narrow, 0o700); err != nil {
		t.Fatalf("create narrow ancestor: %v", err)
	}
	if err := os.Mkdir(leaf, 0o711); err != nil {
		t.Fatalf("create leaf: %v", err)
	}

	err := ValidateTraversable(leaf)

	if err == nil {
		t.Fatal("expected an error for a chain with a non-traversable ancestor; got nil")
	}
	if !strings.Contains(err.Error(), narrow) {
		t.Errorf("error = %v, want it to name the offending directory %q", err, narrow)
	}
	if got := statPerm(t, narrow); got != 0o700 {
		t.Errorf("narrow ancestor mode = %o after a failed validation, want unchanged 0700 — "+
			"ValidateTraversable must never chmod, only report", got)
	}
}

// TestValidateTraversable_MissingAncestorIsNotAnError checks that a path
// which does not exist yet at all (the common case: MkdirAll will create it,
// and everything below it, the first time it is actually used) is not
// treated as a validation failure.
func TestValidateTraversable_MissingAncestorIsNotAnError(t *testing.T) {
	root := t.TempDir()
	makeAncestorsTraversableForTest(t, root)
	neverCreated := filepath.Join(root, "does", "not", "exist", "yet")

	err := ValidateTraversable(neverCreated)
	skipIfEnvironmentBlocksTraversalFixture(t, err, map[string]bool{root: true})
	if err != nil {
		t.Errorf("ValidateTraversable(%q) = %v, want nil — missing ancestors are not a failure", neverCreated, err)
	}
}

// ── WriteFileFchown ──────────────────────────────────────────────────────────

// currentCredential returns a *Credential resolved to the CURRENT process's
// own uid/gid, via the real fake-credential constructor. Chowning a file to
// the uid/gid that already owns it is a permitted no-op even for a
// non-privileged test process, unlike an actual setuid/chown-to-ANOTHER-user,
// which requires root — see internal/worker/session's own isolation guard
// tests for the same constraint on isolation.Apply.
func currentCredential(t *testing.T) *Credential {
	t.Helper()
	p := NewFake(map[string]FakeAccount{
		"self": {UID: uint32(os.Getuid()), GID: uint32(os.Getgid())},
	})
	cred, err := p.Resolve(context.Background(), Spec{User: "self"})
	if err != nil {
		t.Fatalf("resolve self credential: %v", err)
	}
	return cred
}

func TestWriteFileFchown_WritesDataAndChownsToCredential(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run")
	cred := currentCredential(t)

	if err := WriteFileFchown(path, []byte("#!/bin/sh\necho hi\n"), 0o750, cred); err != nil {
		t.Fatalf("WriteFileFchown: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(data) != "#!/bin/sh\necho hi\n" {
		t.Errorf("file contents = %q, want the exact data passed in", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o750 {
		t.Errorf("mode = %o, want 0750", info.Mode().Perm())
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("cannot read raw uid/gid")
	}
	if int(st.Uid) != os.Getuid() || int(st.Gid) != os.Getgid() {
		t.Errorf("owner = %d:%d, want %d:%d (fchown to the credential)", st.Uid, st.Gid, os.Getuid(), os.Getgid())
	}
}

func TestWriteFileFchown_NilCredentialSkipsChown(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data")

	if err := WriteFileFchown(path, []byte("payload"), 0o640, nil); err != nil {
		t.Fatalf("WriteFileFchown: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "payload" {
		t.Errorf("contents = %q, want %q", data, "payload")
	}
}

// TestWriteFileFchown_OverwritesPreexistingFile_LastWins proves last-wins
// duplicate-embedded-file-name semantics are restored: a second write to the
// same deterministic path (two environments — or an environment and the
// step — each declaring an embedded file named "run") must overwrite the
// first, exactly like the plain os.WriteFile this call replaced, rather than
// hard-failing the task. See fmtres.AddFileVars's doc for the documented
// last-wins contract this guards.
func TestWriteFileFchown_OverwritesPreexistingFile_LastWins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run")
	if err := os.WriteFile(path, []byte("first-entry"), 0o600); err != nil {
		t.Fatalf("seed pre-existing file: %v", err)
	}

	if err := WriteFileFchown(path, []byte("last-entry-wins"), 0o750, currentCredential(t)); err != nil {
		t.Fatalf("WriteFileFchown: %v", err)
	}

	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read: %v", readErr)
	}
	if string(data) != "last-entry-wins" {
		t.Errorf("content = %q, want %q — a duplicate embedded-file name must overwrite (last-wins), not hard-fail the task", data, "last-entry-wins")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o750 {
		t.Errorf("mode = %o, want 0750 (the second write's own perm, not the first's)", info.Mode().Perm())
	}
}

// TestWriteFileFchown_SymlinkAtDestinationIsUnlinkedNotFollowed is the
// symlink half of the TOCTOU guard: a symlink planted at the deterministic
// path, pointing at a file OUTSIDE the session, must never be written
// through — Remove unlinks the symlink ENTRY itself (never following it),
// then O_EXCL|O_NOFOLLOW creates a fresh regular file at that name, so the
// write succeeds (consistent with last-wins) while the symlink's outside
// target is left completely untouched.
func TestWriteFileFchown_SymlinkAtDestinationIsUnlinkedNotFollowed(t *testing.T) {
	dir := t.TempDir()
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "outside-target")
	if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	link := filepath.Join(dir, "run")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	if err := WriteFileFchown(link, []byte("fresh-file-contents"), 0o750, currentCredential(t)); err != nil {
		t.Fatalf("WriteFileFchown: %v", err)
	}

	data, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("read target: %v", readErr)
	}
	if string(data) != "untouched" {
		t.Errorf("symlink target content = %q, want untouched — O_NOFOLLOW/Remove must never write through the symlink", data)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat %q: %v", link, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Errorf("%q is still a symlink after WriteFileFchown — the planted entry must be unlinked and replaced with a fresh regular file", link)
	}
	linkData, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("read %q: %v", link, err)
	}
	if string(linkData) != "fresh-file-contents" {
		t.Errorf("content at %q = %q, want the freshly written data", link, linkData)
	}
}
