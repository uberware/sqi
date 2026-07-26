// SPDX-License-Identifier: AGPL-3.0-or-later

// Package fsutil holds small filesystem helpers shared across sqi.
//
// # AppleDouble files
//
// macOS stores a file's extended attributes and resource fork in a companion
// file named "._<original>" whenever the underlying filesystem cannot hold them
// natively — exFAT, FAT32, SMB and NFS shares, and most USB media. A checkout on
// such a volume therefore sprouts a "._foo.yaml" beside every "foo.yaml".
//
// Those companions are binary, carry no content sqi cares about, and — because
// they keep the original extension — are matched by every "*.yaml" glob and
// every `strings.HasSuffix(name, ".yaml")` filter. Code that scans a directory
// then tries to parse one and fails on a control character, reporting a corrupt
// file that does not exist. This has bitten this repository more than once, in
// unrelated subsystems, which is why the guard lives in one place.
package fsutil

import (
	"io/fs"
	"path"
	"strings"
)

// appleDoublePrefix is the base-name prefix macOS gives companion files.
const appleDoublePrefix = "._"

// IsAppleDouble reports whether a path's base name marks it as a macOS
// AppleDouble companion file. It accepts a bare name or a full path.
//
// Both separators are recognized on every platform: filepath.Base only splits
// on the host's separator, so a Windows-style path checked on Linux would
// otherwise be treated as one long base name and slip through.
func IsAppleDouble(name string) bool {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	return strings.HasPrefix(name, appleDoublePrefix)
}

// FilterPaths returns paths with AppleDouble companions removed, preserving
// order. It is the guard for [filepath.Glob] results, which match "._x.yaml"
// against "*.yaml" like any other file.
func FilterPaths(paths []string) []string {
	out := paths[:0:len(paths)]
	for _, p := range paths {
		if !IsAppleDouble(p) {
			out = append(out, p)
		}
	}
	return out
}

// FilterDirEntries returns entries with AppleDouble companions removed,
// preserving order. It is the guard for [os.ReadDir] and [fs.ReadDir] results.
func FilterDirEntries(entries []fs.DirEntry) []fs.DirEntry {
	out := entries[:0:len(entries)]
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), appleDoublePrefix) {
			out = append(out, e)
		}
	}
	return out
}

// HideAppleDouble wraps an [fs.FS] so AppleDouble companions are invisible to
// both directory listings and Open calls. Use it when a package hands an fs.FS
// to code it does not control.
func HideAppleDouble(inner fs.FS) fs.FS { return hidden{inner} }

type hidden struct{ inner fs.FS }

func (h hidden) Open(name string) (fs.File, error) {
	if strings.HasPrefix(path.Base(name), appleDoublePrefix) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return h.inner.Open(name)
}

func (h hidden) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(h.inner, name)
	if err != nil {
		return nil, err
	}
	return FilterDirEntries(entries), nil
}
