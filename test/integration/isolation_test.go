// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration && unix

package integration

// isolation_test.go — the real-OS regression guard for run-as-user task
// isolation (internal/worker/isolation, .../session, .../staging).
//
// # Why this file exists
//
// Every other test of isolation drives a fake Provider
// (internal/worker/isolation/fake.go). Those cover sqi's own policy logic
// thoroughly — refusal of privileged accounts, env-allowlist layering, boot
// capability gating — but a fake CANNOT show what a real OS does with a real
// uid/gid switch: whether the child can actually chdir into its working
// directory, whether an embedded file's permission bits let the target user
// read or execute it, or whether a real symlink-preserving sync tool (rsync)
// interacts safely with a recursive chown. That is not hypothetical — a
// review of commit 0a9a190 found three such defects, none of which any fake
// could have caught:
//
//   - C2 (traversal): <dataDir> is created 0700 and owned by the daemon
//     (root); isolation.SecureWorkDir chowns only the leaf <sessionID>
//     directory. Go sets process credentials BEFORE chdir, so the child needs
//     +x on every ancestor of cmd.Dir — which it does not have. Every isolated
//     task is expected to fail at Start with EACCES. Staging has the same
//     shape: staging.Stager.StageIn never chowns the <scratchBase>/<jobID>
//     intermediate directory it creates as a side effect of MkdirAll.
//   - C3 (embedded files): session.writeEmbeddedFile writes files 0640 (or
//     0750 when Runnable), owned by root with root's primary group. The
//     target user is neither owner nor group member and there are no "other"
//     bits, so it can read neither a data file nor execute a runnable one.
//   - C1 (symlink chown): isolation.ChownRecursive uses os.Chown, which
//     DEREFERENCES symlinks, over staged content that `rsync -a` preserves as
//     symlinks. A symlink landing in scratch whose target lives outside
//     scratch has that OUTSIDE target's ownership silently changed.
//
// TestIsolation_ProcessRunsAsTargetUID, ..._RunnableEmbeddedFileIsExecutable,
// ..._NonRunnableEmbeddedFileIsReadable, and
// ..._StagedSymlinkDoesNotChownTargetOutsideScratch are those bugs' guards —
// they are EXPECTED TO FAIL on the code as it stands. That failure is the
// point: it proves the defects are real (rather than reasoned about) and
// gives the subsequent fix a red -> green target, exactly as
// TestLDAP_NestedGroupExpansionFallsBackOnNonAD did for the OpenLDAP
// nested-group bug that motivated make test-ldap in the first place.
//
// # What it runs against
//
// Two real unprivileged accounts (render-a uid 1001, render-b uid 1002)
// sharing a supplementary group (rendershare gid 3000), created by
// test/integration/isolation/Dockerfile. This file's tests run as root
// (requireRoot skips otherwise) and drive the real
// internal/worker/isolation.Provider, internal/worker/session.Manager, and
// internal/worker/staging.Stager — the same production code the executor
// calls — never a fake.
//
// Several tests deliberately run their probe command with cmd.Dir set to a
// neutral, non-session directory ("/") rather than a session's WorkDir. This
// is not a workaround for C2: it isolates the concern each test is actually
// about (supplementary groups, env-passthrough policy, process-group
// survival) from the traversal defect that TestIsolation_ProcessRunsAsTargetUID
// already demonstrates on its own. Tests about the session working directory
// itself (uid, embedded files) use the real sess.WorkDir and therefore inherit
// C2's blast radius honestly — see the report this suite's task produced for
// how that plays out.
//
// Skips cleanly (via requireRoot) when not running as root or not on a POSIX
// OS, so it never blocks a developer running `go test` directly on a
// non-container machine. make test-isolation is what guarantees it actually
// runs as root against real accounts — see the Makefile target's own comment.

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/envutil"
	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/session"
	"github.com/uberware/sqi/internal/worker/staging"
)

// ── Fixture accounts ──────────────────────────────────────────────────────────

// The uids/gid below must match test/integration/isolation/Dockerfile exactly.
const (
	renderAUser    = "render-a"
	renderAUID     = 1001
	renderBUser    = "render-b"
	renderBUID     = 1002
	renderShareGID = 3000
)

