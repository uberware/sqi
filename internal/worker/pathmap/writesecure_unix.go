// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package pathmap

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// writeSecure (re)writes path with data, refusing to write through any
// filesystem entry a job could have swapped in at that deterministic name.
//
// path_mapping.json is written by the root daemon at points that can run
// AFTER job code has already executed as the target uid — the rewrite in
// executor's buildEffectiveLookup specifically runs after the environment
// onEnter action. Between a plain os.WriteFile and completion, the target uid
// could have replaced the entry with a SYMLINK (a naive open would follow it
// and write through to the target) or a HARDLINK to some other file on the
// same filesystem (O_TRUNC would follow the existing directory entry and
// overwrite THAT inode's content).
//
// This file needs no chown (it is always mode 0644, world-readable by
// design — see WritePathMappingFile), so unlike isolation.WriteFileFchown
// there is nothing to fchown here; the remove-then-O_EXCL-create shape below
// is otherwise identical to isolation.WriteFileFchown's own, so the two
// secure-write helpers in this codebase behave consistently. Any existing
// entry is removed first (best-effort; ErrNotExist is not an error — there
// may be nothing there yet, e.g. the very first write of a session), then
// the file is created fresh with O_EXCL|O_NOFOLLOW: if anything recreates an
// entry at
// path in the tiny window between the remove and this create, the O_EXCL
// open fails closed (EEXIST) rather than writing through whatever was
// recreated — the caller already treats a write failure here as a
// pre-execution task failure, so failing safe costs nothing correctness-wise.
func writeSecure(path string, data []byte, perm os.FileMode) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
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
	return nil
}
