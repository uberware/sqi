// SPDX-License-Identifier: AGPL-3.0-or-later

package fslock_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/fslock"
)

func TestAcquire_SecondAcquireIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sqi.db.lock")
	l, err := fslock.Acquire(path)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	t.Cleanup(func() {
		if err := l.Release(); err != nil {
			t.Errorf("Release: %v", err)
		}
	})

	// A second open of the same file is a separate open file description
	// (flock) or handle (LockFileEx), so it conflicts even in-process.
	if _, err := fslock.Acquire(path); !errors.Is(err, fslock.ErrHeld) {
		t.Fatalf("second Acquire = %v, want ErrHeld", err)
	}
}

func TestRelease_LetsTheNextAcquireSucceed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sqi.db.lock")
	l, err := fslock.Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	l2, err := fslock.Acquire(path)
	if err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	if err := l2.Release(); err != nil {
		t.Fatalf("Release of the second lock: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the lock file must stay after Release (deleting it races a waiter): %v", err)
	}
}

// TestAcquire_AKilledHolderLeavesNoStaleLock runs this test binary as a child
// that takes the lock and blocks, kills it, and takes the lock in the parent.
func TestAcquire_AKilledHolderLeavesNoStaleLock(t *testing.T) {
	if os.Getenv("SQI_FSLOCK_HOLD") != "" {
		l, err := fslock.Acquire(os.Getenv("SQI_FSLOCK_HOLD"))
		if err != nil {
			os.Exit(3)
		}
		// Collect now, so a Lock the child let go of is finalized (closing the
		// file and dropping the lock) before the parent looks, not by chance
		// later.
		runtime.GC()
		runtime.GC()
		time.Sleep(200 * time.Millisecond) // finalizers run on their own goroutine
		os.Stdout.WriteString("held\n")
		time.Sleep(time.Hour)
		runtime.KeepAlive(l) // the lock lives only as long as the *Lock does
		return
	}
	path := filepath.Join(t.TempDir(), "sqi.db.lock")
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestAcquire_AKilledHolderLeavesNoStaleLock$")
	cmd.Env = append(os.Environ(), "SQI_FSLOCK_HOLD="+path)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := out.Read(buf); err != nil || !strings.HasPrefix(string(buf), "held") {
		t.Fatalf("child did not report holding the lock: %q, %v", buf, err)
	}
	if _, err := fslock.Acquire(path); !errors.Is(err, fslock.ErrHeld) {
		t.Fatalf("Acquire while the child holds it = %v, want ErrHeld", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait() //nolint:errcheck // the killed child exits non-zero by design
	l, err := fslock.Acquire(path)
	if err != nil {
		t.Fatalf("Acquire after the holder was killed: %v (stale lock)", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}