// ── Setup helpers ─────────────────────────────────────────────────────────────

// requireRoot skips the test unless running as root. Real setuid/setgid
// transitions require root; make test-isolation guarantees that inside its
// container, but a bare `go test -tags integration` on a developer machine
// must degrade to a clean skip rather than a confusing failure. The file-level
// `unix` build constraint already keeps this off Windows.
func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("isolation integration tests require running as root (e.g. inside the isolation container); see make test-isolation")
	}
}

// testLogger returns a logger that discards everything below Error, matching
// the convention in internal/worker/session's own tests.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 100}))
}

// isolatedDataDir returns a fresh temporary directory usable as an isolated
// session's dataDir — plain t.TempDir() is not enough here.
//
// Go's testing package creates t.TempDir() itself at a hardcoded,
// non-configurable 0700, and nests it under a per-test scratch root of its
// OWN making (e.g. /tmp/TestFoo<random>/001) that is ALSO 0700 — both
// root-owned in this container, since the suite itself runs as root.
//
// Production code deliberately does not widen either of these anymore (see
// workerconfig.LoadOrCreateWorkerID's doc and session.Manager's
// prepareSessionsDir): the run-as-user split creates its session root
// traversable FROM BIRTH at a location the operator controls
// (/var/lib/sqi-worker-sessions by default, whose ancestors are already
// 0755) rather than ever chmod'ing an existing directory to get there. This
// fixture stands in for that operator-provisioned location, so it must
// arrange the SAME starting condition itself: both the directory this
// helper returns (which callers join with "sessions" to build the Manager's
// session root, mirroring cmd/sqi-worker's own fallback shape) and its
// go-test-internal parent — an ancestor entirely outside any path production
// code is ever given a handle to, so no production fix could reach it
// regardless of design — must already be traversable before the code under
// test runs. Mirrors the same execute-only-for-others permission style
// isolation.ValidateTraversable uses in production.
func isolatedDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o711); err != nil {
		t.Fatalf("make t.TempDir() traversable (test-fixture-only accommodation — see isolatedDataDir's doc): %v", err)
	}
	if err := os.Chmod(filepath.Dir(dir), 0o711); err != nil {
		t.Fatalf("make t.TempDir() parent traversable (test-fixture-only accommodation — see isolatedDataDir's doc): %v", err)
	}
	return dir
}

// newRealProvider returns the real POSIX isolation.Provider — never
// isolation.NewFake, which cannot see any of the three defects this suite
// demonstrates.
func newRealProvider(t *testing.T) isolation.Provider {
	t.Helper()
	p, err := isolation.NewProvider(isolation.Config{Logger: testLogger()})
	if err != nil {
		t.Fatalf("isolation.NewProvider: %v", err)
	}
	return p
}

// newIsolatedSession creates a real session.Session for an assignment whose
// Isolation names user/group, using the real Provider and the real
// session.Manager — exactly the code path internal/worker/executor drives.
// dataDir is caller-supplied (rather than always a fresh t.TempDir()) so
// TestIsolation_SessionDirIsPrivateToTargetUser can create two sessions under
// the same daemon data directory, matching how a real worker would host
// concurrent sessions for two different queues.
func newIsolatedSession(t *testing.T, dataDir, user, group string, isolationCfg workerconfig.IsolationConfig) *session.Session {
	t.Helper()
	provider := newRealProvider(t)
	mgr := session.NewManager(filepath.Join(dataDir, "sessions"), false, provider, isolationCfg, testLogger())
	msg := &protocol.AssignMsg{
		JobID:     "job-" + t.Name(),
		TaskID:    "task-1",
		AttemptID: "attempt-1",
		Isolation: &protocol.IsolationSpec{User: user, Group: group},
	}
	sess, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("create isolated session for %q: %v", user, err)
	}
	t.Cleanup(func() { mgr.Cleanup(context.Background(), sess, false) })
	return sess
}

