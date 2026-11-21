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
	"time"
)

// sweepOps is what a sweep needs from the operating system, so the algorithm
// can be tested against a fake process table.
type sweepOps struct {
	// list returns every process ID on the host.
	list func() ([]int, error)
	// inspect returns the process's EnvVar value ("" when it has none) and
	// whether it is a zombie. An error means it could not be read (it exited,
	// or belongs to an account the worker may not read).
	inspect func(pid int) (token string, zombie bool, err error)
	// kill sends SIGKILL to the process alone. A process already gone is not
	// an error.
	kill     func(pid int) error
	deadline time.Duration
	poll     time.Duration
}

// sweptTree is a record the sweep is looking for, and what it found.
type sweptTree struct {
	rec    record
	killed int
}

// sweepWith kills every process carrying a recorded token, then deletes every
// record. Only an unreadable ledger directory is returned as an error; every
// other problem is logged and the sweep goes on, so that one bad record or
// process can neither stop the worker from starting nor repeat on every start.
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
		for _, f := range files {
			if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
				logger.ErrorContext(ctx, "proctree: could not delete a process tree record", slog.String("file", f), slog.Any("error", err))
			}
		}
	}()
	if len(trees) == 0 {
		return nil
	}
	survivors, err := killMarked(ctx, trees, ops)
	if err != nil {
		logger.ErrorContext(ctx, "proctree: could not list processes to sweep", slog.Any("error", err))
		return nil
	}
	for _, t := range trees {
		if t.killed > 0 {
			logKilled(ctx, logger, t)
		}
	}
	for pid, tok := range survivors {
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

// killMarked SIGKILLs every live process whose token is in trees, rescanning
// until none is left or ops.deadline passes (a process can fork between a scan
// and its kill). It returns the marked processes still alive at the deadline,
// pid to token.
func killMarked(ctx context.Context, trees map[string]*sweptTree, ops sweepOps) (map[int]string, error) {
	deadline := time.Now().Add(ops.deadline)
	counted := map[int]bool{}
	for {
		pids, err := ops.list()
		if err != nil {
			return nil, err
		}
		alive := map[int]string{}
		for _, pid := range pids {
			tok, zombie, err := ops.inspect(pid)
			if err != nil || zombie || tok == "" {
				continue
			}
			t, ok := trees[tok]
			if !ok {
				continue
			}
			alive[pid] = tok
			if !counted[pid] {
				counted[pid] = true
				t.killed++
			}
			ops.kill(pid) //nolint:errcheck // a failed kill shows as a survivor on the next pass
		}
		if len(alive) == 0 || !time.Now().Before(deadline) {
			return alive, nil
		}
		select {
		case <-ctx.Done():
			return alive, nil
		case <-time.After(ops.poll):
		}
	}
}

// logKilled reports one tree the sweep killed.
func logKilled(ctx context.Context, logger *slog.Logger, t *sweptTree) {
	attrs := []slog.Attr{slog.String("kind", t.rec.Label.Kind)}
	for _, f := range []struct{ key, val string }{
		{"attempt_id", t.rec.Label.AttemptID},
		{"task_id", t.rec.Label.TaskID},
		{"session_id", t.rec.Label.SessionID},
		{"command", t.rec.Label.Command},
	} {
		if f.val != "" {
			attrs = append(attrs, slog.String(f.key, f.val))
		}
	}
	attrs = append(
		attrs,
		slog.Int("pgid", t.rec.PGID),
		slog.Int("processes", t.killed),
		slog.Duration("running_for", time.Since(t.rec.StartedAt).Round(time.Second)),
	)
	logger.LogAttrs(ctx, slog.LevelWarn, "killed a process tree the previous worker left running", attrs...)
}
