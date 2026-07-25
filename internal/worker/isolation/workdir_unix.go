// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package isolation

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// SecureWorkDir restricts dir to cred's identity: chown to cred's uid/gid,
// then chmod 0700. Called once at session creation so a session's scratch
// directory is unreadable to any user but the one its tasks run as. A nil
// cred is a no-op, matching Apply's no-isolation behavior.
func SecureWorkDir(dir string, cred *Credential) error {
	if cred == nil {
		return nil
	}
	if err := os.Chown(dir, int(cred.cred.Uid), int(cred.cred.Gid)); err != nil {
		return fmt.Errorf("chown %q: %w", dir, err)
	}
	return os.Chmod(dir, 0o700) //nolint:gosec // a directory needs its owner execute bit to be traversable at all; 0700 is owner-only, not the world-writable/readable case G302 guards against
}

// ChownRecursive walks root and chowns every entry to cred's uid/gid. Apply
// only affects the process launched via exec.Cmd; files the daemon writes
// directly to disk (e.g. staged inputs, pre-created staged-output directories)
// are untouched by it and remain daemon-owned unless chowned explicitly. A nil
// cred is a no-op, matching Apply's no-isolation behavior.
//
// Uses os.Lchown, NOT os.Chown, on every entry. root is daemon-created, but
// its CONTENTS are not: they come from job-supplied staging paths, and the
// operator's canonical sync command (rsync -a) preserves symlinks rather than
// following them. os.Chown dereferences a symlink and would chown whatever it
// points at — including a target OUTSIDE root entirely — to the run-as-user
// identity, handing that identity write access to an arbitrary daemon-chosen
// (or worse, attacker-chosen-via-symlink) path. os.Lchown chowns the symlink
// entry itself and never follows it, closing that escalation.
func ChownRecursive(root string, cred *Credential) error {
	if cred == nil {
		return nil
	}
	uid, gid := int(cred.cred.Uid), int(cred.cred.Gid)
	return filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// gosec's G122 flags any filesystem call inside a WalkDir callback as a
		// generic symlink-TOCTOU risk (a race between the directory walk's
		// stat and this call, if path were replaced by a symlink in between).
		// That risk is specifically about an operation that FOLLOWS the final
		// symlink component — os.Lchown deliberately does not: even if path
		// were swapped for a symlink between WalkDir's stat and this call,
		// Lchown still only ever changes the symlink entry's own ownership,
		// never a target it might point at. Using Lchown instead of Chown is
		// the fix for exactly this class of risk (see the doc comment above),
		// not an instance of it.
		if chErr := os.Lchown(path, uid, gid); chErr != nil { //nolint:gosec // Lchown does not follow symlinks; see comment above
			return fmt.Errorf("chown %q: %w", path, chErr)
		}
		return nil
	})
}

// ── Boot-time / per-assignment ancestor validation (never mutates) ─────────

// ValidateTraversable checks path and every existing ancestor of it for the
// execute ("search") bit for others, returning an actionable error naming the
// first offending directory instead of silently widening it.
//
// It NEVER chmod's anything — an earlier revision of this package's
// EnsureTraversable helper widened an existing directory's mode to make it
// traversable, which is exactly the anti-pattern this validation replaces:
// creating a directory 0711 from birth (see session.Manager's session root,
// staging's scratch base) is a different, safe operation, since it can only
// ever affect a directory the creating call itself is making for the first
// time. An operator-chosen path may sit under a directory that is
// deliberately restricted (/root, above all others) and must never be
// widened by sqi to make isolation "just work" — refusing to proceed with a
// clear, actionable error is the correct behavior instead.
//
// A missing ancestor is not an error: os.MkdirAll creates it (at the correct
// mode) the first time the path is actually used, so there is nothing to
// validate yet.
//
// Called from two places, never unconditionally: cmd/sqi-worker's boot-time
// validateIsolationAncestors (only when isolation.required is set — see its
// own doc for why Provider.Capable() alone is the wrong gate: root and
// will-actually-isolate are not the same predicate), and per-assignment from
// session.Manager.Create / staging.Stager.StageIn the moment an assignment
// actually carries run-as-user isolation, so that case fails only the one
// task rather than the whole worker.
func ValidateTraversable(paths ...string) error {
	checked := make(map[string]bool)
	for _, p := range paths {
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return fmt.Errorf("resolve %q: %w", p, err)
		}
		if err := validateAncestorChain(abs, checked); err != nil {
			return err
		}
	}
	return nil
}

