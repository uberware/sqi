// SPDX-License-Identifier: AGPL-3.0-or-later

package proctree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// sweepOps is what a sweep needs from the operating system, so the algorithm
// can be tested against a fake process table.
type sweepOps struct {
	// list returns every process ID on the host.
	list func() ([]int, error)
	// inspect describes a process. An error means it could not be read at all,
	// usually because it exited; the sweep skips it.
	inspect func(pid int) (procInfo, error)
	// kill sends SIGKILL to the process alone. A process already gone is not
	// an error.
	kill func(pid int) error
	// deadline is how long the sweep rescans for marked processes; slack is
	// how much longer a scan in progress at the deadline may take before the
	// sweep gives up on it. A read of a process's environment has no timeout of
	// its own and can block in the kernel (a process stuck on a dead NFS
	// mount), so the sweep never waits longer than deadline+slack in all.
	deadline time.Duration
	slack    time.Duration
	poll     time.Duration
	// self is the sweeping process's own pid, which is never killed: a worker
	// started from a recorded tree (a job that restarts the worker) inherits
	// that tree's token.
	self int
	// leaderStart returns when the process with this pid started, in a unit
	// of the platform's choosing that only has to be stable (microseconds since
	// the epoch on macOS). The bool is false when no such process exists or the
	// platform cannot say. It is what makes a group number trustworthy for a
	// process whose environment is withheld; nil means it never is.
	leaderStart func(pid int) (int64, bool)
	// unreadableHint is the advice logged with processes whose environment
	// could not be read.
	unreadableHint string
}

// procInfo is what the sweep reads about one process.
type procInfo struct {
	// token is the process's EnvVar value, "" when it has none.
	token string
	// pgid is the process's group.
	pgid   int
	zombie bool
	// envUnreadable means the environment could not be read: another
	// account's process, which on Linux root may read only with CAP_SYS_PTRACE
	// (a container's default capability set lacks it). Such a process is never
	// killed, since nothing proves it is a recorded tree's.
	envUnreadable bool
	// envHidden means the kernel answered but withheld the environment: macOS
	// 27 does this for Apple's own binaries (/bin/sh, /bin/sleep). A process the
	// worker launched always carries the token, so an empty environment from a
	// readable process means the answer was withheld, not that it was empty.
	// Such a process has no token to match; it is identified by its group
	// instead (see [sweepPass.owner]).
	envHidden bool
}

// sweptTree is a record the sweep is looking for, and what it found.
type sweptTree struct {
	rec record
	// killed holds the processes the sweep sent SIGKILL.
	killed map[int]bool
	// unchecked holds the processes in the record's group whose environment
	// could not be read.
	unchecked map[int]bool
}

// sweepWith kills every process carrying a recorded token, then deletes every
// record unless ctx has ended. Only an unreadable ledger directory is
// returned as an error; every other problem is logged and the sweep goes on,
// so that one bad record or process can neither stop the worker from starting
// nor repeat on every start.
func (l *Ledger) sweepWith(ctx context.Context, logger *slog.Logger, ops sweepOps) error {
	if l == nil {
		return nil
	}
	ents, err := os.ReadDir(l.dir)
	if err != nil {
		return fmt.Errorf("proctree: read ledger %s: %w", l.dir, err)
	}
	trees, files := l.readRecords(ctx, logger, ents)
	defer func() {
		if ctx.Err() != nil {
			return // an interrupted start forgets nothing; the next one sweeps again
		}
		for _, f := range files {
			if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
				logger.ErrorContext(ctx, "proctree: could not delete a process tree record", slog.String("file", f), slog.Any("error", err))
			}
		}
	}()
	if len(trees) == 0 {
		return nil
	}
	res, done := runBounded(ctx, logger, trees, ops)
	if !done {
		return nil
	}
	if res.err != nil {
		logger.ErrorContext(ctx, "proctree: could not list processes to sweep", slog.Any("error", res.err))
		return nil
	}
	for _, t := range trees {
		if len(t.killed) > 0 {
			logTree(ctx, logger, t, "killed a process tree the previous worker left running", len(t.killed),
				slog.Duration("running_for", time.Since(t.rec.StartedAt).Round(time.Second)))
		}
		if len(t.unchecked) > 0 {
			logTree(ctx, logger, t,
				"proctree: could not check processes in a previous worker's process group; they were left running",
				len(t.unchecked), slog.String("hint", ops.unreadableHint))
		}
	}
	for pid, tok := range res.survivors {
		logger.ErrorContext(ctx, "proctree: a process the previous worker left running survived SIGKILL",
			slog.Int("pid", pid), slog.String("token", tok))
	}
	return nil
}