// newUnisolatedSession creates a real session.Session for an assignment that
// carries no Isolation at all — the pre-isolation default every component
// must leave byte-for-byte unchanged. WithSessionRootMode(0o750) mirrors
// cmd/sqi-worker's effectiveSessionRoot non-root DataDir-fallback branch (the
// shape this dataDir/"sessions" construction stands in for — see
// isolatedDataDir's own doc): a deployment with no run_as_user configured
// anywhere gains nothing from the wider, isolation-only 0711, so the session
// root must come out at the pre-split 0750 exactly as it did before this
// branch's split existed.
func newUnisolatedSession(t *testing.T, dataDir string) *session.Session {
	t.Helper()
	provider := newRealProvider(t)
	mgr := session.NewManager(
		filepath.Join(dataDir, "sessions"), false, provider, workerconfig.IsolationConfig{}, testLogger(),
		session.WithSessionRootMode(0o750),
	)
	msg := &protocol.AssignMsg{JobID: "job-" + t.Name(), TaskID: "task-1", AttemptID: "attempt-1"}
	sess, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("create unisolated session: %v", err)
	}
	t.Cleanup(func() { mgr.Cleanup(context.Background(), sess, false) })
	return sess
}

// configureProcessGroupForTest mirrors internal/worker/executor's
// configureProcessGroup (kill_unix.go) exactly: Setpgid must be set BEFORE
// isolation.Apply so Apply's contract ("MUST preserve any SysProcAttr fields
// the caller already set") is exercised the same way production does.
func configureProcessGroupForTest(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// runInSession launches action inside sess exactly as
// internal/worker/executor.execProcess does: cmd.Dir = sess.WorkDir, Setpgid
// configured first, isolation.Apply second, environment built from
// sess.BaseEnv() via envutil.BuildFromBase. It returns the combined
// stdout+stderr and the run error (nil on a clean zero-exit).
func runInSession(sess *session.Session, action *protocol.Action, envOverrides map[string]string) (string, error) {
	cmd := exec.CommandContext(context.Background(), action.Command, action.Args...)
	cmd.Dir = sess.WorkDir
	configureProcessGroupForTest(cmd)
	if err := isolation.Apply(cmd, sess.Credential()); err != nil {
		return "", fmt.Errorf("apply run-as-user credential: %w", err)
	}
	cmd.Env = envutil.BuildFromBase(sess.BaseEnv(), envOverrides, sess.EnvUnset())

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// statMode returns path's permission bits.
func statMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	return info.Mode().Perm()
}

// statUID returns path's owner uid.
func statUID(t *testing.T, path string) uint32 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat %q: cannot read raw uid/gid (not a *syscall.Stat_t)", path)
	}
	return st.Uid
}

// lstatUID returns path's owner uid WITHOUT following a trailing symlink —
// the distinction TestIsolation_StagedSymlinkDoesNotChownTargetOutsideScratch
// exists to exploit: os.Chown follows symlinks, os.Lchown does not.
func lstatUID(t *testing.T, path string) uint32 {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %q: %v", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("lstat %q: cannot read raw uid/gid (not a *syscall.Stat_t)", path)
	}
	return st.Uid
}

// canUserReadFile reports whether user can read path, using the real
// Provider + isolation.Apply (never a shell su/sudo, which may not be
// configured for non-interactive use in a minimal container). cmd.Dir is "/"
// — deliberately NOT a session working directory — so this checks only the
// permission bits on path's own ancestor chain, not cmd.Dir's, keeping this
// helper independent of the C2 traversal defect it might otherwise trip over
// for unrelated reasons.
func canUserReadFile(t *testing.T, user, path string) bool {
	t.Helper()
	provider := newRealProvider(t)
	cred, err := provider.Resolve(context.Background(), isolation.Spec{User: user})
	if err != nil {
		t.Fatalf("resolve %s: %v", user, err)
	}
	cmd := exec.CommandContext(context.Background(), "cat", path)
	cmd.Dir = "/"
	if err := isolation.Apply(cmd, cred); err != nil {
		t.Fatalf("apply credential for %s: %v", user, err)
	}
	return cmd.Run() == nil
}

