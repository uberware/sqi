// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package isolation

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
func ChownRecursive(root string, cred *Credential) error {
	if cred == nil {
		return nil
	}
	uid, gid := int(cred.cred.Uid), int(cred.cred.Gid)
	return filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// root is a per-attempt scratch directory the daemon itself just
		// created (staging.Stager.StageIn's scratchDir); it is not
		// attacker-controlled input, so the walk-then-chown ordering here
		// carries none of the symlink-swap TOCTOU risk G122 flags in general.
		if chErr := os.Chown(path, uid, gid); chErr != nil { //nolint:gosec // see comment above
			return fmt.Errorf("chown %q: %w", path, chErr)
		}
		return nil
	})
}