// readRecords decodes the ledger's records of this boot, keyed by token, and
// returns every file to delete once the sweep ends. A temporary file (a write
// a crash cut short) or a record that does not decode is logged and deleted.
func (l *Ledger) readRecords(ctx context.Context, logger *slog.Logger, ents []os.DirEntry) (trees map[string]*sweptTree, files []string) {
	trees = map[string]*sweptTree{}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		path := filepath.Join(l.dir, e.Name())
		files = append(files, path)
		rec, err := decodeRecord(path)
		if err != nil {
			logger.ErrorContext(ctx, "proctree: dropped an unreadable process tree record",
				slog.String("file", path), slog.Any("error", err))
			continue
		}
		if rec.BootID != l.bootID {
			continue // from before a reboot: nothing of it survives
		}
		trees[rec.Token] = &sweptTree{rec: rec}
	}
	return trees, files
}

// decodeRecord reads one record file.
func decodeRecord(path string) (record, error) {
	if strings.HasSuffix(path, tmpExt) {
		return record{}, errors.New("an interrupted write")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return record{}, err
	}
	var rec record
	if err := json.Unmarshal(b, &rec); err != nil {
		return record{}, err
	}
	if rec.Token == "" {
		return record{}, errors.New("no token")
	}
	return rec, nil
}

// sweepPass is the state killMarked keeps across its scans.
type sweepPass struct {
	trees map[string]*sweptTree
	// byGroup finds a record by its tree's process group, for a process whose
	// environment cannot be read.
	byGroup map[int]*sweptTree
	ops     sweepOps
	// current is the pid being inspected, for the log line when a read hangs.
	current atomic.Int64
}

// runBounded runs killMarked on its own goroutine and waits at most
// ops.deadline+ops.slack for it, or until ctx ends. done is false when it gave
// up: the scan is then abandoned (it may still kill marked processes later),
// trees must not be read, and the reason has been logged.
func runBounded(ctx context.Context, logger *slog.Logger, trees map[string]*sweptTree, ops sweepOps) (passResult, bool) {
	s := newSweepPass(trees, ops)
	ch := make(chan passResult, 1)
	go func() {
		sv, err := s.killMarked(ctx)
		ch <- passResult{survivors: sv, err: err}
	}()
	timer := time.NewTimer(ops.deadline + ops.slack)
	defer timer.Stop()
	select {
	case r := <-ch:
		return r, true
	case <-timer.C:
		logger.ErrorContext(ctx, "proctree: the sweep did not finish in time; starting without it",
			slog.Int64("pid", s.current.Load()), slog.Duration("waited", ops.deadline+ops.slack))
	case <-ctx.Done():
		logger.WarnContext(context.Background(), "proctree: the sweep was canceled", slog.Any("error", context.Cause(ctx)))
	}
	return passResult{}, false
}

// passResult is what killMarked returns: the marked processes still alive at
// its deadline, pid to token, or the error listing the processes.
type passResult struct {
	survivors map[int]string
	err       error
}

// newSweepPass prepares a scan of trees.
func newSweepPass(trees map[string]*sweptTree, ops sweepOps) *sweepPass {
	s := &sweepPass{trees: trees, byGroup: make(map[int]*sweptTree, len(trees)), ops: ops}
	for _, t := range trees {
		t.killed, t.unchecked = map[int]bool{}, map[int]bool{}
		s.byGroup[t.rec.PGID] = t
	}
	return s
}

