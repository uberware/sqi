// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// workerIDFilename is the name of the file that persists the worker's UUID.
const workerIDFilename = "worker.id"

// LoadOrCreateWorkerID returns the worker's persistent UUID.
//
// On the first call for a given dataDir the function:
//  1. Creates dataDir (and any missing parents) with mode 0700.
//  2. Generates a new random UUID.
//  3. Writes it to <dataDir>/worker.id (mode 0600).
//
// On subsequent calls the existing file is read and the UUID returned
// unchanged, so the server can correlate the worker across restarts.
//
// An error is returned if the directory cannot be created, the file cannot be
// read or written, or the stored value is not a valid UUID.
func LoadOrCreateWorkerID(dataDir string) (string, error) {
	if dataDir == "" {
		return "", errors.New("worker.data_dir must not be empty")
	}

	idFile := filepath.Join(dataDir, workerIDFilename)

	// ── Try to read an existing ID ────────────────────────────────────────────
	data, err := os.ReadFile(idFile)
	if err == nil {
		id := strings.TrimSpace(string(data))
		if _, parseErr := uuid.Parse(id); parseErr != nil {
			return "", fmt.Errorf("worker id file %s contains invalid UUID %q: %w", idFile, id, parseErr)
		}
		return id, nil
	}

	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read worker id file %s: %w", idFile, err)
	}

	// ── First start: create data dir and generate a new UUID ──────────────────
	if mkErr := os.MkdirAll(dataDir, 0o700); mkErr != nil {
		return "", fmt.Errorf("create data dir %s: %w", dataDir, mkErr)
	}

	id := uuid.New().String()

	if writeErr := os.WriteFile(idFile, []byte(id+"\n"), 0o600); writeErr != nil {
		return "", fmt.Errorf("write worker id file %s: %w", idFile, writeErr)
	}

	return id, nil
}
