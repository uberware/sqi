// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"fmt"
	"os"
	"path/filepath"
)

// probeFileName is the file probeWritable writes in the broker's data
// directory.
const probeFileName = ".sqi-write-probe"

// probeBytes is how much probeWritable writes.
const probeBytes = 64 << 10

// fdHeadroomMin and fdHeadroomDivisor set the descriptors that must be free
// before the broker restarts: max(fdHeadroomMin, limit/fdHeadroomDivisor).
// A restart closes every connection and then reopens the listener and every
// stream's files while the workers redial, so one free descriptor is not
// enough.
const (
	fdHeadroomMin     = 64
	fdHeadroomDivisor = 20
)

// probeRecovered reports whether what broke a stream has cleared: dir takes a
// write and there are descriptors to spare.
func probeRecovered(dir string) error {
	if err := probeWritable(dir); err != nil {
		return err
	}
	return fdHeadroom()
}

// probeWritable writes probeBytes to a probe file in dir, syncs it and
// removes it.
func probeWritable(dir string) error {
	path := filepath.Join(dir, probeFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	_, werr := f.Write(make([]byte, probeBytes))
	serr := f.Sync()
	cerr := f.Close()
	rerr := os.Remove(path)
	switch {
	case werr != nil:
		return fmt.Errorf("write %s: %w", path, werr)
	case serr != nil:
		return fmt.Errorf("sync %s: %w", path, serr)
	case cerr != nil:
		return fmt.Errorf("close %s: %w", path, cerr)
	case rerr != nil:
		return fmt.Errorf("remove %s: %w", path, rerr)
	}
	return nil
}

// fdHeadroomFrom reports whether open descriptors leave enough of limit free.
func fdHeadroomFrom(open, limit uint64) error {
	need := max(uint64(fdHeadroomMin), limit/fdHeadroomDivisor)
	if limit < need || open > limit-need {
		return fmt.Errorf("%d of %d file descriptors are open; a restart needs %d free", open, limit, need)
	}
	return nil
}