// anyProcessInGroup reports whether any process on the system still belongs
// to POSIX process group pgid, by scanning /proc. Used to confirm a SIGKILL
// to a process group actually reaped every descendant, not just the direct
// child. Linux-only (matches the container this test targets); skips rather
// than fails if /proc is unavailable, since that means the check itself
// cannot run, not that reaping failed.
func anyProcessInGroup(t *testing.T, pgid int) bool {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Skipf("cannot read /proc to verify process-group reaping: %v", err)
	}
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue // not a pid directory
		}
		data, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue // process exited between ReadDir and ReadFile
		}
		// proc(5): "pid (comm) state ppid pgrp ...". comm may itself contain
		// spaces or parentheses, so split on the LAST ")" rather than the
		// first whitespace run.
		rest := string(data)
		if i := strings.LastIndex(rest, ")"); i >= 0 {
			rest = rest[i+1:]
		}
		fields := strings.Fields(rest)
		if len(fields) < 3 {
			continue
		}
		if pgrp, err := strconv.Atoi(fields[2]); err == nil && pgrp == pgid {
			return true
		}
	}
	return false
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestIsolation_ProcessRunsAsTargetUID is the C2 guard. It launches a task
// exactly as the executor does — cmd.Dir = sess.WorkDir, under render-a's
// resolved credential — and expects `id -u` to report 1001.
//
// EXPECTED TO FAIL on the current code: <dataDir> is 0700 root-owned and
// SecureWorkDir only chowns the session leaf, so the render-a child cannot
// chdir into cmd.Dir at all. cmd.Run returns an EACCES "permission denied"
// error before the task ever produces output.
func TestIsolation_ProcessRunsAsTargetUID(t *testing.T) {
	requireRoot(t)
	sess := newIsolatedSession(t, isolatedDataDir(t), renderAUser, "", workerconfig.IsolationConfig{})

	out, err := runInSession(sess, &protocol.Action{Command: "id", Args: []string{"-u"}}, nil)
	if err != nil {
		t.Fatalf("run `id -u` as %s inside the session working directory: %v (output: %q)", renderAUser, err, out)
	}
	if got := strings.TrimSpace(out); got != strconv.Itoa(renderAUID) {
		t.Errorf("task ran as uid %q, want %d (%s)", got, renderAUID, renderAUser)
	}
}

// TestIsolation_RunnableEmbeddedFileIsExecutable is the C3 guard for a
// runnable embedded file — the canonical OpenJD shape is
// `embeddedFiles: [{name: run, runnable: true}]` plus
// `onRun.command: "{{Task.File.run}}"`, which resolves to exactly the path
// this test executes.
//
// EXPECTED TO FAIL on the current code, though not cleanly: session.go writes
// the file 0750 root-owned, which alone would produce "permission denied"
// executing it as render-a — but C2 (see
// TestIsolation_ProcessRunsAsTargetUID) already blocks the chdir into
// sess.WorkDir before the exec attempt is ever reached, so today this test
// fails with the SAME traversal EACCES, not a distinct one. C3's own defect
// only becomes independently observable once C2 is fixed.
func TestIsolation_RunnableEmbeddedFileIsExecutable(t *testing.T) {
	requireRoot(t)
	sess := newIsolatedSession(t, isolatedDataDir(t), renderAUser, "", workerconfig.IsolationConfig{})

	files := []protocol.EmbeddedFile{{
		Name:     "run",
		Data:     "#!/bin/sh\necho embedded-file-executed\n",
		Runnable: true,
	}}
	if err := sess.WriteEmbeddedFiles(files); err != nil {
		t.Fatalf("write embedded files: %v", err)
	}

	runPath := filepath.Join(sess.WorkDir, "run")
	out, err := runInSession(sess, &protocol.Action{Command: runPath}, nil)
	if err != nil {
		t.Fatalf("execute runnable embedded file as %s: %v (output: %q)", renderAUser, err, out)
	}
	if !strings.Contains(out, "embedded-file-executed") {
		t.Errorf("embedded file output = %q, want it to contain %q", out, "embedded-file-executed")
	}
}

