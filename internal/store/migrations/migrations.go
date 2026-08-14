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
// The full schema lives in the numbered migration files; this package is wired
// up by the migrate subcommand so the CLI is operational from the start.
package migrations

import (
	"embed"

	"github.com/uberware/sqi/internal/fsutil"
)

// FS is the embedded filesystem containing all SQL migration files.
// It filters out macOS AppleDouble metadata files (._*) that may appear on
// HFS+/APFS volumes mounted on Linux, which would otherwise cause goose to
// fail when parsing migration file names.
//
//go:embed *.sql
var raw embed.FS

// FS wraps the raw embed, hiding macOS AppleDouble companion files ("._x.sql")
// from goose. See internal/fsutil for why they appear and what they break.
var FS = fsutil.HideAppleDouble(raw)