// validateAncestorChain walks dir and every ancestor of it up to the
// filesystem root, requiring the "other" execute bit on every EXISTING
// directory. checked memoizes directories already validated (via an earlier
// path in the same ValidateTraversable call) so a shared ancestor is not
// re-stat'd once per caller-supplied path.
func validateAncestorChain(dir string, checked map[string]bool) error {
	for {
		if checked[dir] {
			return nil
		}
		info, err := os.Stat(dir)
		switch {
		case errors.Is(err, os.ErrNotExist):
			// Not created yet; MkdirAll will make it (and everything below
			// it) at the correct mode when the path is first used for real.
		case err != nil:
			return fmt.Errorf("stat %q: %w", dir, err)
		default:
			if mode := info.Mode().Perm(); mode&0o001 == 0 {
				return fmt.Errorf(
					"isolation: directory %q has mode %04o, missing the execute (search) bit for "+
						"others; a run-as-user task's process credentials are switched BEFORE it can "+
						"chdir (Go's forkAndExecInChild), so every ancestor of an isolated session's or "+
						"staged attempt's working directory must be traversable by any uid — sqi will "+
						"not chmod this for you (an operator-chosen path may be deliberately restricted); "+
						"fix with: chmod o+x %s",
					dir, mode, dir,
				)
			}
			checked[dir] = true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil // reached the filesystem root
		}
		dir = parent
	}
}

// ── Symlink/hardlink-safe file writes ───────────────────────────────────────

// WriteFileFchown (re)writes path with data, refusing to write THROUGH any
// filesystem entry a job could have swapped in at that deterministic path,
// and — when cred is non-nil — chowns the result to cred's identity via
// fchown on the still-open descriptor.
//
// Step embedded files are written by the root daemon AFTER job code has
// already executed as the target uid (the environment onEnter action runs
// before any task's embedded files are written), at a deterministic path
// (e.g. {{Task.File.run}}). Between a plain os.WriteFile and a later
// path-based chown, the target uid could replace that path with:
//   - a SYMLINK: an open with no O_NOFOLLOW would follow it and write
//     through to the symlink's target. O_NOFOLLOW refuses the open outright.
//   - a HARDLINK to a root-owned file elsewhere on the same filesystem: a
//     plain os.WriteFile's O_TRUNC follows the existing directory entry and
//     truncates+overwrites THAT inode, and a subsequent path-based chown
//     would then hand the target uid ownership of it — escalation.
//
// This mirrors internal/worker/pathmap's writeSecure exactly (see its doc):
// any existing entry at path is removed first (best-effort; fs.ErrNotExist is
// not an error — there may be nothing there yet), then the file is created
// fresh with O_EXCL|O_NOFOLLOW. Remove unlinks the directory ENTRY without
// ever following it, so an attacker-planted symlink's target — or a
// hardlink's other name for the same inode — is never touched, only the
// entry at path is; O_EXCL then guarantees the create only succeeds when
// nothing (attacker-planted or otherwise) currently occupies that name. A
// naive O_EXCL-only create (this function's own previous shape) refused ANY
// pre-existing entry outright, including a second EMBEDDED FILE legitimately
// sharing the same name — two environments, or an environment and the step,
// each declaring one named "run" — which OpenJD's own last-wins semantics
// (see fmtres.AddFileVars's doc) requires this call to allow. Remove-then-
// create restores that: a legitimate second write succeeds (last-wins), while
// an attacker's swap is simply unlinked, never written through. If a job
// wins the race and recreates something in the tiny window between Remove
// and this Open, the O_EXCL create fails closed (EEXIST), which only ever
// fails that one task — a job can DoS itself this way, nothing more.
//
// fchown (f.Chown) acts on the inode via the already-open descriptor, so it
// is immune to the directory entry being replaced after this call returns:
// there is nothing left at the path to race.
func WriteFileFchown(path string, data []byte, perm os.FileMode, cred *Credential) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove existing %q: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, perm)
	if err != nil {
		return fmt.Errorf("create %q: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write %q: %w", path, err)
	}
	if cred == nil {
		return nil
	}
	if err := f.Chown(int(cred.cred.Uid), int(cred.cred.Gid)); err != nil {
		return fmt.Errorf("chown %q: %w", path, err)
	}
	return nil
}