// TestIsolation_NonRunnableEmbeddedFileIsReadable is the C3 guard for a
// non-runnable (data) embedded file.
//
// EXPECTED TO FAIL on the current code, for the same compounded reason as
// TestIsolation_RunnableEmbeddedFileIsExecutable: C2 blocks the chdir before
// the 0640 root-owned file's own permission bits can matter.
func TestIsolation_NonRunnableEmbeddedFileIsReadable(t *testing.T) {
	requireRoot(t)
	sess := newIsolatedSession(t, isolatedDataDir(t), renderAUser, "", workerconfig.IsolationConfig{})

	const marker = "embedded-data-file-contents-xyz"
	files := []protocol.EmbeddedFile{{Name: "data", Data: marker}}
	if err := sess.WriteEmbeddedFiles(files); err != nil {
		t.Fatalf("write embedded files: %v", err)
	}

	dataPath := filepath.Join(sess.WorkDir, "data")
	out, err := runInSession(sess, &protocol.Action{Command: "cat", Args: []string{dataPath}}, nil)
	if err != nil {
		t.Fatalf("read non-runnable embedded file as %s: %v (output: %q)", renderAUser, err, out)
	}
	if !strings.Contains(out, marker) {
		t.Errorf("embedded file contents = %q, want it to contain %q", out, marker)
	}
}

// TestIsolation_StagedSymlinkDoesNotChownTargetOutsideScratch is the C1
// guard. It drives staging.Stager.StageIn directly (the same production code
// internal/worker/executor calls before running a task) with an `rsync -a`
// sync command against an IN entry that is itself a symlink pointing OUTSIDE
// the scratch tree, at a throwaway file this test creates and owns as root.
// This never touches a real system file (e.g. /etc/shadow) — only a fixture
// this test creates inside its own temp directories.
//
// EXPECTED TO FAIL on the current code: isolation.ChownRecursive walks the
// scratch directory with os.Chown, which DEREFERENCES the symlink and
// silently reassigns ownership of the OUTSIDE target to the isolated user,
// rather than leaving it root-owned.
func TestIsolation_StagedSymlinkDoesNotChownTargetOutsideScratch(t *testing.T) {
	requireRoot(t)

	provider := newRealProvider(t)
	cred, err := provider.Resolve(context.Background(), isolation.Spec{User: renderAUser})
	if err != nil {
		t.Fatalf("resolve %s: %v", renderAUser, err)
	}

	// A throwaway root-owned file living OUTSIDE any scratch directory this
	// test creates. Deliberately never a real system path.
	targetDir := t.TempDir()
	targetFile := filepath.Join(targetDir, "root-owned-target")
	if err := os.WriteFile(targetFile, []byte("root-owned-secret\n"), 0o600); err != nil {
		t.Fatalf("create throwaway root-owned target: %v", err)
	}
	if wantUID := lstatUID(t, targetFile); wantUID != 0 {
		t.Fatalf("test fixture invariant violated: throwaway target is not root-owned (uid %d)", wantUID)
	}

	// A symlink to that target, staged in as a single IN entry. `rsync -a`
	// (its -l) preserves this as a symlink rather than following it.
	inDir := t.TempDir()
	symlinkPath := filepath.Join(inDir, "link-to-target")
	if err := os.Symlink(targetFile, symlinkPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	// isolatedDataDir (not a bare t.TempDir()) for the scratch base
	// specifically: StageIn now validates the scratch base's own ancestors
	// per-assignment (Finding 4's staging.Stager.StageIn check) whenever cred
	// is non-nil, exactly as it validates in production — a bare t.TempDir()
	// sits under Go testing's own per-test MkdirTemp parent, hardcoded 0700
	// regardless of umask, which is a test-fixture artifact production never
	// hits (a real scratch base is an operator-provisioned location, not
	// nested under a throwaway inherently-0700 ancestor).
	stager := staging.New(isolatedDataDir(t), "rsync -a {src} {dest}", false, testLogger())
	if !stager.Configured() {
		t.Fatal("stager not configured")
	}

	entries := []protocol.StageEntry{{Path: symlinkPath, Direction: "IN", ObjectType: "FILE"}}
	_, scratchDir, err := stager.StageIn(context.Background(), "job-symlink", "attempt-1", entries, cred)
	if scratchDir != "" {
		defer stager.Cleanup(scratchDir)
	}
	if err != nil {
		t.Fatalf("StageIn: %v", err)
	}

	if gotUID := lstatUID(t, targetFile); gotUID != 0 {
		t.Errorf("root-owned target OUTSIDE scratch changed owner to uid %d after staging a symlink to it; "+
			"isolation.ChownRecursive must use os.Lchown, not os.Chown, to avoid dereferencing symlinks", gotUID)
	}
}

// TestIsolation_SupplementaryGroupsPreserved checks that render-a's
// rendershare (gid 3000) supplementary membership survives
// isolation.Provider.Resolve + isolation.Apply. cmd.Dir is "/" rather than a
// session's WorkDir: this test is about Credential.Groups, a concern
// orthogonal to the C2 traversal defect TestIsolation_ProcessRunsAsTargetUID
// already demonstrates on its own.
func TestIsolation_SupplementaryGroupsPreserved(t *testing.T) {
	requireRoot(t)
	provider := newRealProvider(t)
	cred, err := provider.Resolve(context.Background(), isolation.Spec{User: renderAUser})
	if err != nil {
		t.Fatalf("resolve %s: %v", renderAUser, err)
	}

	cmd := exec.CommandContext(context.Background(), "id", "-G")
	cmd.Dir = "/"
	if err := isolation.Apply(cmd, cred); err != nil {
		t.Fatalf("apply credential: %v", err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run `id -G` as %s: %v (output: %q)", renderAUser, err, out)
	}
	if !strings.Contains(string(out), strconv.Itoa(renderShareGID)) {
		t.Errorf("`id -G` output = %q, want it to contain %d (rendershare)", out, renderShareGID)
	}
}

// TestIsolation_SessionDirIsPrivateToTargetUser checks that a session working
// directory is 0700 and owned by the target user, and that a SECOND
// unprivileged user (render-b) cannot read into a session created for the
// FIRST (render-a) — proving isolation holds between two different queues'
// tasks, not just between the daemon and one task.
func TestIsolation_SessionDirIsPrivateToTargetUser(t *testing.T) {
	requireRoot(t)
	dataDir := isolatedDataDir(t)
	sessA := newIsolatedSession(t, dataDir, renderAUser, "", workerconfig.IsolationConfig{})

	if got := statMode(t, sessA.WorkDir); got != 0o700 {
		t.Errorf("session dir mode = %o, want 0700", got)
	}
	if got := statUID(t, sessA.WorkDir); got != renderAUID {
		t.Errorf("session dir owner uid = %d, want %d (%s)", got, renderAUID, renderAUser)
	}

	probe := filepath.Join(sessA.WorkDir, "probe")
	if err := os.WriteFile(probe, []byte("render-a-secret"), 0o600); err != nil {
		t.Fatalf("write probe file: %v", err)
	}

	if canUserReadFile(t, renderBUser, probe) {
		t.Errorf("%s could read a file inside %s's session directory %q", renderBUser, renderAUser, sessA.WorkDir)
	}
}

// TestIsolation_DaemonSecretAbsentAllowlistedLicensePresent is the
// env-passthrough policy guard against a REAL uid switch: the daemon's
// AWS_ACCESS_KEY_ID must never reach an isolated task, while an
// operator-allowlisted licensing variable must. cmd.Dir is "/" rather than a
// session WorkDir, for the same reason as
// TestIsolation_SupplementaryGroupsPreserved — this test is about
// session.BaseEnv()'s filtering, not about C2.
func TestIsolation_DaemonSecretAbsentAllowlistedLicensePresent(t *testing.T) {
	requireRoot(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "must-not-leak")
	t.Setenv("foundry_LICENSE", "4101@licsrv")

	isolationCfg := workerconfig.IsolationConfig{EnvPassthrough: []string{"foundry_*"}}
	sess := newIsolatedSession(t, isolatedDataDir(t), renderAUser, "", isolationCfg)

	cmd := exec.CommandContext(context.Background(), "env")
	cmd.Dir = "/"
	if err := isolation.Apply(cmd, sess.Credential()); err != nil {
		t.Fatalf("apply credential: %v", err)
	}
	cmd.Env = envutil.BuildFromBase(sess.BaseEnv(), nil, sess.EnvUnset())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run `env` as %s: %v (output: %q)", renderAUser, err, out)
	}

	if strings.Contains(string(out), "must-not-leak") {
		t.Error("daemon secret AWS_ACCESS_KEY_ID leaked into the isolated task environment")
	}
	if !strings.Contains(string(out), "4101@licsrv") {
		t.Error("allowlisted foundry_LICENSE variable was not inherited by the isolated task")
	}
}

// TestIsolation_ProcessGroupKillReapsPrivilegeDroppedGrandchild proves
// isolation.Apply preserves Setpgid (configureProcessGroupForTest, applied
// FIRST, mirroring internal/worker/executor.execProcess): a privilege-dropped
// child that itself spawns a grandchild must have BOTH reaped by a SIGKILL to
// the process group. cmd.Dir is "/" — this test is about SysProcAttr
// survival, not about C2.
func TestIsolation_ProcessGroupKillReapsPrivilegeDroppedGrandchild(t *testing.T) {
	requireRoot(t)
	provider := newRealProvider(t)
	cred, err := provider.Resolve(context.Background(), isolation.Spec{User: renderAUser})
	if err != nil {
		t.Fatalf("resolve %s: %v", renderAUser, err)
	}

	cmd := exec.CommandContext(context.Background(), "sh", "-c", "sleep 300 & sleep 300 & wait")
	cmd.Dir = "/"
	configureProcessGroupForTest(cmd)
	if err := isolation.Apply(cmd, cred); err != nil {
		t.Fatalf("apply credential: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start process-group fixture: %v", err)
	}
	pid := cmd.Process.Pid

	// Give the shell time to fork both sleep children before killing the group.
	time.Sleep(300 * time.Millisecond)

	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL process group -%d: %v", pid, err)
	}
	_ = cmd.Wait() //nolint:errcheck // killed on purpose; only reaping is asserted below

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !anyProcessInGroup(t, pid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("a descendant of pid %d survived SIGKILL to its process group — Setpgid may have been clobbered by isolation.Apply", pid)
}

// TestIsolation_NoRunAsUserBehaviorUnchanged is the standing regression
// invariant every Phase 3 component carries: with no run_as_user configured
// anywhere, behavior must be byte-for-byte identical to before isolation
// existed — full daemon environment inherited, workdir mode 0750, no
// credential resolved. It also guards the run-as-user split itself (Fix 1):
// a directory that plays the role of the worker's data_dir (holding only
// worker.id in production) must come out of session creation at exactly the
// mode it went in at — session.Manager must never widen it, regardless of
// whether isolation is configured — since data_dir now holds nothing but the
// worker's persistent, server-correlated identity and must stay private.
func TestIsolation_NoRunAsUserBehaviorUnchanged(t *testing.T) {
	requireRoot(t)
	t.Setenv("ARBITRARY_DAEMON_VAR", "inherited-value")

	// A real data_dir (as workerconfig.LoadOrCreateWorkerID actually
	// produces it) is 0700; t.TempDir() itself is created via
	// os.Mkdir(dir, 0777) internally by the testing package (masked by
	// umask, NOT 0700), so this test sets up the exact precondition
	// LoadOrCreateWorkerID would have left behind rather than relying on
	// t.TempDir()'s own incidental default.
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0o700); err != nil {
		t.Fatalf("set up data_dir precondition: %v", err)
	}

	sess := newUnisolatedSession(t, dataDir)

	if sess.Credential() != nil {
		t.Fatal("no credential should be resolved when the assignment carries no isolation")
	}
	if got := statMode(t, sess.WorkDir); got != 0o750 {
		t.Errorf("workdir mode = %o, want 0750 (unchanged, no-isolation default)", got)
	}
	if got := statMode(t, dataDir); got != 0o700 {
		t.Errorf("data_dir mode = %o after session creation, want 0700 unchanged — "+
			"data_dir must NEVER be widened for session traversal (see workerconfig.LoadOrCreateWorkerID's doc)", got)
	}
	sessionRoot := filepath.Join(dataDir, "sessions")
	if got := statMode(t, sessionRoot); got != 0o750 {
		t.Errorf("session root mode = %o, want 0750 (pre-Task-8 this directory was created 0750 as a "+
			"byproduct of a single MkdirAll call; a deployment with no run_as_user configured anywhere "+
			"gains nothing from the wider, isolation-only 0711)", got)
	}

	out, err := runInSession(sess, &protocol.Action{Command: "env"}, nil)
	if err != nil {
		t.Fatalf("run task with no isolation configured: %v (output: %q)", err, out)
	}
	if !strings.Contains(out, "ARBITRARY_DAEMON_VAR=inherited-value") {
		t.Error("full daemon environment must still be inherited when isolation is off")
	}
}

