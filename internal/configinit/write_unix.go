// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package configinit

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"runtime"
	"strconv"
)

// platformWriter writes through the real file system and account database.
func platformWriter(spec Spec) writer {
	return unixWriter(realUnixSys(), spec.UnixGroup)
}

func realUnixSys() unixSys {
	return unixSys{
		goos:        runtime.GOOS,
		lookupGroup: lookupGroup,
		chown:       os.Chown,
		chmod:       os.Chmod,
		mkdirAll:    os.MkdirAll,
		isDir:       isDir,
		writeTemp:   writeTemp,
		rename:      os.Rename,
		remove:      os.Remove,
	}
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

func lookupGroup(name string) (int, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		if _, ok := errors.AsType[user.UnknownGroupError](err); ok {
			return 0, errGroupNotFound
		}
		return 0, err
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return 0, fmt.Errorf("group %q has gid %q: %w", name, g.Gid, err)
	}
	return gid, nil
}

// writeTemp creates a file in dir that only its owner can read.
func writeTemp(dir string, content []byte) (string, error) {
	f, err := os.CreateTemp(dir, ".sqi-config-*")
	if err != nil {
		return "", err
	}
	if _, err := f.Write(content); err != nil {
		return "", closeAndRemove(f, err)
	}
	if err := f.Close(); err != nil {
		return "", closeAndRemove(f, err)
	}
	return f.Name(), nil
}

func closeAndRemove(f *os.File, cause error) error {
	_ = f.Close()
	_ = os.Remove(f.Name())
	return cause
}
