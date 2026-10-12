// SPDX-License-Identifier: AGPL-3.0-or-later

// Package migrations embeds the SQL migration files goose uses to manage the
// sqi-server state store schema, one set per driver: [FS] for SQLite (this
// directory, from 00001) and [PostgresFS] for PostgreSQL (the postgres
// subdirectory, from 00037_baseline.sql). Version N is the same schema on
// both backends, so every schema change after 00037 ships as a pair.
//
// [NewProvider] returns a goose provider over a database and its driver's set.
// The provider carries its own dialect and file system, so production code
// never touches goose's package-level SetBaseFS/SetDialect state: the SQLite
// and PostgreSQL stores' AutoMigrate and the "sqi-server migrate" subcommands
// all go through it. Embedding means the migration files ship inside the
// binary and need not be present on disk at runtime.
//
// Migration files follow the goose naming convention:
//
//	<version>_<description>.sql
//
// where <version> is a zero-padded integer and each file contains
// "-- +goose Up" and "-- +goose Down" sections.
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
