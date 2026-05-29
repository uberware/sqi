// SPDX-License-Identifier: AGPL-3.0-only

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

import "embed"

// FS is the embedded filesystem containing all SQL migration files.
// It is the source of truth passed to goose at runtime.
//
//go:embed *.sql
var FS embed.FS
