// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package executor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// spawnDetachedGrandchild starts, via "cmd /c start /b <script.bat>", a
// background batch file that pings for pingSeconds and then writes marker.
// The returned *exec.Cmd is the immediate ("cmd /c start /b ...") process;
// by the time it exits, the batch file it launched is already a distinct,
// running process not waited on by it — an orphan-in-waiting the moment its
// starter exits, exactly like the case "taskkill /F /T" cannot reach.
//
// The batch file (not an inline "cmd /c \"...\" & exit" one-liner) is
// deliberate: passed through Go's os/exec on Windows, a script argument
// containing nested double quotes is re-escaped by the standard Windows
// argv-quoting convention (CommandLineToArgvW-style backslash-escaping) before
// cmd.exe re-parses it — but cmd.exe's own command-line tail parsing does not
// follow that convention, so the nested quotes arrive corrupted and the
// intended "start /b ... & exit" chain silently never launches the
// background process at all. That failure is invisible to a test that only
// asserts the marker is ABSENT (an unlaunched process also never writes it),
// which is why it surfaced only once verified independently: a probe of the
// literal brief script showed the marker file was never created even without
// any job-object code in the loop. Writing the script to a .bat file and
// passing its path as a single, unquoted argument (TempDir paths carry no
// spaces) sidesteps the quoting mismatch entirely.
func spawnDetachedGrandchild(t *testing.T, pingSeconds int) (cmd *exec.Cmd, marker string) {
	t.Helper()
	dir := t.TempDir()
	marker = filepath.Join(dir, "marker.txt")
	bat := filepath.Join(dir, "grandchild.bat")
	script := "@echo off\r\nping -n " + strconv.Itoa(pingSeconds) + " 127.0.0.1 > nul\r\necho alive > \"" + marker + "\"\r\n"
	if err := os.WriteFile(bat, []byte(script), 0o700); err != nil {
		t.Fatalf("write grandchild.bat: %v", err)
	}
	cmd = exec.CommandContext(t.Context(), "cmd", "/c", "start", "/b", bat)
	return cmd, marker
}

// TestProcessTree_KillReapsOrphanedGrandchild is the Windows counterpart of
// kill_unix_test.go's process-group reaping test.
//
// "taskkill /F /T" walks the parent-PID tree AT THE MOMENT OF THE CALL, so a
// grandchild whose parent has already exited is orphaned and survives — and
// under run-as-user isolation that survivor runs as the target account,
// outliving its task while holding its license and file handles. A job object
// has no such gap.
func TestProcessTree_KillReapsOrphanedGrandchild(t *testing.T) {
	// The grandchild waits, then writes the marker. The parent starts it
	// detached and exits immediately, so the grandchild is already orphaned
	// by the time the kill runs — the exact case taskkill /T misses.
	cmd, marker := spawnDetachedGrandchild(t, 30)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	tree, err := superviseTree(cmd)
	if err != nil {
		t.Fatalf("superviseTree: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait for parent: %v", err)
	}

	if err := tree.sendKILL(); err != nil {
		t.Fatalf("sendKILL: %v", err)
	}

	// The grandchild would write its marker several seconds from now if it
	// were still alive; give it long enough to prove it is not.
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Error("orphaned grandchild survived the kill and wrote its marker; the job object did not reap it")
	}
}

// TestProcessTree_ReleaseLeavesSurvivorsAlone proves the SUCCESS path matches
// POSIX. internal/worker/staging's TOCTOU analysis (staging.go,
// validateStageOutSource) depends on the fact that nothing kills a task's
// process group on a successful exit, so a still-running background child is
// expected to survive there. release() must clear
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE before closing the handle, or Windows
// would silently diverge from POSIX in a way no test in that package would
// catch.
func TestProcessTree_ReleaseLeavesSurvivorsAlone(t *testing.T) {
	cmd, marker := spawnDetachedGrandchild(t, 3)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	tree, err := superviseTree(cmd)
	if err != nil {
		t.Fatalf("superviseTree: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait for parent: %v", err)
	}

	if err := tree.release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	time.Sleep(5 * time.Second)
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("release() killed a surviving background child; POSIX leaves it running: %v", err)
	}
}

// TestProcessTree_ReleaseClosesHandleEvenIfSetKillOnCloseFails proves release()
// does not leak the job handle when clearing KILL_ON_JOB_CLOSE fails: a
// deliberately invalid handle makes setKillOnClose (SetInformationJobObject)
// fail, and the pre-fix release() returned that error immediately, before
// ever calling CloseHandle or zeroing t.job — leaking the handle value for
// the life of the worker. The fix must close (best-effort) and zero t.job
// regardless, so a second release() call is a true no-op.
func TestProcessTree_ReleaseClosesHandleEvenIfSetKillOnCloseFails(t *testing.T) {
	tree := &processTree{job: windows.Handle(0xDEADBEEF)}

	if err := tree.release(); err == nil {
		t.Fatal("release: expected an error from an invalid job handle, got nil")
	}
	if tree.job != 0 {
		t.Errorf("release: t.job = %#x, want 0 (must be zeroed even when setKillOnClose/CloseHandle fail)", tree.job)
	}

	// Idempotent: once zeroed, a second call must be a no-op, not a second
	// attempt to operate on the stale handle value.
	if err := tree.release(); err != nil {
		t.Errorf("release: second call on an already-released tree returned %v, want nil", err)
	}
}
