// SPDX-License-Identifier: AGPL-3.0-or-later

package restore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/uberware/sqi/internal/store/backend"
	"github.com/uberware/sqi/internal/store/migrations"
)

// ErrNotSQIBackup means the file is not a database or archive sqi wrote.
var ErrNotSQIBackup = errors.New("restore: not an sqi backup")

// Options says what Run restores, into which store.
type Options struct {
	Driver      string // backend.DriverSQLite (also "") or backend.DriverPostgres
	SQLitePath  string
	PostgresDSN string
	NATSDataDir string // "" skips the quarantine
	Backup      string // the backup to restore; "" with InPlace
	InPlace     bool
	Now         func() time.Time // nil: time.Now
	Logger      *slog.Logger
}

// Result reports what Run did.
type Result struct {
	BackupVersion int64    // 0 with InPlace
	Version       int64    // after migrate up
	Fenced        int      // attempts the fence handed back
	MovedAside    []string // where the previous database files went
	NATSDir       string   // the absolute directory looked at
	NATSMovedTo   string   // "" when there was none
}

// stampLayout names moved-aside files: sqi.db.pre-restore-20261012T153000Z.
const stampLayout = "20060102T150405Z"

// rename is os.Rename, replaceable by tests (SetRenameForTest).
var rename = os.Rename

// Run restores o.Backup (or, with InPlace, the database already in place) and
// makes it safe to start a server on: lock, check, put in place, migrate,
// fence, quarantine the NATS directory. Each step must succeed before the
// next. The lock is held until Run returns.
func Run(ctx context.Context, o Options) (Result, error) {
	if err := validate(o); err != nil {
		return Result{}, err
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	now := o.Now().UTC()
	stamp := now.Format(stampLayout)
	bo := backend.Options{
		Driver: o.Driver, SQLitePath: o.SQLitePath, PostgresDSN: o.PostgresDSN, MaxConns: 2, Logger: o.Logger,
	}

	release, err := backend.LockStore(ctx, bo)
	if err != nil {
		return Result{}, fmt.Errorf("restore: %w", err)
	}
	defer release() //nolint:errcheck // releasing on exit; the OS drops the lock with the process anyway

	var res Result
	if o.InPlace {
		err = requireInPlace(ctx, o)
	} else {
		res.BackupVersion, res.MovedAside, err = place(ctx, o, stamp)
	}
	if err != nil {
		return res, err
	}
	if err := finish(ctx, o, bo, now, stamp, &res); err != nil {
		return res, fmt.Errorf("%w%s", err, previousAt(res.MovedAside))
	}
	return res, nil
}

// validate refuses options Run cannot act on.
func validate(o Options) error {
	if o.InPlace == (o.Backup != "") {
		return errors.New("restore: give a backup file, or --in-place, not both")
	}
	if backend.Name(o.Driver) == backend.DriverSQLite && o.SQLitePath == "" {
		return errors.New("restore: the SQLite database path is empty; use --db, set store.sqlite_path, " +
			"or set SQI_STORE_SQLITE_PATH")
	}
	return nil
}

// place puts o.Backup in place of the configured database and returns the
// backup's schema version and where the previous database files went.
func place(ctx context.Context, o Options, stamp string) (version int64, moved []string, err error) {
	if backend.Name(o.Driver) == backend.DriverPostgres {
		version, err = placePostgres(ctx, o)
		return version, nil, err
	}
	return placeSQLite(ctx, o, stamp)
}

// requireInPlace refuses --in-place on a database that is not there (SQLite)
// or has no sqi schema (PostgreSQL): opening it would create, and migrate, an
// empty one.
func requireInPlace(ctx context.Context, o Options) error {
	if backend.Name(o.Driver) == backend.DriverPostgres {
		return requireInPlacePostgres(ctx, o)
	}
	if _, err := os.Stat(o.SQLitePath); err != nil {
		return fmt.Errorf("restore: --in-place needs the database at %s: %w", o.SQLitePath, err)
	}
	n, err := walSize(o.SQLitePath)
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("restore: --in-place refuses %[1]s: its -wal file holds %[2]d bytes of writes not yet "+
			"in the database file, and nothing ties a -wal file to the database it was written for, so SQLite "+
			"would apply them to whatever database is at %[1]s. If you put this database in place by hand over "+
			"one whose server stopped uncleanly, the -wal and -shm files are that old database's: move them aside "+
			"together with it, or restore with \"sqi-server restore BACKUP\" instead. If they are this database's "+
			"own (its server crashed, or a snapshot took them with it), let SQLite fold them in first: run "+
			"\"sqi-server migrate up\" on it, or start the server on it and stop it cleanly once; then run "+
			"--in-place again", o.SQLitePath, n)
	}
	return nil
}

// walSize is the size of db's -wal file, 0 when there is none. A cleanly
// stopped server leaves none, or an empty one; a non-empty one holds committed
// writes not yet copied into the database file.
func walSize(db string) (int64, error) {
	fi, err := os.Stat(db + "-wal")
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("restore: %w", err)
	}
	return fi.Size(), nil
}

// notFenced is the state a failure before the fence completes leaves.
const notFenced = "the database in place is not fenced, so do not start a server on it: " +
	"fix the cause and run \"sqi-server restore --in-place\" to finish"

// finish migrates the database now in place, fences it, and moves the NATS
// directory aside, filling in res. Each failure says what state it left.
func finish(ctx context.Context, o Options, bo backend.Options, now time.Time, stamp string, res *Result) error {
	bo.AutoMigrate = true // the version guard runs inside Open
	st, err := backend.Open(ctx, bo)
	if err != nil {
		return fmt.Errorf("restore: open the restored database: %w; %s", err, notFenced)
	}
	defer st.Close()
	if res.Version, err = migrations.MaxVersion(backend.Name(o.Driver)); err != nil {
		return fmt.Errorf("%w; %s", err, notFenced)
	}
	if res.Fenced, err = Fence(ctx, st, now); err != nil {
		return fmt.Errorf("%w; %s", err, notFenced)
	}
	if res.NATSDir, res.NATSMovedTo, err = quarantineNATS(o.NATSDataDir, stamp); err != nil {
		return fmt.Errorf("%w; the database in place is restored and fenced, but the NATS data directory "+
			"was not moved aside: move it aside by hand, or run \"sqi-server restore --in-place\", "+
			"before starting a server", err)
	}
	return nil
}

// moveAside renames p to p.pre-restore-<stamp> and returns the new name. It
// refuses to replace anything already there.
func moveAside(p, stamp string) (string, error) {
	to := p + ".pre-restore-" + stamp
	if _, err := os.Lstat(to); err == nil {
		return "", fmt.Errorf("restore: cannot move %s aside: %s already exists", p, to)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("restore: cannot move %s aside: %w", p, err)
	}
	if err := rename(p, to); err != nil {
		return "", fmt.Errorf("restore: move %s aside: %w", p, err)
	}
	return to, nil
}

// previousAt appends where the previous database went to an error message:
// every file moved aside, since a database and its -wal/-shm files are one
// database only together.
func previousAt(moved []string) string {
	if len(moved) == 0 {
		return ""
	}
	return fmt.Sprintf(" (the previous database was moved aside as %s; to go back to it, "+
		"rename all of these back together)", strings.Join(moved, ", "))
}
