// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration && unix

package integration

// isolation_test.go is the real-OS regression guard for run-as-user task
// isolation (internal/worker/isolation, .../session, .../staging).
//
// # Why this file exists
//
// The other isolation tests drive a fake Provider
// (internal/worker/isolation/fake.go). They cover sqi's own policy logic:
// refusal of privileged accounts, env-allowlist layering, boot capability
// gating. A fake cannot show what a real OS does with a real uid/gid switch:
// whether the child can chdir into its working directory, whether an embedded
// file's permission bits let the target user read or execute it, or whether a
// symlink-preserving sync tool (rsync) interacts safely with a recursive
// chown. This file covers three failure modes of that kind:
//
//   - Traversal: if <dataDir> is 0700 and owned by the daemon (root) and
//     isolation.SecureWorkDir chowns only the leaf <sessionID> directory, the
//     child cannot reach its working directory. Go sets process credentials
//     before chdir, so the child needs +x on every ancestor of cmd.Dir;
//     without it every isolated task fails at Start with EACCES. Staging has
//     the same shape: the <scratchBase>/<jobID> intermediate directory that
//     staging.Stager.StageIn creates as a side effect of MkdirAll must be
//     traversable too.
//   - Embedded files: a file written 0640 (or 0750 when Runnable), owned by
//     root with root's primary group, is unusable by the target user, which
//     is neither owner nor group member and gets no "other" bits, so it can
//     read neither a data file nor execute a runnable one.
//   - Symlink chown: os.Chown dereferences symlinks, and `rsync -a` preserves
//     staged content as symlinks. A recursive chown built on os.Chown changes
//     the ownership of a symlink target that lives outside scratch, so
//     isolation.ChownRecursive uses os.Lchown.
//
// TestIsolation_ProcessRunsAsTargetUID, ..._RunnableEmbeddedFileIsExecutable,
// ..._NonRunnableEmbeddedFileIsReadable, and
// ..._StagedSymlinkDoesNotChownTargetOutsideScratch cover those three.
//
// # What it runs against
//
// Two real unprivileged accounts (render-a uid 1001, render-b uid 1002)
// sharing a supplementary group (rendershare gid 3000), created by
// test/integration/isolation/Dockerfile. The tests run as root (requireRoot
// skips otherwise) and drive the real internal/worker/isolation.Provider,
// internal/worker/session.Manager, and internal/worker/staging.Stager, the
// production code the executor calls.
//
// Several tests run their probe command with cmd.Dir set to a neutral,
// non-session directory ("/") rather than a session's WorkDir. That separates
// the concern each test is about (supplementary groups, env-passthrough
// policy, process-group survival) from working-directory traversal, which
// TestIsolation_ProcessRunsAsTargetUID covers on its own. Tests about the
// session working directory itself (uid, embedded files) use the real
// sess.WorkDir and therefore also fail if traversal breaks.
//
// The tests skip (via requireRoot) when not running as root, so a bare
// `go test` on a non-container machine does not fail. make test-isolation
// runs them as root against real accounts; see the Makefile target's comment.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/envutil"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/proctree"
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
// transitions require root; make test-isolation provides that inside its
// container, and a bare `go test -tags integration` on a developer machine
// skips. The file-level `unix` build constraint keeps this off Windows.
func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("isolation integration tests require running as root (e.g. inside the isolation container); see make test-isolation")
	}
}

// testLogger returns a logger that discards everything, as in
// internal/worker/session's own tests.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 100}))
}

// isolatedDataDir returns a fresh temporary directory usable as an isolated
// session's dataDir. Plain t.TempDir() is not enough.
//
// Go's testing package creates t.TempDir() at a fixed 0700, nested under a
// per-test scratch root (e.g. /tmp/TestFoo<random>/001) that is also 0700.
// Both are root-owned here, since the suite runs as root.
//
// Production code does not widen either of these (see
// workerconfig.LoadOrCreateWorkerID's doc and session.Manager's
// prepareSessionsDir): the run-as-user split creates its session root
// traversable from birth at a location the operator controls
// (/var/lib/sqi-worker-sessions by default, whose ancestors are already
// 0755) and never chmods an existing directory to get there. This fixture
// stands in for that location, so it sets up the same starting condition:
// both the directory it returns (which callers join with "sessions" to build
// the Manager's session root, as cmd/sqi-worker's fallback does) and its
// go-test-internal parent, which no production code is given a handle to,
// must be traversable before the code under test runs. The permission style
// (execute-only for others) is the one isolation.ValidateTraversable accepts.
func isolatedDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o711); err != nil {
		t.Fatalf("make t.TempDir() traversable (test-fixture-only accommodation, see isolatedDataDir's doc): %v", err)
	}
	if err := os.Chmod(filepath.Dir(dir), 0o711); err != nil {
		t.Fatalf("make t.TempDir() parent traversable (test-fixture-only accommodation, see isolatedDataDir's doc): %v", err)
	}
	return dir
}

