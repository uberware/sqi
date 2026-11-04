// SPDX-License-Identifier: AGPL-3.0-or-later

package observe

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver

	"github.com/uberware/sqi/internal/loadtest/measure"
	"github.com/uberware/sqi/internal/pgdsn"
)

// A managed target keeps the server's state under its data directory: the
// SQLite database and its write-ahead log beside it, and the embedded broker's
// store in a subdirectory.
const (
	sqliteDBFile  = "sqi.db"
	sqliteWALFile = "sqi.db-wal"
	natsSubdir    = "nats"
)

// sampleDisk records the sizes of the SQLite database, its write-ahead log and
// the broker's store under dataDir. A file that does not exist is skipped: a
// PostgreSQL-backed server has no database file, and a WAL that was just
// checkpointed away has no size to report.
func sampleDisk(rec *measure.Recorder, t time.Time, dataDir string) error {
	var firstErr error
	note := func(err error) {
		if err != nil && !errors.Is(err, fs.ErrNotExist) && firstErr == nil {
			firstErr = err
		}
	}
	for _, f := range []struct{ file, series string }{
		{sqliteDBFile, "disk.sqlite_db_bytes"},
		{sqliteWALFile, "disk.sqlite_wal_bytes"},
	} {
		fi, err := os.Stat(filepath.Join(dataDir, f.file))
		if err != nil {
			note(err)
			continue
		}
		rec.Add(f.series, "bytes", t, float64(fi.Size()))
	}
	size, err := dirSize(filepath.Join(dataDir, natsSubdir))
	if err != nil {
		note(err)
	} else {
		rec.Add("disk.nats_bytes", "bytes", t, float64(size))
	}
	return firstErr
}

// dirSize totals the size of the regular files under dir. A file that vanishes
// while the walk runs (the broker compacts its store) counts as zero.
func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path != dir && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// pgSizer reads the size of the PostgreSQL database the server stores state in.
type pgSizer struct {
	dsn string
	db  *sql.DB
}

func (p *pgSizer) size(ctx context.Context) (float64, error) {
	if p.db == nil {
		db, err := sql.Open("pgx", p.dsn)
		if err != nil {
			return 0, p.redact(err)
		}
		db.SetMaxOpenConns(1)
		p.db = db
	}
	var n int64
	if err := p.db.QueryRowContext(ctx, "SELECT pg_database_size(current_database())").Scan(&n); err != nil {
		return 0, p.redact(err)
	}
	return float64(n), nil
}

// redact keeps the DSN's password out of an error that quotes the DSN.
func (p *pgSizer) redact(err error) error {
	msg := strings.ReplaceAll(err.Error(), p.dsn, pgdsn.Redact(p.dsn))
	return fmt.Errorf("PostgreSQL: %s", msg)
}

func (p *pgSizer) close() {
	if p.db != nil {
		_ = p.db.Close()
	}
}
