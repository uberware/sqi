// SPDX-License-Identifier: AGPL-3.0-or-later

package diskfill

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// The names Fill writes under the directory it is given.
const (
	// BallastName is the file that takes up the volume's free space.
	BallastName = ".sqiload-ballast"
	// InodesName is the directory of empty files that take up its inodes.
	InodesName = ".sqiload-inodes"
)

// ErrUnsupported is what Fill returns off unix.
var ErrUnsupported = errors.New("diskfill: needs statfs, which this OS does not have")

// Ballast is what Fill wrote on one volume.
type Ballast struct {
	dir string

	mu    sync.Mutex
	freed bool
}

// Free removes everything Fill wrote. It is safe on a nil Ballast and more
// than once.
func (b *Ballast) Free() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.freed {
		return nil
	}
	if err := removeFilled(b.dir); err != nil {
		return err
	}
	b.freed = true
	return nil
}

// removeFilled removes the ballast file and the inode files under dir. A
// missing one is not an error.
func removeFilled(dir string) error {
	var errs []error
	for _, name := range []string{BallastName, InodesName} {
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			errs = append(errs, fmt.Errorf("diskfill: remove %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// Adopt returns the Ballast for whatever an earlier Fill left in dir, so a
// later process can free it. Freeing a directory Fill never wrote to removes
// nothing.
func Adopt(dir string) *Ballast { return &Ballast{dir: dir} }
