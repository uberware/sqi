// SPDX-License-Identifier: AGPL-3.0-or-later

package log

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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
	f          *os.File // nil after a failed reopen, until the next Write retries it
	closed     bool
	size       int64
	threshold  int64 // size at which the next rotation is attempted

	rename   func(oldpath, newpath string) error // os.Rename; replaced in tests
	remove   func(name string) error             // os.Remove
	openFile func(name string) (*os.File, error) // openLogFile
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
		openFile:   openLogFile,
	}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

// openLogFile opens name for appending, creating it if needed.
func openLogFile(name string) (*os.File, error) {
	//nolint:gosec // G302: log files are group-readable so a log shipper in the service's group can tail them
	return os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
}

func (r *RotatingFile) open() error {
	f, err := r.openFile(r.path)
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
//
// A failed reopen (a full disk, a transient lock) loses the record that hit it
// and returns the error, but leaves the writer without an open file rather than
// with a closed one: the next Write retries the open, so logging resumes as soon
// as the condition clears. Write after [RotatingFile.Close] is an error.
func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, fmt.Errorf("log: write %s: %w", r.path, fs.ErrClosed)
	}
	if r.f == nil {
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	if r.size > 0 && r.size+int64(len(p)) > r.threshold {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotate closes, shifts and reopens the file. Only a failure to close or to
// reopen is an error; a failed shift leaves the current file in place and defers
// the next attempt by another maxBytes. Either error leaves r.f nil, never
// pointing at the closed file, so the next Write reopens it.
func (r *RotatingFile) rotate() error {
	err := r.f.Close()
	r.f = nil
	if err != nil {
		return fmt.Errorf("log: close %s for rotation: %w", r.path, err)
	}
	if err := r.shift(); err != nil {
		r.threshold = r.size + r.maxBytes
	} else {
		r.threshold = r.maxBytes
	}
	return r.open()
}

// shift moves the current file to path.1, first shifting the backups up. The
// current file is moved aside before any backup is touched: that is the rename
// a handle held without FILE_SHARE_DELETE blocks, and failing it first leaves
// every backup in place, where failing it last would drop the oldest backup on
// each deferred retry. Should a later step fail, the current file is put back.
func (r *RotatingFile) shift() error {
	if r.maxBackups == 0 {
		return r.remove(r.path)
	}
	pending := r.path + ".rotating"
	if err := r.rename(r.path, pending); err != nil {
		return err
	}
	err := r.shiftBackups()
	if err == nil {
		err = r.rename(pending, backupName(r.path, 1))
	}
	if err != nil {
		if rbErr := r.rename(pending, r.path); rbErr != nil {
			return errors.Join(err, rbErr)
		}
	}
	return err
}

// shiftBackups drops the oldest backup and renames path.i to path.i+1.
func (r *RotatingFile) shiftBackups() error {
	if err := r.remove(backupName(r.path, r.maxBackups)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for i := r.maxBackups - 1; i >= 1; i-- {
		if err := r.rename(backupName(r.path, i), backupName(r.path, i+1)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func backupName(path string, i int) string { return path + "." + strconv.Itoa(i) }

// Close closes the current file. It is idempotent, and it returns nil when a
// failed reopen left no file open, so a shutdown path can always call it.
func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
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

// FileProblem describes what makes file unusable as a log file for
// [OpenRotatingFile], or returns "". It must name a file (not end in a path
// separator, not be an existing directory) in a directory that exists; example
// is the file name the suggested fix uses. Config validation reports it under
// log.file, so the messages name that key. An empty file is no problem: it
// means stderr.
func FileProblem(file, example string) string {
	if file == "" {
		return ""
	}
	if os.IsPathSeparator(file[len(file)-1]) {
		return fmt.Sprintf("%q ends with a path separator; log.file must name a file, such as %q",
			file, filepath.Join(file, example))
	}
	if info, err := os.Stat(file); err == nil && info.IsDir() {
		return fmt.Sprintf("%q is a directory; log.file must name a file, such as %q",
			file, filepath.Join(file, example))
	}
	dir := filepath.Dir(file)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return fmt.Sprintf("directory %q does not exist; create it or choose another path", dir)
	}
	return ""
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }
