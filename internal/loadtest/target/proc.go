// SPDX-License-Identifier: AGPL-3.0-or-later

package target

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// killWait bounds the wait for a killed process to be reaped.
const killWait = 10 * time.Second

// procSpec is a child process to start.
type procSpec struct {
	name    string // for logs and errors: "sqi-server", "sqi-worker 2"
	bin     string
	args    []string
	env     []string
	logPath string // stdout and stderr, appended
}

// proc is a running child process. Its output goes straight to a log file,
// so no goroutine of ours copies it. One goroutine waits for it.
type proc struct {
	name   string
	cmd    *exec.Cmd
	plat   platformProc
	logger *slog.Logger

	done    chan struct{} // closed once the process is reaped
	waitErr error         // set before done closes

	releaseOnce sync.Once

	mu      sync.Mutex
	stalled bool // the harness sent SIGSTOP and no SIGCONT since
}

// startProc starts spec in its own process group (a new console process
// group on Windows), so the harness alone decides when it is interrupted,
// and so a kill reaches what it started too.
func startProc(ctx context.Context, spec procSpec, logger *slog.Logger) (*proc, error) {
	logFile, err := os.OpenFile(spec.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("target: open %s log: %w", spec.name, err)
	}
	// Not ctx: the process outlives the context that started it and is
	// stopped only by stopCtx.
	cmd := exec.CommandContext(context.Background(), spec.bin, spec.args...) //nolint:gosec // G204: the binaries the harness built or was given
	cmd.Env = spec.env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	setProcAttrs(cmd)
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("target: start %s: %w", spec.name, err)
	}
	p := &proc{
		name:   spec.name,
		cmd:    cmd,
		logger: logger.With(slog.String("process", spec.name), slog.Int("pid", cmd.Process.Pid)),
		done:   make(chan struct{}),
	}
	plat, err := supervise(cmd.Process)
	if err != nil {
		// The process still stops through stop; only the protection against
		// the harness itself dying is missing.
		p.logger.WarnContext(ctx, "could not tie the process's lifetime to the harness", slog.Any("error", err))
	}
	p.plat = plat
	// The process outlives ctx; the waiter logs with ctx's values only.
	waitCtx := context.WithoutCancel(ctx)
	go func() {
		p.waitErr = cmd.Wait()
		if err := logFile.Close(); err != nil {
			p.logger.WarnContext(waitCtx, "close process log", slog.Any("error", err))
		}
		close(p.done)
	}()
	return p, nil
}

func (p *proc) pid() int { return p.cmd.Process.Pid }

// exited reports whether the process has ended.
func (p *proc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// release frees what supervises the process (on Windows, closing its job
// kills whatever the process left running). It is safe to call more than
// once.
func (p *proc) release() { p.releaseOnce.Do(p.plat.release) }

// stall stops the process alone with SIGSTOP; on Windows it returns
// errStallUnsupported.
func (p *proc) stall() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exited() {
		return fmt.Errorf("target: %s (pid %d) has exited", p.name, p.pid())
	}
	if err := stopProcess(p.pid()); err != nil {
		return fmt.Errorf("target: stop %s (pid %d): %w", p.name, p.pid(), err)
	}
	p.stalled = true
	return nil
}

// resume continues the process with SIGCONT. Continuing a process that is
// not stopped does nothing, so it is safe to call on any process.
func (p *proc) resume() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exited() {
		p.stalled = false
		return nil
	}
	if err := contProcess(p.pid()); err != nil {
		return fmt.Errorf("target: continue %s (pid %d): %w", p.name, p.pid(), err)
	}
	p.stalled = false
	return nil
}

func (p *proc) isStalled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stalled
}

