// SPDX-License-Identifier: AGPL-3.0-or-later

package migrations

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"

	"github.com/uberware/sqi/internal/fsutil"
)

//go:embed postgres/*.sql
var rawPostgres embed.FS

// PostgresFS holds the PostgreSQL migration set, rooted at its own directory.
// It starts at 00037_baseline.sql: version N is the same schema on both
// backends.
var PostgresFS = fsutil.HideAppleDouble(mustSub(rawPostgres, "postgres"))

func mustSub(f fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic(err) // the embed pattern guarantees the directory
	}
	return sub
}

// NewProvider returns a goose provider for driver ("sqlite" or "postgres")
// over db and that driver's migration set. A provider keeps its dialect and
// file system to itself, unlike goose's package-level SetDialect/SetBaseFS,
// so a SQLite and a PostgreSQL store can migrate in one process at once.
func NewProvider(db *sql.DB, driver string) (*goose.Provider, error) {
	switch driver {
	case "sqlite":
		return goose.NewProvider(goose.DialectSQLite3, db, FS)
	case "postgres":
		return goose.NewProvider(goose.DialectPostgres, db, PostgresFS)
	default:
		return nil, fmt.Errorf("migrations: unknown driver %q", driver)
	}
}
