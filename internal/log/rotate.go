// SPDX-License-Identifier: AGPL-3.0-or-later

package log

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"sync"
)

// RotatingFile appends to a log file and rotates it by size: path becomes
// path.1, path.1 becomes path.2, and so on up to maxBackups, oldest dropped.
//
// Rotation never costs a log line. On Windows a rename fails while another
// process holds the file open without FILE_SHARE_DELETE (an operator tailing
// it, a log shipper); the writer then keeps appending to the current file and
// retries only after another maxBytes of output, rather than retrying on every
// write or returning the error — slog would drop the record.
type RotatingFile struct {
	mu         sync.Mutex
	path       string
	maxBytes   int64
	maxBackups int
	f          *os.File
	size       int64
	threshold  int64 // size at which the next rotation is attempted

	rename func(oldpath, newpath string) error // os.Rename; replaced in tests
	remove func(name string) error             // os.Remove
}

// OpenRotatingFile opens (creating or appending to) path, rotating it once it
// exceeds maxSizeMB megabytes and keeping at most maxBackups rotated files.
func OpenRotatingFile(path string, maxSizeMB, maxBackups int) (*RotatingFile, error) {
	if maxSizeMB <= 0 {
		return nil, fmt.Errorf("log: max size must be positive, got %d MB", maxSizeMB)
	}
	if maxBackups < 0 {
		return nil, fmt.Errorf("log: max backups must not be negative, got %d", maxBackups)
	}
	return openRotatingFile(path, int64(maxSizeMB)<<20, maxBackups)
}

func openRotatingFile(path string, maxBytes int64, maxBackups int) (*RotatingFile, error) {
	r := &RotatingFile{
		path:       path,
		maxBytes:   maxBytes,
		maxBackups: maxBackups,
		threshold:  maxBytes,
		rename:     os.Rename,
		remove:     os.Remove,
	}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *RotatingFile) open() error {
	//nolint:gosec // G302: log files are group-readable so a log shipper in the service's group can tail them
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("log: open %s: %w", r.path, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close() // already returning the more useful Stat error
		return fmt.Errorf("log: stat %s: %w", r.path, err)
	}
	r.f, r.size = f, info.Size()
	return nil
}

// Write appends p, rotating first when p would take the file past its limit.
func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size > 0 && r.size+int64(len(p)) > r.threshold {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotate closes, shifts and reopens the file. Only a failure to reopen is an
// error; a failed shift leaves the current file in place and defers the next
// attempt by another maxBytes.
func (r *RotatingFile) rotate() error {
	if err := r.f.Close(); err != nil {
		return fmt.Errorf("log: close %s for rotation: %w", r.path, err)
	}
	if err := r.shift(); err != nil {
		r.threshold = r.size + r.maxBytes
	} else {
		r.threshold = r.maxBytes
	}
	return r.open()
}

func (r *RotatingFile) shift() error {
	if r.maxBackups == 0 {
		return r.remove(r.path)
	}
	if err := r.remove(backupName(r.path, r.maxBackups)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for i := r.maxBackups - 1; i >= 1; i-- {
		if err := r.rename(backupName(r.path, i), backupName(r.path, i+1)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return r.rename(r.path, backupName(r.path, 1))
}

func backupName(path string, i int) string { return path + "." + strconv.Itoa(i) }

// Close closes the current file.
func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}

// Output returns the log destination for a configured log file: a
// [RotatingFile] when path is set, otherwise stderr behind a Close that does
// nothing (the process must keep its stderr).
func Output(path string, maxSizeMB, maxBackups int) (io.WriteCloser, error) {
	if path == "" {
		return nopCloser{os.Stderr}, nil
	}
	// Not `return OpenRotatingFile(...)`: on error that would wrap a nil
	// *RotatingFile in a non-nil interface.
	r, err := OpenRotatingFile(path, maxSizeMB, maxBackups)
	if err != nil {
		return nil, err
	}
	return r, nil
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }
