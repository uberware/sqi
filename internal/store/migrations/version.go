// SPDX-License-Identifier: AGPL-3.0-or-later

package migrations

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/pressly/goose/v3"
)

// ErrSchemaTooNew means the database was migrated by a newer sqi-server than
// this one. sqi's schema is forward-only: the way back is restoring the backup
// taken before the upgrade, with the old binary.
var ErrSchemaTooNew = errors.New("database schema is newer than this sqi-server")

// SchemaTooNewError carries both versions.
type SchemaTooNewError struct{ DBVersion, BinaryVersion int64 }

func (e *SchemaTooNewError) Error() string {
	return fmt.Sprintf("database schema is version %d, newer than this sqi-server (%d); this binary is older than "+
		"the data: run the newer binary, or roll back by restoring the pre-upgrade backup "+
		"(see docs/operations.md, Rollback)", e.DBVersion, e.BinaryVersion)
}

// Is makes errors.Is(err, ErrSchemaTooNew) true.
func (*SchemaTooNewError) Is(target error) bool { return target == ErrSchemaTooNew }

// MaxVersion is the highest migration embedded for driver ("sqlite" or "postgres").
func MaxVersion(driver string) (int64, error) {
	var fsys fs.FS
	switch driver {
	case "sqlite":
		fsys = FS
	case "postgres":
		fsys = PostgresFS
	default:
		return 0, fmt.Errorf("migrations: unknown driver %q", driver)
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return 0, fmt.Errorf("migrations: list %s: %w", driver, err)
	}
	var maxV int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		v, err := goose.NumericComponent(e.Name()) // goose's own reading of the name
		if err != nil {
			return 0, fmt.Errorf("migrations: %w", err)
		}
		maxV = max(maxV, v)
	}
	return maxV, nil
}

// CheckVersion returns a *SchemaTooNewError when dbVersion is above
// MaxVersion(driver).
func CheckVersion(driver string, dbVersion int64) error {
	maxV, err := MaxVersion(driver)
	if err != nil {
		return err
	}
	if dbVersion > maxV {
		return &SchemaTooNewError{DBVersion: dbVersion, BinaryVersion: maxV}
	}
	return nil
}
