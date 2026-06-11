// SPDX-License-Identifier: AGPL-3.0-or-later

// Package migrations embeds the SQL migration files used by goose to manage
// the sqi-server SQLite schema.
//
// The embedded [FS] is passed to goose via [goose.SetBaseFS] so that migration
// files are included in the binary and do not need to be present on disk at
// runtime.
//
// Migration files follow the goose naming convention:
//
//	<version>_<description>.sql
//
// where <version> is a zero-padded integer and each file contains
// "-- +goose Up" and "-- +goose Down" sections.
//
// The full schema is introduced in tasks 25–27; this package is wired up by
// the migrate subcommand (task 14) so the CLI is operational from the start.
package migrations

import (
	"embed"
	"io/fs"
	"path"
	"strings"
)

// FS is the embedded filesystem containing all SQL migration files.
// It filters out macOS AppleDouble metadata files (._*) that may appear on
// HFS+/APFS volumes mounted on Linux, which would otherwise cause goose to
// fail when parsing migration file names.
//
//go:embed *.sql
var raw embed.FS

// FS wraps the raw embed, hiding any file whose base name begins with "._".
var FS fs.FS = appleDoubleFilter{raw}

// appleDoubleFilter is an [fs.FS] wrapper that hides macOS AppleDouble
// companion files (those whose base names begin with "._") from directory
// listings and open calls. These files are created automatically on HFS+/APFS
// volumes and carry no content relevant to goose migrations.
type appleDoubleFilter struct{ inner embed.FS }

func (f appleDoubleFilter) Open(name string) (fs.File, error) {
	if strings.HasPrefix(path.Base(name), "._") {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return f.inner.Open(name)
}

func (f appleDoubleFilter) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := f.inner.ReadDir(name)
	if err != nil {
		return nil, err
	}
	out := entries[:0:len(entries)]
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "._") {
			out = append(out, e)
		}
	}
	return out, nil
}
