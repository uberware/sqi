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
// after job code has executed as the target uid: the rewrite in executor's
// buildEffectiveLookup runs after the environment onEnter action. By then the
// target uid could have replaced the entry with a symlink (an open would
// follow it and write through to the target) or a hardlink to another file on
// the same filesystem (O_TRUNC would overwrite that inode's content).
//
// The file needs no chown (it is always mode 0644; see WritePathMappingFile),
// so unlike isolation.WriteFileFchown there is nothing to fchown; otherwise
// it uses the same remove-then-O_EXCL-create shape. Any existing entry is
// removed first (ErrNotExist is not an error: the first write of a session
// finds nothing), then the file is created with O_EXCL|O_NOFOLLOW. If
// anything recreates an entry at path between the remove and the create, the
// O_EXCL open fails (EEXIST) rather than writing through it, and the caller
// treats the write failure as a pre-execution task failure.
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
