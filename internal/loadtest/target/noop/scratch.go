// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"os"
	"time"
)

const (
	// scratchFile is what a task with scratch_bytes writes, in its working
	// directory.
	scratchFile = "scratch.bin"
	// scratchEvery is how often a task writes a chunk when it logs once or
	// not at all.
	scratchEvery = 200 * time.Millisecond
	// scratchPiece is the most one write call carries.
	scratchPiece = 1 << 20

	exitScratch = 1
)

// loopResult is how a main process's loop ended.
type loopResult int

const (
	loopDone loopResult = iota
	loopBrokenStdout
	loopScratchFailed
)

// loop runs the main process's body: its printing, and its scratch writes when
// the class asks for them.
func (t task) loop() loopResult {
	every := time.Duration(t.class.LogEveryMS) * time.Millisecond
	if t.class.ScratchBytes <= 0 {
		if !t.printFor(defaultLine, every) {
			return loopBrokenStdout
		}
		return loopDone
	}
	return t.scratchFor(every)
}

// scratchFor prints like printFor and writes ScratchBytes to scratch.bin in
// equal chunks, one per print interval (every 200 ms when it prints once), the
// first at once. It prints scratch write failed and reports it on any error:
// a task that cannot write to a full disk fails, as a real one does.
func (t task) scratchFor(logEvery time.Duration) loopResult {
	interval := logEvery
	if interval <= 0 {
		interval = scratchEvery
	}
	start := time.Now()
	deadline := start.Add(t.duration())
	ticks := max(1, int((t.duration()+interval-1)/interval))

	f, err := os.OpenFile(scratchFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return scratchFailed(err)
	}
	if _, err := fmt.Println(defaultLine); err != nil {
		f.Close()
		return loopBrokenStdout
	}
	remaining := t.class.ScratchBytes
	buf := make([]byte, scratchPiece)
	for i := range ticks {
		chunk := remaining / int64(ticks-i)
		remaining -= chunk
		if err := writeChunk(f, buf, chunk); err != nil {
			f.Close()
			return scratchFailed(err)
		}
		if i == ticks-1 {
			break
		}
		time.Sleep(time.Until(start.Add(time.Duration(i+1) * interval)))
		if logEvery > 0 {
			if _, err := fmt.Println(defaultLine); err != nil {
				f.Close()
				return loopBrokenStdout
			}
		}
	}
	if err := f.Close(); err != nil {
		return scratchFailed(err)
	}
	time.Sleep(time.Until(deadline))
	return loopDone
}

// writeChunk writes n bytes to f and syncs it, so a volume that takes a write
// into its cache and refuses it later still fails the task.
func writeChunk(f *os.File, buf []byte, n int64) error {
	for n > 0 {
		w, err := f.Write(buf[:min(int64(len(buf)), n)])
		n -= int64(w)
		if err != nil {
			return err
		}
	}
	return f.Sync()
}

func scratchFailed(err error) loopResult {
	fmt.Fprintf(os.Stderr, "scratch write failed: %v\n", err)
	return loopScratchFailed
}
