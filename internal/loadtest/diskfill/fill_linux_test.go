// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux

package diskfill

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

const (
	volumeBytes  = 64 << 20
	volumeInodes = 4096
)

// smallVolume mounts a tmpfs of volumeBytes and volumeInodes at a fresh
// directory and returns it. It skips unless the process may mount.
func smallVolume(t *testing.T) string {
	t.Helper()
	const reason = "needs a size-limited volume: run under make chaos-container"
	if os.Geteuid() != 0 {
		t.Skip(reason)
	}
	dir := t.TempDir()
	if err := unix.Mount("tmpfs", dir, "tmpfs", 0, "size=64m,nr_inodes=4096"); err != nil {
		t.Skipf("%s (mount: %v)", reason, err)
	}
	t.Cleanup(func() { unix.Unmount(dir, unix.MNT_DETACH) }) //nolint:errcheck // best-effort unmount
	return dir
}

func freeBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		t.Fatalf("statfs: %v", err)
	}
	return int64(st.Bavail) * st.Bsize
}

// writeBytes writes n bytes to a new file under dir and returns the error.
func writeBytes(dir, name string, n int) error {
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(make([]byte, n))
	return errors.Join(werr, f.Sync(), f.Close())
}

func TestFill_LeavesHeadroom(t *testing.T) {
	dir := smallVolume(t)
	const headroom = 1 << 20
	if freeBytes(t, dir) < 4*headroom {
		t.Fatalf("test volume has only %d free", freeBytes(t, dir))
	}
	b, err := Fill(dir, headroom, false)
	if err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if free := freeBytes(t, dir); free > headroom+4096 || free < headroom-4096 {
		t.Errorf("free after Fill = %d, want about %d", free, headroom)
	}
	if err := writeBytes(dir, "x", 2<<20); !errors.Is(err, syscall.ENOSPC) {
		t.Errorf("a 2 MiB write on the filled volume = %v, want ENOSPC", err)
	}
	if err := b.Free(); err != nil {
		t.Fatalf("Free: %v", err)
	}
	if err := writeBytes(dir, "y", 2<<20); err != nil {
		t.Errorf("a 2 MiB write after Free = %v, want success", err)
	}
	if err := b.Free(); err != nil {
		t.Errorf("second Free = %v, want nil", err)
	}
}

func TestFill_Inodes(t *testing.T) {
	dir := smallVolume(t)
	b, err := Fill(dir, 0, true)
	if err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o750); !errors.Is(err, syscall.ENOSPC) {
		t.Errorf("Mkdir on the filled volume = %v, want ENOSPC", err)
	}
	if err := b.Free(); err != nil {
		t.Fatalf("Free: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o750); err != nil {
		t.Errorf("Mkdir after Free = %v, want success", err)
	}
}

// TestFill_RefillReplacesStaleBallast checks that a ballast left by an earlier
// run is not counted as the volume's free space.
func TestFill_RefillReplacesStaleBallast(t *testing.T) {
	dir := smallVolume(t)
	const headroom = 1 << 20
	if _, err := Fill(dir, headroom, false); err != nil {
		t.Fatalf("first Fill: %v", err)
	}
	b, err := Fill(dir, headroom, false)
	if err != nil {
		t.Fatalf("second Fill: %v", err)
	}
	if free := freeBytes(t, dir); free > headroom+4096 || free < headroom-4096 {
		t.Errorf("free after a second Fill = %d, want about %d", free, headroom)
	}
	if err := b.Free(); err != nil {
		t.Fatalf("Free: %v", err)
	}
	if free := freeBytes(t, dir); free < volumeBytes/2 {
		t.Errorf("free after Free = %d, want most of the volume", free)
	}
}

func TestFill_HeadroomAboveFreeSpaceIsNotAnError(t *testing.T) {
	dir := smallVolume(t)
	b, err := Fill(dir, 1<<40, false)
	if err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, BallastName)); !os.IsNotExist(err) {
		t.Errorf("a ballast file exists though the volume was under headroom: %v", err)
	}
	if err := b.Free(); err != nil {
		t.Errorf("Free: %v", err)
	}
}

func TestFill_MissingDirIsAnError(t *testing.T) {
	if _, err := Fill(filepath.Join(t.TempDir(), "nope"), 0, false); err == nil {
		t.Error("Fill of a missing directory succeeded")
	}
}

func TestFree_NilBallast(t *testing.T) {
	var b *Ballast
	if err := b.Free(); err != nil {
		t.Errorf("Free on a nil Ballast = %v", err)
	}
}
