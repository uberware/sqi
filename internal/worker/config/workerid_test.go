// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func TestLoadOrCreateWorkerID_CreatesOnFirstRun(t *testing.T) {
	dir := t.TempDir()

	id, err := LoadOrCreateWorkerID(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, parseErr := uuid.Parse(id); parseErr != nil {
		t.Fatalf("returned value %q is not a valid UUID: %v", id, parseErr)
	}

	// File must exist.
	data, err := os.ReadFile(filepath.Join(dir, workerIDFilename))
	if err != nil {
		t.Fatalf("worker id file not written: %v", err)
	}
	if got := trimNewline(string(data)); got != id {
		t.Errorf("file contains %q, want %q", got, id)
	}
}

func TestLoadOrCreateWorkerID_ReusesExistingID(t *testing.T) {
	dir := t.TempDir()

	id1, err := LoadOrCreateWorkerID(dir)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	id2, err := LoadOrCreateWorkerID(dir)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}

	if id1 != id2 {
		t.Errorf("worker ID changed between restarts: %q vs %q", id1, id2)
	}
}

func TestLoadOrCreateWorkerID_CreatesMissingDataDir(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "nested", "data")

	_, err := LoadOrCreateWorkerID(dir)
	if err != nil {
		t.Fatalf("should create missing directory: %v", err)
	}
	if _, statErr := os.Stat(dir); statErr != nil {
		t.Errorf("data dir not created: %v", statErr)
	}
}

func TestLoadOrCreateWorkerID_RejectsInvalidStoredUUID(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, workerIDFilename), []byte("not-a-uuid\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadOrCreateWorkerID(dir); err == nil {
		t.Error("expected error for invalid UUID in file, got nil")
	}
}

func TestLoadOrCreateWorkerID_EmptyDataDirError(t *testing.T) {
	if _, err := LoadOrCreateWorkerID(""); err == nil {
		t.Error("expected error for empty data dir, got nil")
	}
}

// trimNewline trims a trailing newline from s.
func trimNewline(s string) string {
	if len(s) > 0 && s[len(s)-1] == '\n' {
		return s[:len(s)-1]
	}
	return s
}
