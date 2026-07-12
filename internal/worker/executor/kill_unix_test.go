// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package executor

import (
	"bufio"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestSendKILL_TerminatesProcessGroup reproduces the per-task timeout hang:
// a shell forks a `sleep` child that outlives it and inherits the stdout
// pipe. Killing only the direct child (the old proc.Kill behavior) leaves the
// grandchild alive holding the pipe write-end, so a reader never sees EOF and
// killAndWait blocks on <-waitDone until the sleep exits naturally. Terminating
// the whole process group kills the grandchild too, closing the pipe promptly.
func TestSendKILL_TerminatesProcessGroup(t *testing.T) {
	// Background the sleep so it is a distinct child that outlives the shell,
	// then echo a marker and wait. Reading the marker guarantees the `sleep`
	// grandchild already exists before we kill — otherwise the kill could race
	// the fork and trivially "pass".
	cmd := exec.CommandContext(t.Context(), "sh", "-c", "sleep 30 & echo started; wait")
	configureProcessGroup(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	br := bufio.NewReader(stdout)
	line, err := br.ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "started" {
		t.Fatalf("marker read = %q, err = %v; want %q", line, err, "started")
	}

	if err := sendKILL(cmd.Process); err != nil {
		t.Fatalf("sendKILL: %v", err)
	}

	// With the whole group dead, every inherited pipe write-end is closed, so
	// reading to EOF returns almost immediately. If the grandchild survived,
	// this read blocks until `sleep 30` exits and the 5 s guard fails.
	done := make(chan struct{})
	go func() {
		//nolint:errcheck // draining stdout to EOF; a read error also ends the copy and closes done
		io.Copy(io.Discard, br)
		close(done)
	}()

	select {
	case <-done:
		// EOF reached promptly — the process group was terminated.
	case <-time.After(5 * time.Second):
		t.Fatal("stdout did not reach EOF within 5s: grandchild process survived the kill")
	}

	cmd.Wait() //nolint:errcheck // process was killed; Wait only reaps the zombie, a non-nil error is expected
}
