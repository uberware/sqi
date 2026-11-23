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
// It uses os.Lchown, not os.Chown, on every entry. root is daemon-created,
// but its contents come from job-supplied staging paths, and the operator's
// usual sync command (rsync -a) preserves symlinks rather than following
// them. os.Chown dereferences a symlink and would chown whatever it points
// at, including a target outside root, to the run-as-user identity, giving
// that identity write access to a path a job chose. os.Lchown chowns the
// symlink entry itself and never follows it.
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
		// That risk applies to an operation that follows the final symlink
		// component, which os.Lchown does not: even if path were swapped for
		// a symlink between WalkDir's stat and this call, Lchown changes only
		// the symlink entry's own ownership, never its target (see the doc
		// comment above).
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
// It never chmods anything. Creating a directory 0711 (session.Manager's
// session root, staging's scratch base) affects only a directory the call is
// creating; widening an existing directory is different. An operator-chosen
// path may sit under a directory restricted on purpose (/root above all),
// which sqi must not widen to make isolation work, so it refuses with an
// error instead.
//
// A missing ancestor is not an error: os.MkdirAll creates it (at the correct
// mode) the first time the path is used, so there is nothing to validate yet.
//
// Called from two places: cmd/sqi-worker's boot-time
// validateIsolationAncestors (only when isolation.required is set; see its
// doc for why Provider.Capable() is the wrong gate), and per assignment from
// session.Manager.Create / staging.Stager.StageIn when an assignment carries
// run-as-user isolation, so that case fails only the one task rather than
// the whole worker.
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
// filesystem root, requiring the "other" execute bit on every existing
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
			// it) at the correct mode when the path is first used.
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

// WriteFileFchown (re)writes path with data, refusing to write through any
// filesystem entry a job could have swapped in at that deterministic path,
// and, when cred is non-nil, chowns the result to cred's identity via fchown
// on the still-open descriptor.
//
// Step embedded files are written by the root daemon after job code has
// already executed as the target uid (the environment onEnter action runs
// before any task's embedded files are written), at a deterministic path
// (e.g. {{Task.File.run}}). Between a plain os.WriteFile and a later
// path-based chown, the target uid could replace that path with:
//   - a symlink: an open with no O_NOFOLLOW would follow it and write
//     through to the symlink's target. O_NOFOLLOW refuses the open.
//   - a hardlink to a root-owned file elsewhere on the same filesystem: a
//     plain os.WriteFile's O_TRUNC follows the existing directory entry and
//     truncates and overwrites that inode, and a later path-based chown
//     would hand the target uid ownership of it.
//
// This mirrors internal/worker/pathmap's writeSecure (see its doc): any
// existing entry at path is removed first (fs.ErrNotExist is not an error;
// there may be nothing there yet), then the file is created fresh with
// O_EXCL|O_NOFOLLOW. Remove unlinks the directory entry without following
// it, so a planted symlink's target, or a hardlink's other name for the same
// inode, is never touched; O_EXCL then makes the create succeed only when
// nothing occupies that name. Removing first lets a second embedded file
// with the same name (two environments, or an environment and the step, each
// declaring one named "run") overwrite the first, as OpenJD's last-wins
// semantics require (see fmtres.AddFileVars's doc), while a planted entry is
// unlinked, never written through. If a job recreates something in the
// window between Remove and this Open, the O_EXCL create fails with EEXIST,
// which fails only that job's own task.
//
// fchown (f.Chown) acts on the inode through the open descriptor, so
// replacing the directory entry afterward has no effect on it.
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