// killMarked SIGKILLs every live process whose token is in s.trees,
// rescanning until none is left or ops.deadline passes (a process can fork
// between a scan and its kill). It returns the marked processes still alive at
// the deadline, pid to token.
func (s *sweepPass) killMarked(ctx context.Context) (map[int]string, error) {
	ops := s.ops
	deadline := time.Now().Add(ops.deadline)
	for {
		pids, err := ops.list()
		if err != nil {
			return nil, err
		}
		alive := map[int]string{}
		for _, pid := range pids {
			if tok, ok := s.visit(pid); ok {
				alive[pid] = tok
			}
		}
		if len(alive) == 0 {
			return alive, nil
		}
		if !time.Now().Before(deadline) {
			return s.stillMarked(ctx, alive), nil
		}
		select {
		case <-ctx.Done():
			return alive, nil
		case <-time.After(ops.poll):
		}
	}
}

// stillMarked waits one poll interval, then returns the processes of alive
// that still run carrying the same token: the deadline's last pass sent
// SIGKILL to everything in alive, and those that died of it did not survive.
func (s *sweepPass) stillMarked(ctx context.Context, alive map[int]string) map[int]string {
	select {
	case <-ctx.Done():
		return alive
	case <-time.After(s.ops.poll):
	}
	left := map[int]string{}
	for pid, tok := range alive {
		if info, err := s.ops.inspect(pid); err == nil && !info.zombie {
			if t, ok := s.owner(info); ok && t.rec.Token == tok {
				left[pid] = tok
			}
		}
	}
	return left
}

// visit looks at one process: it kills a live process carrying a recorded
// token, returning the token and true, and notes an unreadable one in a
// recorded group.
func (s *sweepPass) visit(pid int) (string, bool) {
	if pid == s.ops.self {
		return "", false
	}
	s.current.Store(int64(pid))
	info, err := s.ops.inspect(pid)
	if err != nil || info.zombie {
		return "", false
	}
	if info.envUnreadable {
		if t, ok := s.byGroup[info.pgid]; ok {
			t.unchecked[pid] = true
		}
		return "", false
	}
	t, ok := s.owner(info)
	if !ok {
		return "", false
	}
	t.killed[pid] = true
	s.ops.kill(pid) //nolint:errcheck // a failed kill shows as a survivor on the next pass
	return t.rec.Token, true
}

// owner returns the recorded tree a process belongs to. A process carrying a
// recorded token belongs to that tree. A process whose environment was
// withheld has no token, so it belongs to the tree whose group it is in, but
// only when the group is verified: group numbers are reused, so the record
// must carry its leader's start time and the process now holding that number,
// if there is one, must have started then. A leader that is gone and members
// that remain still count: the kernel does not reuse a group number while the
// group has a member.
func (s *sweepPass) owner(info procInfo) (*sweptTree, bool) {
	if t, ok := s.trees[info.token]; ok { // a record's token is never empty (decodeRecord)
		return t, true
	}
	if !info.envHidden {
		return nil, false
	}
	t, ok := s.byGroup[info.pgid]
	if !ok || t.rec.LeaderStart == 0 || s.ops.leaderStart == nil {
		return nil, false
	}
	if start, alive := s.ops.leaderStart(t.rec.PGID); alive && start != t.rec.LeaderStart {
		return nil, false
	}
	return t, true
}

// logTree reports what the sweep found of one tree: msg, the tree's label and
// group, n processes, and one more attribute.
func logTree(ctx context.Context, logger *slog.Logger, t *sweptTree, msg string, n int, extra slog.Attr) {
	attrs := append(labelAttrs(t.rec), slog.Int("pgid", t.rec.PGID), slog.Int("processes", n), extra)
	logger.LogAttrs(ctx, slog.LevelWarn, msg, attrs...)
}

// labelAttrs is a record's label as log attributes, empty fields left out.
func labelAttrs(rec record) []slog.Attr {
	attrs := []slog.Attr{slog.String("kind", rec.Label.Kind)}
	for _, f := range []struct{ key, val string }{
		{"attempt_id", rec.Label.AttemptID},
		{"task_id", rec.Label.TaskID},
		{"session_id", rec.Label.SessionID},
		{"command", rec.Label.Command},
	} {
		if f.val != "" {
			attrs = append(attrs, slog.String(f.key, f.val))
		}
	}
	return attrs
}
