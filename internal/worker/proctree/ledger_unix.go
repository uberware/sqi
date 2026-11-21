// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package proctree

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// write records a tree: written to a temporary name, synced and renamed, so a
// crash never leaves a half-written record under the record's own name.
func (l *Ledger) write(rec record) (string, error) {
	b, err := json.Marshal(rec)
	if err != nil {
		return "", fmt.Errorf("proctree: encode record: %w", err)
	}
	path := filepath.Join(l.dir, rec.Token+recordExt)
	tmp := path + tmpExt
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("proctree: write record: %w", err)
	}
	_, werr := f.Write(b)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		os.Remove(tmp) // best effort; the sweep drops a leftover .tmp
		return "", fmt.Errorf("proctree: write record: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp) // best effort; the sweep drops a leftover .tmp
		return "", fmt.Errorf("proctree: write record: %w", err)
	}
	return path, nil
}

// removeRecord deletes a record written by write. An absent record is not an
// error.
func removeRecord(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("proctree: remove record: %w", err)
	}
	return nil
}