// killOne kills the process alone, not its group: what it started keeps
// running. It returns once the process is reaped, or an error when it was not
// reaped within killWait or ctx ended first. It does not release the process's
// supervision, so on Windows its job (and the processes still in it) stays
// until stopCtx or release.
func (p *proc) killOne(ctx context.Context) error {
	if p.exited() {
		return nil
	}
	if err := killOne(p.cmd.Process); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("target: kill %s (pid %d): %w", p.name, p.pid(), err)
	}
	timer := time.NewTimer(killWait)
	defer timer.Stop()
	select {
	case <-p.done:
		p.mu.Lock()
		p.stalled = false
		p.mu.Unlock()
		return nil
	case <-timer.C:
		return fmt.Errorf("target: %s (pid %d) did not exit %s after kill", p.name, p.pid(), killWait)
	case <-ctx.Done():
		return fmt.Errorf("target: waiting for %s (pid %d) to exit after kill: %w", p.name, p.pid(), ctx.Err())
	}
}

// stopCtx stops the process (see stop) and then releases its supervision.
// It is safe to call more than once and on a process that already exited.
func (p *proc) stopCtx(ctx context.Context, grace time.Duration) error {
	defer p.release()
	return p.stop(ctx, grace)
}

// stop interrupts the process, waits up to grace for it to exit, then kills
// it (and its process group); a done ctx cuts the grace short. A process the
// harness stalled is continued first, so it can act on the interrupt rather
// than wait out the grace. It returns once the process is reaped, or an
// error when even a kill did not end it.
func (p *proc) stop(ctx context.Context, grace time.Duration) error {
	if p.exited() {
		return nil
	}
	if p.isStalled() {
		if err := p.resume(); err != nil {
			p.logger.WarnContext(ctx, "could not continue the stalled process before stopping it", slog.Any("error", err))
		}
	}
	if err := interrupt(p.cmd.Process); err != nil {
		p.logger.WarnContext(ctx, "interrupt failed; killing", slog.Any("error", err))
		return p.kill(ctx)
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-p.done:
		return nil
	case <-timer.C:
		p.logger.WarnContext(ctx, "process did not exit after interrupt; killing", slog.Duration("grace", grace))
	case <-ctx.Done():
		p.logger.WarnContext(context.WithoutCancel(ctx), "stop canceled; killing", slog.Any("cause", context.Cause(ctx)))
	}
	return p.kill(ctx)
}

func (p *proc) kill(ctx context.Context) error {
	if err := killTree(p.cmd.Process, p.plat); err != nil && !errors.Is(err, os.ErrProcessDone) {
		p.logger.WarnContext(context.WithoutCancel(ctx), "kill failed", slog.Any("error", err))
	}
	timer := time.NewTimer(killWait)
	defer timer.Stop()
	select {
	case <-p.done:
		return nil
	case <-timer.C:
		return fmt.Errorf("target: %s (pid %d) did not exit %s after kill", p.name, p.pid(), killWait)
	}
}

// exitError describes how a process that should still be running ended.
func (p *proc) exitError() error {
	<-p.done
	if p.waitErr != nil {
		return fmt.Errorf("%s exited: %w", p.name, p.waitErr)
	}
	return fmt.Errorf("%s exited with status 0", p.name)
}

// childEnv returns base without any SQI_ variable (case-insensitively, as
// Windows compares names), followed by extra. The harness configures its
// children completely, so a variable the operator has set for their own
// server must not reach the one under test.
func childEnv(base []string, extra ...string) []string {
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(strings.ToUpper(name), "SQI_") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, extra...)
}

// tailBytes bounds how much of a log tailLines reads.
const tailBytes = 64 << 10

// tailLines returns up to the last n lines of the file at path, for an error
// message. A file that cannot be read yields a note saying so.
func tailLines(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf("(no log at %s: %v)\n", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Sprintf("(cannot stat %s: %v)\n", path, err)
	}
	offset := max(info.Size()-tailBytes, 0)
	data, err := io.ReadAll(io.NewSectionReader(f, offset, info.Size()-offset))
	if err != nil {
		return fmt.Sprintf("(cannot read %s: %v)\n", path, err)
	}
	lines := strings.SplitAfter(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if offset > 0 && len(lines) > 0 {
		lines = lines[1:] // the first line was cut by the offset
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := strings.Join(lines, "")
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out
}
