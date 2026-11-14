// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux

package diskfill

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// writeChunk is the size of one plain write when the ballast cannot be
// preallocated.
const writeChunk = 1 << 20

// maxInodeFiles bounds the empty files Fill creates.
const maxInodeFiles = 1_000_000

// shardSize is how many inode files go into one directory.
const shardSize = 1000

// Fill writes a ballast file under dir (BallastName) sized to the volume's
// free space (statfs Bavail*Bsize) minus headroom, preallocated when the
// file system allows it and written otherwise. With inodes it then creates
// empty files under dir/InodesName until the volume refuses one, at most
// 1,000,000. A volume already at or under headroom gets no ballast file and
// is not an error. A ballast left in dir by an earlier Fill is replaced.
//
// When Fill fails it removes what it wrote.
func Fill(dir string, headroom int64, inodes bool) (*Ballast, error) {
	if err := removeFilled(dir); err != nil {
		return nil, err
	}
	b := &Ballast{dir: dir}
	if err := fill(dir, headroom, inodes); err != nil {
		return nil, errors.Join(err, b.Free())
	}
	return b, nil
}

func fill(dir string, headroom int64, inodes bool) error {
	if err := fillBytes(dir, headroom); err != nil {
		return err
	}
	if !inodes {
		return nil
	}
	return fillInodes(dir)
}

// freeSpace is the bytes an unprivileged writer can still write to dir's
// volume, and the volume's block size.
func freeSpace(dir string) (free, block int64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, 0, fmt.Errorf("diskfill: statfs %s: %w", dir, err)
	}
	block = int64(st.Bsize)                     //nolint:unconvert // Bsize is not an int64 on every unix
	return int64(st.Bavail) * block, block, nil //nolint:gosec // G115: a free block count fits
}

// fillBytes writes the ballast file: as much as leaves headroom free, rounded
// down to a whole block.
func fillBytes(dir string, headroom int64) error {
	free, block, err := freeSpace(dir)
	if err != nil {
		return err
	}
	size := free - headroom
	if block > 0 {
		size -= size % block
	}
	if size <= 0 {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(dir, BallastName), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("diskfill: create the ballast: %w", err)
	}
	werr := allocate(f, size)
	return errors.Join(werr, f.Close())
}

// allocate makes f size bytes, preallocated when it can be and written
// otherwise. A write the volume refuses ends the ballast: the volume is full.
func allocate(f *os.File, size int64) error {
	if err := unix.Fallocate(int(f.Fd()), 0, 0, size); err == nil {
		return nil
	}
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("diskfill: reset the ballast: %w", err)
	}
	buf := make([]byte, writeChunk)
	for written := int64(0); written < size; {
		n := min(int64(len(buf)), size-written)
		w, err := f.Write(buf[:n])
		written += int64(w)
		switch {
		case errors.Is(err, syscall.ENOSPC):
			return nil
		case err != nil:
			return fmt.Errorf("diskfill: write the ballast: %w", err)
		}
	}
	return nil
}

// fillInodes creates empty files under dir/InodesName until the volume
// refuses one for want of space or inodes, or maxInodeFiles exist.
func fillInodes(dir string) error {
	root := filepath.Join(dir, InodesName)
	if err := os.Mkdir(root, 0o750); err != nil {
		if exhausted(err) {
			return nil
		}
		return fmt.Errorf("diskfill: create %s: %w", InodesName, err)
	}
	for n := range maxInodeFiles {
		shard := filepath.Join(root, strconv.Itoa(n/shardSize))
		if n%shardSize == 0 {
			if err := os.Mkdir(shard, 0o750); err != nil {
				return stopOnExhausted(err)
			}
		}
		f, err := os.OpenFile(filepath.Join(shard, strconv.Itoa(n)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return stopOnExhausted(err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("diskfill: close an inode file: %w", err)
		}
	}
	return nil
}

// stopOnExhausted is nil when err says the volume is out of space or inodes,
// which is what fillInodes is after.
func stopOnExhausted(err error) error {
	if exhausted(err) {
		return nil
	}
	return fmt.Errorf("diskfill: create inode files: %w", err)
}

func exhausted(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}
