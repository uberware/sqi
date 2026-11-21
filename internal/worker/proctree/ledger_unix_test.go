// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package proctree

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestTrack_WritesRecordAndReleaseRemovesIt(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenLedger(dir)
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	cmd, tree := startSleepTree(t)
	if err := tree.Track(l, Label{Kind: "task", AttemptID: "a1"}); err != nil {
		t.Fatalf("Track: %v", err)
	}
	tok := envToken(cmd.Env)

	// The on-disk keys are spelled out here rather than taken from record: a
	// newer worker must read the records an older one wrote.
	path := filepath.Join(dir, tok+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	var rec struct {
		Token  string `json:"token"`
		PGID   int    `json:"pgid"`
		BootID string `json:"boot_id"`
		Label  struct {
			Kind      string `json:"kind"`
			AttemptID string `json:"attempt_id"`
		} `json:"label"`
		StartedAt string `json:"started_at"`
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("decode record %s: %v", b, err)
	}
	if rec.Token != tok || rec.PGID != cmd.Process.Pid || rec.Label.Kind != "task" || rec.Label.AttemptID != "a1" || rec.StartedAt == "" {
		t.Errorf("record = %s; want token %s, pgid %d, kind task, attempt a1, a start time", b, tok, cmd.Process.Pid)
	}
	if runtime.GOOS == "linux" && rec.BootID == "" {
		t.Errorf("record has no boot_id on Linux: %s", b)
	}
	left, err := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if err != nil || len(left) > 0 {
		t.Errorf("temporary files left behind: %v (glob err %v)", left, err)
	}

	if err := tree.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	cmd.Wait() //nolint:errcheck // killed
	if err := tree.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("record still present after Release: stat err = %v", err)
	}
}

func TestTrack_WriteFailureIsReturned(t *testing.T) {
	dir := t.TempDir() + "/proctrees"
	l, err := OpenLedger(dir)
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	_, tree := startSleepTree(t)
	if err := tree.Track(l, Label{Kind: "task"}); err == nil {
		t.Error("Track into a removed ledger dir = nil, want an error")
	}
	if err := tree.Release(); err != nil {
		t.Errorf("Release after a failed Track = %v, want nil", err)
	}
}

func TestTrack_UnmarkedCommandIsRefused(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenLedger(dir)
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), "sleep", "30")
	Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	tree, err := Supervise(cmd)
	if err != nil {
		t.Fatalf("Supervise: %v", err)
	}
	t.Cleanup(func() {
		tree.Kill() //nolint:errcheck // cleanup
		cmd.Wait()  //nolint:errcheck // reaps it
	})
	if err := tree.Track(l, Label{Kind: "task"}); err == nil {
		t.Error("Track of a command MarkEnv never marked = nil, want an error")
	}
	if left := ledgerFiles(t, l); len(left) != 0 {
		t.Errorf("an unmarked command was recorded: %v", left)
	}
}
