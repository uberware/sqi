// SPDX-License-Identifier: AGPL-3.0-or-later

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

// WorkerIDFilePath returns the path [LoadOrCreateWorkerID] reads and writes
// under dataDir. Exported so other worker-side entry points that need to
// check for an existing worker ID without creating one — the "sqi-worker
// keygen" CLI, notably — derive the same path from the data directory
// rather than each hardcoding the filename.
func WorkerIDFilePath(dataDir string) string {
	return filepath.Join(dataDir, workerIDFilename)
}

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
//
// dataDir is NEVER widened for run-as-user traversal. As the shared ancestor
// of isolated session working directories it would have to be chmod'd
// traversable (0711) for isolated tasks to chdir into their own session — Go's
// forkAndExecInChild sets the child's process credentials BEFORE chdir. That
// coupling is backwards: worker.id is the worker's persistent,
// server-correlated identity and must stay private (0700) and byte-for-byte
// stable, while session scratch is ephemeral and needs to be world-traversable
// when isolation is in play. So a worker that can isolate (running as root, or
// on Windows) keeps its session working directories under a separate root,
// created traversable FROM BIRTH; only a non-root worker, which cannot isolate,
// keeps them under <dataDir>/sessions (see cmd/sqi-worker's
// effectiveSessionRoot and session.Manager). dataDir is created 0700 and its
// mode is never changed afterwards.
func LoadOrCreateWorkerID(dataDir string) (string, error) {
	if dataDir == "" {
		return "", errors.New("worker.data_dir must not be empty")
	}

	if mkErr := os.MkdirAll(dataDir, 0o700); mkErr != nil {
		return "", fmt.Errorf("create data dir %s: %w", dataDir, mkErr)
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

	// ── First start: generate a new UUID (dataDir already created above) ──────
	id := uuid.New().String()

	if writeErr := os.WriteFile(idFile, []byte(id+"\n"), 0o600); writeErr != nil {
		return "", fmt.Errorf("write worker id file %s: %w", idFile, writeErr)
	}

	return id, nil
}
