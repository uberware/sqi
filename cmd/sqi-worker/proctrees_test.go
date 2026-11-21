// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestLockDataDir_SecondHolderRefused(t *testing.T) {
	dir := t.TempDir()
	first, err := lockDataDir(dir)
	if err != nil {
		t.Fatalf("first lockDataDir: %v", err)
	}
	if _, err := lockDataDir(dir); err == nil {
		t.Fatal("second lockDataDir on a held data dir = nil, want an error")
	} else if want := "another sqi-worker is already running with data_dir " + dir; err.Error() != want {
		t.Errorf("second lockDataDir error = %q, want %q", err, want)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	third, err := lockDataDir(dir)
	if err != nil {
		t.Fatalf("lockDataDir after Release: %v", err)
	}
	third.Release() //nolint:errcheck // end of test
}

func TestSweepPreviousTrees_CreatesLedgerDir(t *testing.T) {
	dir := t.TempDir()
	l, err := sweepPreviousTrees(t.Context(), dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("sweepPreviousTrees: %v", err)
	}
	if l == nil {
		t.Fatal("sweepPreviousTrees returned a nil ledger")
	}
	if fi, err := os.Stat(filepath.Join(dir, "proctrees")); err != nil || !fi.IsDir() {
		t.Errorf("proctrees dir: %v, %v; want a directory", fi, err)
	}
}

func TestSweepPreviousTrees_UncreatableDirFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "proctrees"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := sweepPreviousTrees(t.Context(), dir, slog.New(slog.DiscardHandler)); err == nil {
		t.Error("sweepPreviousTrees with proctrees a regular file = nil, want an error")
	}
}

func TestClaimDataDir_HoldsTheLockUntilReleased(t *testing.T) {
	dir := t.TempDir()
	release, ledger, err := claimDataDir(t.Context(), dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("claimDataDir: %v", err)
	}
	if ledger == nil {
		t.Fatal("claimDataDir returned a nil ledger")
	}
	if _, _, err := claimDataDir(t.Context(), dir, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("second claimDataDir while the first holds the lock = nil, want an error")
	}
	release()
	again, _, err := claimDataDir(t.Context(), dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("claimDataDir after release: %v", err)
	}
	again()
}

func TestClaimDataDir_SweepFailureReleasesTheLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "proctrees"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := claimDataDir(t.Context(), dir, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("claimDataDir with an uncreatable ledger = nil, want an error")
	}
	l, err := lockDataDir(dir)
	if err != nil {
		t.Fatalf("lock still held after a failed claim: %v", err)
	}
	l.Release() //nolint:errcheck // end of test
}
