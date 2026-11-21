// SPDX-License-Identifier: AGPL-3.0-or-later

package configinit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
)

// File modes "config init" gives what it writes on POSIX hosts.
const (
	// modeShared is the system-wide file: root writes it and the service's
	// group reads it.
	modeShared os.FileMode = 0o640
	// modePrivate is any other file, readable by its owner alone.
	modePrivate os.FileMode = 0o600
	// modeSharedDir is the directory that holds the system-wide file.
	modeSharedDir os.FileMode = 0o755
	// modePrivateDir is a directory created for a private file or data.
	modePrivateDir os.FileMode = 0o700
)

// errGroupNotFound is what unixSys.lookupGroup returns for a group that does
// not exist, as opposed to a lookup that failed.
var errGroupNotFound = errors.New("group not found")

// unixSys is every interaction writeUnix has with the operating system, so
// the modes and owners it asks for can be checked on any host.
type unixSys struct {
	goos        string
	lookupGroup func(name string) (gid int, err error)
	chown       func(path string, uid, gid int) error
	chmod       func(path string, mode os.FileMode) error
	mkdirAll    func(path string, mode os.FileMode) error
	// writeTemp creates a new file in dir that only its owner can read, writes
	// content to it and returns its path.
	writeTemp func(dir string, content []byte) (string, error)
	rename    func(oldpath, newpath string) error
	remove    func(path string) error
}

// unixOpts says what writeUnix is writing.
type unixOpts struct {
	// Shared marks the system-wide file the service's group reads: mode 0640,
	// owned by root and Group.
	Shared bool
	// Group is the service's group; only Shared uses it.
	Group string
	// DataDir is created 0700 on macOS, where nothing else creates it.
	DataDir string
}

// useraddLine is the command that creates the account a service runs as.
func useraddLine(group string) string {
	return "sudo useradd --system --no-create-home --shell /usr/sbin/nologin " + group
}

// writeUnix writes content to target. The file is written under a temporary
// name beside target, given its owner and mode, and renamed over target, so a
// file that is replaced never keeps its old mode and a failure leaves the old
// file in place. A missing group is found before anything is written.
func writeUnix(target string, content []byte, opts unixOpts, sys unixSys) error {
	gid := -1
	if opts.Shared {
		var err error
		if gid, err = sharedGroup(opts.Group, sys); err != nil {
			return err
		}
	}
	dirMode := modePrivateDir
	if opts.Shared {
		dirMode = modeSharedDir
	}
	dir := path.Dir(target)
	if err := sys.mkdirAll(dir, dirMode); err != nil {
		return denied(fmt.Errorf("create %s: %w", dir, err))
	}
	if sys.goos == goosDarwin && opts.DataDir != "" {
		if err := sys.mkdirAll(opts.DataDir, modePrivateDir); err != nil {
			return denied(fmt.Errorf("create %s: %w", opts.DataDir, err))
		}
	}
	return replaceFile(target, dir, content, opts.Shared, gid, sys)
}

func sharedGroup(group string, sys unixSys) (int, error) {
	gid, err := sys.lookupGroup(group)
	switch {
	case errors.Is(err, errGroupNotFound):
		return 0, fmt.Errorf("group %q does not exist; create the service account first:\n  %s", group, useraddLine(group))
	case err != nil:
		return 0, fmt.Errorf("look up group %q: %w", group, err)
	}
	return gid, nil
}

// replaceFile writes content to a temporary file in dir, applies the mode and
// owner, and renames it over target, removing the temporary file on failure.
func replaceFile(target, dir string, content []byte, shared bool, gid int, sys unixSys) (err error) {
	tmp, err := sys.writeTemp(dir, content)
	if err != nil {
		return denied(fmt.Errorf("write %s: %w", target, err))
	}
	defer func() {
		if err != nil {
			_ = sys.remove(tmp) //nolint:errcheck // the write error is the one to report
		}
	}()
	mode := modePrivate
	if shared {
		mode = modeShared
		if err = sys.chown(tmp, 0, gid); err != nil {
			return denied(fmt.Errorf("set the owner of %s: %w", target, err))
		}
	}
	if err = sys.chmod(tmp, mode); err != nil {
		return denied(fmt.Errorf("set the mode of %s: %w", target, err))
	}
	if err = sys.rename(tmp, target); err != nil {
		return denied(fmt.Errorf("replace %s: %w", target, err))
	}
	return nil
}

// denied adds the way out to a permission error: the system-wide location
// needs root, and any other location can be chosen with --config.
func denied(err error) error {
	if !errors.Is(err, fs.ErrPermission) {
		return err
	}
	return fmt.Errorf("%w (re-run with sudo, or write elsewhere with --config)", err)
}

// unixWriter returns the platform write step for a POSIX host. The
// system-wide file is the Linux default target; every other target is private.
func unixWriter(sys unixSys, group string) writer {
	return func(req writeRequest) error {
		shared := sys.goos == "linux" && req.Target == DefaultTarget(req.Binary, req.Platform)
		return writeUnix(req.Target, req.Content, unixOpts{Shared: shared, Group: group, DataDir: req.DataDir}, sys)
	}
}
