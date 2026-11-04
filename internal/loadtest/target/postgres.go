// SPDX-License-Identifier: AGPL-3.0-or-later

package target

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver

	"github.com/uberware/sqi/internal/pgdsn"
)

// maintenanceDB is the database an administrative DSN names. Given one, the
// harness creates a database of its own for the run and drops it afterwards;
// given any other, it uses that database as it is.
const maintenanceDB = "postgres"

// maxIdentifier is PostgreSQL's identifier length limit, in bytes.
const maxIdentifier = 63

// isAdminDSN reports whether dsn names the maintenance database, the sign of
// an administrative DSN the harness may create run databases with.
func isAdminDSN(dsn string) (bool, error) {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return false, fmt.Errorf("target: PostgreSQL DSN %s: %w", pgdsn.Redact(dsn), err)
	}
	return cfg.Database == maintenanceDB, nil
}

// databaseName returns the run database's name for runID: lower-case
// letters, digits and underscores, within PostgreSQL's identifier limit.
func databaseName(runID string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(runID) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	id := b.String()
	if id == "" {
		id = "run"
	}
	name := "sqi_load_" + id
	if len(name) > maxIdentifier {
		name = name[:maxIdentifier]
	}
	return name
}

// withDatabase returns dsn naming database name instead. A URL gets a new
// path; a keyword/value DSN gets a trailing dbname, which wins over an
// earlier one.
func withDatabase(dsn, name string) (string, error) {
	out := dsn + " dbname=" + name
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("target: PostgreSQL DSN %s: %w", pgdsn.Redact(dsn), err)
		}
		u.Path = "/" + name
		u.RawPath = ""
		out = u.String()
	}
	cfg, err := pgconn.ParseConfig(out)
	if err != nil || cfg.Database != name {
		return "", fmt.Errorf("target: cannot point PostgreSQL DSN %s at database %s", pgdsn.Redact(dsn), name)
	}
	return out, nil
}

// createDatabase creates database name through the administrative DSN.
func createDatabase(ctx context.Context, adminDSN, name string) error {
	return execAdmin(ctx, adminDSN, `CREATE DATABASE "`+name+`"`) // name is databaseName's: letters, digits, underscores
}

// dropDatabase drops database name, disconnecting anything still on it.
func dropDatabase(ctx context.Context, adminDSN, name string) error {
	return execAdmin(ctx, adminDSN, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`)
}

func execAdmin(ctx context.Context, adminDSN, stmt string) error {
	db, err := sql.Open("pgx", adminDSN)
	if err != nil {
		return fmt.Errorf("target: open %s: %w", pgdsn.Redact(adminDSN), err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("target: %s on %s: %w", strings.Fields(stmt)[0]+" "+strings.Fields(stmt)[1], pgdsn.Redact(adminDSN), err)
	}
	return nil
}