// newRealProvider returns the real POSIX isolation.Provider, never
// isolation.NewFake, which cannot see the failure modes this suite covers.
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
// session.Manager, the code path internal/worker/executor drives. dataDir is
// caller-supplied so a test can create two sessions under the same daemon
// data directory, as a worker hosting concurrent sessions for two queues
// would.
func newIsolatedSession(t *testing.T, dataDir, user, group string, isolationCfg workerconfig.IsolationConfig) *session.Session {
	t.Helper()
	provider := newRealProvider(t)
	mgr := session.NewManager(filepath.Join(dataDir, "sessions"), false, provider, isolationCfg, fmtres.ExprLimits{}, testLogger())
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
// carries no Isolation, the no-isolation default. WithSessionRootMode(0o750)
// mirrors cmd/sqi-worker's effectiveSessionRoot non-root DataDir fallback: a
// deployment with no run_as_user configured has no use for the wider,
// isolation-only 0711, so the session root stays at 0750.
func newUnisolatedSession(t *testing.T, dataDir string) *session.Session {
	t.Helper()
	provider := newRealProvider(t)
	mgr := session.NewManager(
		filepath.Join(dataDir, "sessions"), false, provider, workerconfig.IsolationConfig{},
		fmtres.ExprLimits{}, testLogger(),
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

// runInSession launches action inside sess as
// internal/worker/executor.execProcess does: cmd.Dir = sess.WorkDir, Setpgid
// configured first, isolation.Apply second, environment built from
// sess.BaseEnv() via envutil.BuildFromBase. It returns the combined
// stdout+stderr and the run error (nil on a clean zero-exit).
func runInSession(sess *session.Session, action *protocol.Action, envOverrides map[string]string) (string, error) {
	cmd := exec.CommandContext(context.Background(), action.Command, action.Args...)
	cmd.Dir = sess.WorkDir
	proctree.Configure(cmd)
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

// lstatUID returns path's owner uid without following a trailing symlink. The
// distinction matters to TestIsolation_StagedSymlinkDoesNotChownTargetOutsideScratch:
// os.Chown follows symlinks, os.Lchown does not.
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
// Provider and isolation.Apply (not su/sudo, which a minimal container may not
// configure for non-interactive use). cmd.Dir is "/", not a session working
// directory, so this checks only the permission bits on path's own ancestor
// chain, independent of session working-directory traversal.
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
// to POSIX process group pgid, by scanning /proc. It confirms a SIGKILL to a
// process group reaped every descendant, not just the direct child.
// Linux-only; it skips if /proc is unavailable, because then the check cannot
// run.
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
		// proc(5): "pid (comm) state ppid pgrp ...". comm may contain spaces
		// or parentheses, so split on the last ")".
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

// TestIsolation_ProcessRunsAsTargetUID checks working-directory traversal. It
// launches a task as the executor does (cmd.Dir = sess.WorkDir, under
// render-a's resolved credential) and expects `id -u` to report 1001.
//
// If any ancestor of the session leaf is not traversable by render-a (for
// example a 0700 root-owned <dataDir>), the child cannot chdir into cmd.Dir
// and cmd.Run returns an EACCES "permission denied" error before the task
// produces output.
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

// TestIsolation_RunnableEmbeddedFileIsExecutable checks that render-a can
// execute a runnable embedded file. The usual OpenJD shape is
// `embeddedFiles: [{name: run, runnable: true}]` plus
// `onRun.command: "{{Task.File.run}}"`, which resolves to the path this test
// executes.
//
// A file written 0750 root-owned fails here with "permission denied". A
// traversal failure (see TestIsolation_ProcessRunsAsTargetUID) also fails
// this test, with the traversal EACCES, because it blocks the chdir into
// sess.WorkDir before the exec is attempted.
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

// TestIsolation_NonRunnableEmbeddedFileIsReadable checks that render-a can
// read a non-runnable (data) embedded file. A file written 0640 root-owned
// fails here. As in TestIsolation_RunnableEmbeddedFileIsExecutable, a
// traversal failure blocks the chdir before the file's own permission bits
// matter.
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

// TestIsolation_StagedSymlinkDoesNotChownTargetOutsideScratch checks the
// recursive chown of staged content. It drives staging.Stager.StageIn
// directly (the production code internal/worker/executor calls before running
// a task) with an `rsync -a` sync command against an IN entry that is itself a
// symlink pointing outside the scratch tree, at a throwaway file this test
// creates and owns as root. It touches no real system file, only fixtures in
// its own temp directories.
//
// If isolation.ChownRecursive walked the scratch directory with os.Chown, it
// would dereference the symlink and reassign ownership of the outside target
// to the isolated user instead of leaving it root-owned.
func TestIsolation_StagedSymlinkDoesNotChownTargetOutsideScratch(t *testing.T) {
	requireRoot(t)

	provider := newRealProvider(t)
	cred, err := provider.Resolve(context.Background(), isolation.Spec{User: renderAUser})
	if err != nil {
		t.Fatalf("resolve %s: %v", renderAUser, err)
	}

	// A throwaway root-owned file outside any scratch directory this test
	// creates.
	targetDir := t.TempDir()
	targetFile := filepath.Join(targetDir, "root-owned-target")
	if err := os.WriteFile(targetFile, []byte("root-owned-secret\n"), 0o600); err != nil {
		t.Fatalf("create throwaway root-owned target: %v", err)
	}
	if wantUID := lstatUID(t, targetFile); wantUID != 0 {
		t.Fatalf("fixture target is not root-owned (uid %d)", wantUID)
	}

	// A symlink to that target, staged in as a single IN entry. `rsync -a`
	// (through -l) preserves it as a symlink.
	inDir := t.TempDir()
	symlinkPath := filepath.Join(inDir, "link-to-target")
	if err := os.Symlink(targetFile, symlinkPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	// The scratch base uses isolatedDataDir, not a bare t.TempDir(): StageIn
	// validates the scratch base's ancestors whenever cred is non-nil, and a
	// bare t.TempDir() sits under Go testing's per-test parent, fixed at 0700
	// regardless of umask. A production scratch base is an
	// operator-provisioned location with traversable ancestors.
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
// isolation.Provider.Resolve and isolation.Apply. cmd.Dir is "/" rather than a
// session's WorkDir: the test is about Credential.Groups, separate from the
// working-directory traversal that TestIsolation_ProcessRunsAsTargetUID
// covers.
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

// TestIsolation_SupplementaryGroupsStripsGidZero is the real-OS counterpart
// to stripGID0FromSupplementary's unit test against the fake provider
// (internal/worker/isolation/isolation_unix_test.go). render-a is a member of
// both rendershare (gid 3000, an ordinary supplementary group that must be
// preserved) and root (gid 0, via the Dockerfile's `usermod -aG root
// render-a`), the only account in this image placed in gid 0. Of the four
// routes to gid 0 that isolation.Provider closes (explicit group, NSS primary
// gid, primary gid, and the supplementary set), this is the one a fake cannot
// exercise: the fake's GroupIds() is canned, so a fake-only assertion shows
// the stripping code runs, not that a real OS's `id -G` output, which is what
// finalizeGroups filters, no longer contains 0.
func TestIsolation_SupplementaryGroupsStripsGidZero(t *testing.T) {
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

	groups := strings.Fields(string(out))
	if !slices.Contains(groups, strconv.Itoa(renderShareGID)) {
		t.Errorf("`id -G` output = %q, want it to still contain %d (rendershare); only gid 0 is stripped", out, renderShareGID)
	}
	if slices.Contains(groups, "0") {
		t.Errorf("`id -G` output = %q, want it not to contain 0 (root); render-a is a member of root in this "+
			"image (see the Dockerfile) so that this assertion is meaningful", out)
	}
}

// TestIsolation_SessionDirIsPrivateToTargetUser checks that a session working
// directory is 0700 and owned by the target user, and that a second
// unprivileged user (render-b) cannot read into a session created for the
// first (render-a), so isolation holds between two queues' tasks as well as
// between the daemon and one task.
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

// TestIsolation_DaemonSecretAbsentAllowlistedLicensePresent checks the
// env-passthrough policy across a real uid switch: the daemon's
// AWS_ACCESS_KEY_ID must not reach an isolated task, while an
// operator-allowlisted licensing variable must. cmd.Dir is "/" rather than a
// session WorkDir, for the same reason as
// TestIsolation_SupplementaryGroupsPreserved: the test is about
// session.BaseEnv()'s filtering.
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

// TestIsolation_ProcessGroupKillReapsPrivilegeDroppedGrandchild checks that
// isolation.Apply preserves Setpgid (proctree.Configure is applied first, as
// in internal/worker/executor.execProcess): a privilege-dropped child that
// spawns grandchildren must have all of them reaped by a SIGKILL to the
// process group. cmd.Dir is "/" because the test is about SysProcAttr, not
// working-directory traversal.
func TestIsolation_ProcessGroupKillReapsPrivilegeDroppedGrandchild(t *testing.T) {
	requireRoot(t)
	cmd, waited := startIsolatedSleepGroup(t, nil)
	pid := cmd.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL process group -%d: %v", pid, err)
	}
	<-waited
	if !waitGroupGone(t, pid, 5*time.Second) {
		t.Errorf("a descendant of pid %d survived SIGKILL to its process group; isolation.Apply may have overwritten Setpgid", pid)
	}
}

// TestIsolation_SweepKillsIsolatedOrphan checks that a root worker's startup
// sweep reads and kills a tree its killed predecessor left running as another
// account: the tree runs as render-a, is recorded and never released (the
// worker died), and the sweep must still find it by its environment marker.
func TestIsolation_SweepKillsIsolatedOrphan(t *testing.T) {
	requireRoot(t)
	ledger, err := proctree.OpenLedger(filepath.Join(t.TempDir(), "proctrees"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	cmd, _ := startIsolatedSleepGroup(t, proctree.MarkEnv([]string{"PATH=/usr/bin:/bin"}))
	tree, err := proctree.Supervise(cmd)
	if err != nil {
		t.Fatalf("supervise: %v", err)
	}
	if err := tree.Track(ledger, proctree.Label{Kind: "task", AttemptID: "iso-1"}); err != nil {
		t.Fatalf("track: %v", err)
	}
	if err := ledger.Sweep(context.Background(), testLogger()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !waitGroupGone(t, cmd.Process.Pid, 5*time.Second) {
		t.Errorf("a process of the isolated tree (group %d, running as %s) survived the sweep", cmd.Process.Pid, renderAUser)
	}
}

// startIsolatedSleepGroup starts `sh -c "sleep 300 & sleep 300 & wait"` as
// render-a in a process group of its own (proctree.Configure before
// isolation.Apply, as in internal/worker/executor.execProcess), with env (nil
// inherits this process's), and waits until the shell has forked both sleeps.
// cmd.Dir is "/" because the callers are about the process tree, not
// working-directory traversal. One goroutine owns Wait, so the shell is
// reaped as soon as it is killed; waited closes then. Cleanup kills the group.
func startIsolatedSleepGroup(t *testing.T, env []string) (cmd *exec.Cmd, waited <-chan struct{}) {
	t.Helper()
	cred, err := newRealProvider(t).Resolve(context.Background(), isolation.Spec{User: renderAUser})
	if err != nil {
		t.Fatalf("resolve %s: %v", renderAUser, err)
	}
	cmd = exec.CommandContext(context.Background(), "sh", "-c", "sleep 300 & sleep 300 & wait")
	cmd.Dir = "/"
	proctree.Configure(cmd)
	if err := isolation.Apply(cmd, cred); err != nil {
		t.Fatalf("apply credential: %v", err)
	}
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		t.Fatalf("start process-group fixture: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		cmd.Wait() //nolint:errcheck // the test kills the group; only reaping matters
	}()
	pgid := cmd.Process.Pid
	t.Cleanup(func() {
		syscall.Kill(-pgid, syscall.SIGKILL) //nolint:errcheck // ESRCH when the test already killed it
		<-done
	})
	time.Sleep(300 * time.Millisecond) // let the shell fork both sleeps
	return cmd, done
}

// waitGroupGone reports whether every process of group pgid has gone within d.
func waitGroupGone(t *testing.T, pgid int, d time.Duration) bool {
	t.Helper()
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if !anyProcessInGroup(t, pgid) {
			return true
		}
	}
	return false
}

// TestIsolation_EnvironmentActionTimeoutReapsPrivilegeDroppedChild proves an
// environment action runs contained as the target account: its process group
// survives isolation.Apply, so a timeout stops the whole tree and not just the
// shell. The onEnter action backgrounds a sleep and waits on it; the sleep is
// a grandchild of the session manager, running as render-a, and must be gone
// once Create has failed on the timeout. While it still runs, its real uid is
// read from /proc to show the action did run as the target account.
func TestIsolation_EnvironmentActionTimeoutReapsPrivilegeDroppedChild(t *testing.T) {
	requireRoot(t)
	target, err := user.Lookup(renderAUser)
	if err != nil {
		t.Fatalf("look up %s: %v", renderAUser, err)
	}
	dataDir := isolatedDataDir(t)
	provider := newRealProvider(t)
	mgr := session.NewManager(filepath.Join(dataDir, "sessions"), false, provider,
		workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, testLogger())

	// The action runs as render-a and writes the pid where the test can read it.
	pidDir := t.TempDir()
	if err := os.Chmod(pidDir, 0o777); err != nil {
		t.Fatalf("make the pid directory writable by the target account: %v", err)
	}
	if err := os.Chmod(filepath.Dir(pidDir), 0o711); err != nil {
		t.Fatalf("make the pid directory traversable: %v", err)
	}
	pidFile := filepath.Join(pidDir, "child.pid")

	msg := &protocol.AssignMsg{
		JobID:     "job-" + t.Name(),
		TaskID:    "task-1",
		AttemptID: "attempt-1",
		Isolation: &protocol.IsolationSpec{User: renderAUser},
		Environments: []protocol.AssignEnvironment{{
			Name: "slow-env",
			OnEnter: &protocol.Action{
				Command:        "sh",
				Args:           []string{"-c", "sleep 300 & echo $! > " + pidFile + "; wait"},
				TimeoutSeconds: 2,
			},
		}},
	}

	type created struct {
		sess *session.Session
		err  error
	}
	result := make(chan created, 1)
	go func() {
		sess, err := mgr.Create(context.Background(), msg)
		result <- created{sess, err}
	}()

	pid := waitForPID(t, pidFile, 10*time.Second)
	// Read while the action is still running: its two-second timeout starts
	// only once it is running, and the pid file appears as soon as it is.
	if got, want := realUID(t, pid), target.Uid; got != want {
		t.Errorf("environment action's real uid = %s, want %s (%s)", got, want, renderAUser)
	}

	select {
	case r := <-result:
		if r.sess != nil {
			mgr.Cleanup(context.Background(), r.sess, true)
			t.Fatal("Create returned a session although the onEnter action timed out")
		}
		if r.err == nil || !strings.Contains(r.err.Error(), "timed out") {
			t.Errorf("Create error = %v, want one naming the timeout", r.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Create did not return after the onEnter action's timeout")
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("process %d, started by the timed-out onEnter action, survived: the action's process tree was not stopped", pid)
}

// waitForPID polls file until it holds a pid and returns it.
func waitForPID(t *testing.T, file string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(file); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(b))); convErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s did not receive a pid within %s", file, timeout)
	return 0
}

// realUID returns the real uid of process pid, from /proc.
func realUID(t *testing.T, pid int) string {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatalf("read the status of process %d: %v", pid, err)
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "Uid:"); ok {
			if f := strings.Fields(rest); len(f) > 0 {
				return f[0]
			}
		}
	}
	t.Fatalf("no Uid line in /proc/%d/status", pid)
	return ""
}

// TestIsolation_NoRunAsUserBehaviorUnchanged pins the no-isolation default:
// with no run_as_user configured anywhere, the full daemon environment is
// inherited, the workdir mode is 0750 and no credential is resolved. It also
// checks the split between data_dir and the session root: a directory in the
// role of the worker's data_dir (holding only worker.id in production) must
// come out of session creation at the mode it went in at. session.Manager
// never widens it, whether or not isolation is configured, because data_dir
// holds the worker's persistent, server-correlated identity and must stay
// private.
func TestIsolation_NoRunAsUserBehaviorUnchanged(t *testing.T) {
	requireRoot(t)
	t.Setenv("ARBITRARY_DAEMON_VAR", "inherited-value")

	// A real data_dir (as workerconfig.LoadOrCreateWorkerID produces it) is
	// 0700. t.TempDir()'s mode comes from the testing package, so set the
	// precondition explicitly.
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
		t.Errorf("data_dir mode = %o after session creation, want 0700 unchanged; "+
			"data_dir is never widened for session traversal (see workerconfig.LoadOrCreateWorkerID's doc)", got)
	}
	sessionRoot := filepath.Join(dataDir, "sessions")
	if got := statMode(t, sessionRoot); got != 0o750 {
		t.Errorf("session root mode = %o, want 0750 (a deployment with no run_as_user configured "+
			"has no use for the wider, isolation-only 0711)", got)
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
// counterpart to internal/worker/session's unit tests with a fake credential,
// TestCredentialClose_ClosedExactlyOnceOnNormalCleanup and
// TestCredentialClose_NeverCalledWhenCredentialNeverObtained. It covers
// Manager.Create's OnEnter-failure teardown path: a credential is obtained
// (the account resolves), but a later OnEnter action fails, so Create tears
// everything down itself, including closing the credential, before returning
// the error.
//
// This needs real root. The session unit tests fake applyCredential and never
// exec under a switched identity; this test drives the real isolation.Apply
// for its OnEnter failure ("sh -c exit 1"). exec.Cmd.SysProcAttr.Credential
// always calls setgroups(2), which requires CAP_SETGID even to set a
// process's own current supplementary group list unless the caller is already
// privileged. Unprivileged, the "exit 1" failure would be preempted by an
// EPERM out of isolation.Apply itself. As root (requireRoot, and make
// test-isolation's container), the OnEnter action fails with `exit 1` run as
// render-a.
//
// session.closeCredentialFn, the call-counting seam the unit tests swap to
// count Close calls, is unexported and unreachable from this package. This
// test observes the teardown through its effect instead: in Create's
// environment-setup-failure branch, closeCredential(cred) is followed by
// os.RemoveAll(workDir) (see internal/worker/session/session.go). This
// assignment carries no path map, so a session directory that is gone after a
// failing OnEnter shows that branch ran to completion.
func TestIsolation_CredentialClosedOnEnterEnvironmentsFailure(t *testing.T) {
	requireRoot(t)
	dataDir := isolatedDataDir(t)
	provider := newRealProvider(t)
	sessionRoot := filepath.Join(dataDir, "sessions")
	mgr := session.NewManager(sessionRoot, false, provider, workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, testLogger())

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
		// Create returns a session with a nil error, or nil with a non-nil
		// error, never both (see its doc). Clean up so a violation does not
		// also leak a session directory.
		mgr.Cleanup(context.Background(), sess, true)
		t.Fatalf("Create must return nil session alongside a non-nil error; got %+v", sess)
	}
	if err == nil {
		t.Fatal("expected Create to fail when OnEnter fails")
	}

	// Create generates the session ID, so the test does not know the workDir
	// path. This test's sessionRoot holds nothing else, so an empty sessionRoot
	// shows the one workDir Create made was removed by the OnEnter-failure
	// teardown branch.
	entries, rdErr := os.ReadDir(sessionRoot)
	if rdErr != nil {
		t.Fatalf("read session root %q: %v", sessionRoot, rdErr)
	}
	if len(entries) != 0 {
		t.Errorf("session root has %d leftover entries after a failed Create, want 0; "+
			"the OnEnter-failure teardown branch (which closes the credential before removing "+
			"the working directory) must run to completion", len(entries))
	}
}
