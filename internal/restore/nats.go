// SPDX-License-Identifier: AGPL-3.0-or-later

package restore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// quarantineNATS moves the JetStream directory aside. Its messages belong to a
// different moment than the restored database, and after the fence nothing in
// it is needed. It returns the absolute directory looked at, and where it
// went ("" when there was none). An empty dir skips the step.
func quarantineNATS(dir, stamp string) (abs, movedTo string, err error) {
	if dir == "" {
		return "", "", nil
	}
	if abs, err = filepath.Abs(dir); err != nil {
		return "", "", fmt.Errorf("restore: NATS data dir %s: %w", dir, err)
	}
	if _, err := os.Stat(abs); errors.Is(err, fs.ErrNotExist) {
		return abs, "", nil
	} else if err != nil {
		return abs, "", fmt.Errorf("restore: NATS data dir %s: %w", abs, err)
	}
	if movedTo, err = moveAside(abs, stamp); err != nil {
		return abs, "", err
	}
	return abs, movedTo, nil
}
