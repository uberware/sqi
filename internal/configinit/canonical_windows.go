// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package configinit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// canonicalTarget is canonicalPath: the target is judged, written and
// printed in the one spelling Win32 opens.
func canonicalTarget(path string) (string, error) {
	return canonicalPath(path)
}

// canonicalPath is the one spelling of path that config init compares,
// checks and writes. Win32 opens many spellings of a directory as the same
// one, so comparing the spelling given would let C:\ProgramData\sqi. or
// C:\PROGRA~3\sqi pass for some other directory:
//
//   - GetFullPathName makes it absolute and resolves . and .. components;
//   - trailing dots and spaces, which Win32 drops from a name it opens, are
//     dropped from every component (a component of only dots and spaces is
//     refused);
//   - GetLongPathName expands 8.3 names in the longest part that exists,
//     and the rest is appended as given.
//
// A \\?\, \\.\ or \??\ path, which skips Win32's normalization, is refused.
func canonicalPath(path string) (string, error) {
	if isDevicePath(path) {
		return "", deviceErr(path)
	}
	full, err := fullPathName(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}
	if isDevicePath(full) {
		return "", deviceErr(path)
	}
	vol := filepath.VolumeName(full)
	if vol == "" || !filepath.IsAbs(full) {
		return "", fmt.Errorf("resolve %s: not an absolute path", path)
	}
	var parts []string
	for c := range strings.SplitSeq(full[len(vol):], `\`) {
		if c == "" {
			continue
		}
		trimmed := strings.TrimRight(c, ". ")
		if trimmed == "" {
			return "", fmt.Errorf("resolve %s: %q names no file or directory", path, c)
		}
		parts = append(parts, trimmed)
	}
	root := vol + `\`
	for n := len(parts); n >= 0; n-- {
		existing := filepath.Join(append([]string{root}, parts[:n]...)...)
		long, err := longExisting(existing)
		if err != nil {
			// Missing, or unreadable to an unelevated caller inside an
			// administrators-only directory: expand what is above it.
			if n > 0 && (errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission)) {
				continue
			}
			return "", fmt.Errorf("resolve %s: %w", path, err)
		}
		return filepath.Join(append([]string{long}, parts[n:]...)...), nil
	}
	return "", fmt.Errorf("resolve %s: no part of it exists", path)
}

// longExisting is the long-name spelling of path, which must exist.
func longExisting(path string) (string, error) {
	if _, err := os.Lstat(path); err != nil {
		return "", err
	}
	return longPathName(path)
}

// isDevicePath reports a \\?\, \\.\ or \??\ prefix, in either slash.
func isDevicePath(path string) bool {
	p := strings.ReplaceAll(path, "/", `\`)
	return strings.HasPrefix(p, `\\?\`) || strings.HasPrefix(p, `\\.\`) || strings.HasPrefix(p, `\??\`)
}

func deviceErr(path string) error {
	return fmt.Errorf(`%s: a \\?\ or \\.\ path is not accepted; pass an ordinary one, e.g. C:\ProgramData\sqi-render\sqi-worker.yaml`, path)
}

func fullPathName(path string) (string, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	return growBuffer(func(buf []uint16, size uint32) (uint32, error) {
		return windows.GetFullPathName(p, size, &buf[0], nil)
	})
}

func longPathName(path string) (string, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	return growBuffer(func(buf []uint16, size uint32) (uint32, error) {
		return windows.GetLongPathName(p, &buf[0], size)
	})
}

// growBuffer calls a Win32 function, given a buffer of size elements, that
// returns the length it needs when the buffer is too small, until the result
// fits.
func growBuffer(call func(buf []uint16, size uint32) (uint32, error)) (string, error) {
	size := uint32(windows.MAX_PATH)
	for {
		buf := make([]uint16, size)
		n, err := call(buf, size)
		if err != nil {
			return "", err
		}
		if n < size {
			return windows.UTF16ToString(buf[:n]), nil
		}
		size = n
	}
}
