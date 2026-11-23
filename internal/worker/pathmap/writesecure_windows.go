// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package pathmap

import (
	"errors"
	"fmt"
	"os"
)

// writeSecure is the Windows counterpart of the one in writesecure_unix.go;
// see its doc for the threat model. NTFS symlinks and junctions need an
// explicit reparse-point call to create, unlike POSIX symlinks, so O_EXCL
// alone refuses to write through a pre-existing entry of any kind and no
// O_NOFOLLOW equivalent is needed.
func writeSecure(path string, data []byte, perm os.FileMode) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove existing %q: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("create %q: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write %q: %w", path, err)
	}
	return nil
}