// TestIsolation_CredentialClosedOnEnterEnvironmentsFailure is the real-root
// counterpart to internal/worker/session's own (unit-level, fake-credential)
// TestCredentialClose_ClosedExactlyOnceOnNormalCleanup and
// TestCredentialClose_NeverCalledWhenCredentialNeverObtained. A THIRD sibling
// test used to live there — TestCredentialClose_ClosedExactlyOnceOnEnterEnvironmentsFailure
// — covering Manager.Create's OnEnter-failure teardown path: a credential IS
// obtained (the account resolves), but a later OnEnter action fails, so
// Create tears everything down itself, including closing the credential,
// before returning the error.
//
// That test could never pass unprivileged, on any POSIX OS, sandboxed or
// not: unlike this package's other tests (which fake applyCredential and so
// never actually exec anything under a switched identity), it drove the REAL
// isolation.Apply for its engineered OnEnter failure ("sh -c exit 1").
// exec.Cmd.SysProcAttr.Credential always calls setgroups(2), which requires
// CAP_SETGID even to set a process's own CURRENT supplementary group list
// unless the caller is already privileged — so the intended "exit 1"
// failure was always preempted by an earlier, unintended EPERM out of
// isolation.Apply itself when run unprivileged. Real root — guaranteed here
// by requireRoot and make test-isolation's container — has that privilege,
// so the OnEnter action fails for the reason the test actually engineers
// (`exit 1`, run as render-a) rather than for lack of permission to even
// attempt the identity switch.
//
// session.closeCredentialFn — the call-counting seam the original unit test
// swapped to prove Close was invoked exactly once — is unexported and
// unreachable from this package. This test proves the same teardown branch
// ran by its externally observable consequence instead: Manager.Create's
// OnEnter-failure path calls closeCredential(cred) and then unconditionally
// os.RemoveAll(workDir) in that one branch (see
// internal/worker/session/session.go, Create's env-setup-failure branch),
// never in a loop and never on any other return path — so a session
// directory that is gone after a genuinely failing OnEnter proves that
// branch, and the single credential-close call it performs, executed to
// completion.
func TestIsolation_CredentialClosedOnEnterEnvironmentsFailure(t *testing.T) {
	requireRoot(t)
	dataDir := isolatedDataDir(t)
	provider := newRealProvider(t)
	sessionRoot := filepath.Join(dataDir, "sessions")
	mgr := session.NewManager(sessionRoot, false, provider, workerconfig.IsolationConfig{}, testLogger())

	msg := &protocol.AssignMsg{
		JobID:     "job-" + t.Name(),
		TaskID:    "task-1",
		AttemptID: "attempt-1",
		Isolation: &protocol.IsolationSpec{User: renderAUser},
		Environments: []protocol.AssignEnvironment{
			{
				Name: "bad-env",
				OnEnter: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "exit 1"},
				},
			},
		},
	}

	sess, err := mgr.Create(context.Background(), msg)
	if sess != nil {
		// Create's contract is "session and nil error, or nil and non-nil
		// error — never both" (see its own doc); clean up defensively so a
		// contract violation here doesn't also leak a session directory.
		mgr.Cleanup(context.Background(), sess, true)
		t.Fatalf("Create must return nil session alongside a non-nil error; got %+v", sess)
	}
	if err == nil {
		t.Fatal("expected Create to fail when OnEnter fails")
	}

	// Create generates the session ID internally, so its workDir path is not
	// known to this test directly — but this test's own sessionRoot holds
	// nothing else, so confirming it is empty proves the ONE workDir Create
	// created for this failed attempt was removed by the OnEnter-failure
	// teardown branch.
	entries, rdErr := os.ReadDir(sessionRoot)
	if rdErr != nil {
		t.Fatalf("read session root %q: %v", sessionRoot, rdErr)
	}
	if len(entries) != 0 {
		t.Errorf("session root has %d leftover entries after a failed Create, want 0 — "+
			"the OnEnter-failure teardown branch (which closes the credential before removing "+
			"the working directory) must run to completion", len(entries))
	}
}
