// SPDX-License-Identifier: AGPL-3.0-or-later

// Command noop is the task a load run's real workers execute. By default it
// ignores its arguments, prints one line and exits 0, so a real sqi-worker
// exercises the full task lifecycle at no cost of its own.
//
// Given a --sqi-load-config=<noop.json> argument, a task of a class the config
// lists runs as that class's real_task: it records itself in the marker
// directory, prints for the class's duration and starts the children the
// class asks for, a chatty one printing to the task's stdout and a silent one
// with no stdio of the task's, which writes the task's output file. The
// children re-execute this command with a --sqi-load-role argument and stay in
// the task's process group.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	"github.com/uberware/sqi/internal/loadtest/marker"
	"github.com/uberware/sqi/internal/loadtest/scenario"
)

const (
	defaultLine = "sqi-load noop task"
	chattyLine  = "sqi-load noop chatty"

	roleArgPrefix   = "--sqi-load-role="
	execIDArgPrefix = "--sqi-load-exec-id="

	// childEvery is how often a child prints or writes its output.
	childEvery = 200 * time.Millisecond

	// Exit codes. exitBrokenStdout ends a process whose stdout write failed
	// without recording an end, as SIGPIPE ends it on POSIX.
	exitConfig       = 2
	exitBrokenStdout = 3
	exitChildren     = 4
)

func main() { os.Exit(run(os.Args[1:])) }

// task is one real task process's settings.
type task struct {
	args        []string
	dir         string
	key         string
	incarnation string
	class       marker.NoopClass
}

func run(args []string) int {
	path, ok := scenario.LastArg(args, marker.ConfigArgPrefix)
	if !ok {
		fmt.Println(defaultLine)
		return 0
	}
	cfg, err := marker.ReadConfig(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sqi-load noop:", err)
		return exitConfig
	}
	name, _ := scenario.ClassFromArgs(args)
	class, ok := cfg.Classes[name]
	if !ok {
		fmt.Println(defaultLine)
		return 0
	}
	key, _ := scenario.KeyFromArgs(args)
	t := task{args: args, dir: cfg.MarkerDir, key: key, incarnation: os.Getenv("SQILOAD_INCARNATION"), class: class}
	switch role, _ := scenario.LastArg(args, roleArgPrefix); role {
	case "":
		return t.runMain()
	case marker.RoleChatty:
		return t.runChatty()
	case marker.RoleSilent:
		id, _ := scenario.LastArg(args, execIDArgPrefix)
		return t.runSilent(id)
	default:
		fmt.Fprintf(os.Stderr, "sqi-load noop: unknown role %q\n", role)
		return exitConfig
	}
}

func (t task) duration() time.Duration { return time.Duration(t.class.DurationMS) * time.Millisecond }

// finish records the end and returns code.
func finish(w *marker.Writer, code int) int {
	if err := w.Finish(code); err != nil {
		fmt.Fprintln(os.Stderr, "sqi-load noop:", err)
	}
	return code
}

// runMain prints a line every LogEveryMS (once when it is 0) for the
// duration, beside the children the class asks for, and waits for them.
func (t task) runMain() int {
	w, err := marker.Begin(t.dir, t.key, t.incarnation, marker.RoleMain)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sqi-load noop:", err)
		return exitConfig
	}
	children, err := t.startChildren()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sqi-load noop:", err)
		return finish(w, exitChildren)
	}
	if !t.printFor(defaultLine, time.Duration(t.class.LogEveryMS)*time.Millisecond) {
		return exitBrokenStdout
	}
	for _, c := range children {
		// A child that died, of a broken pipe say, shows in its own record;
		// the task still succeeds.
		c.Wait() //nolint:errcheck // see above
	}
	return finish(w, 0)
}

// printFor prints line at once, then once per interval (only once when the
// interval is 0), until the duration has passed. It reports false when a
// write failed.
func (t task) printFor(line string, interval time.Duration) bool {
	start := time.Now()
	deadline := start.Add(t.duration())
	if _, err := fmt.Println(line); err != nil {
		return false
	}
	if interval > 0 {
		for next := start.Add(interval); next.Before(deadline); next = next.Add(interval) {
			time.Sleep(time.Until(next))
			if _, err := fmt.Println(line); err != nil {
				return false
			}
		}
	}
	time.Sleep(time.Until(deadline))
	return true
}

// startChildren starts the chatty and silent children the class asks for.
func (t task) startChildren() ([]*exec.Cmd, error) {
	var children []*exec.Cmd
	if t.class.ChattyChild {
		c, err := t.startChatty()
		if err != nil {
			return nil, err
		}
		children = append(children, c)
	}
	if t.class.SilentChild {
		c, err := t.startSilent()
		if err != nil {
			return nil, errors.Join(err, killAll(children))
		}
		children = append(children, c)
	}
	return children, nil
}

func killAll(cmds []*exec.Cmd) error {
	var errs []error
	for _, c := range cmds {
		errs = append(errs, c.Process.Kill())
		c.Wait() //nolint:errcheck // killed
	}
	return errors.Join(errs...)
}

// child is a command re-executing this one with the task's own arguments
// plus extra. It inherits the task's environment and process group.
func (t task) child(extra ...string) *exec.Cmd {
	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	args := append(slices.Clone(t.args), extra...)
	return exec.CommandContext(context.Background(), self, args...) //nolint:gosec // G204: this very binary
}

func (t task) startChatty() (*exec.Cmd, error) {
	c := t.child(roleArgPrefix + marker.RoleChatty)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	if err := c.Start(); err != nil {
		return nil, fmt.Errorf("start the chatty child: %w", err)
	}
	return c, nil
}

// startSilent starts the silent child with its stdin, stdout and stderr on
// <marker dir>/logs/<exec id>.log, so it holds nothing of the task's stdio.
func (t task) startSilent() (*exec.Cmd, error) {
	id, err := marker.NewID()
	if err != nil {
		return nil, err
	}
	logs := filepath.Join(t.dir, marker.LogsDir)
	if err := os.MkdirAll(logs, 0o750); err != nil {
		return nil, fmt.Errorf("silent child's log directory: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(logs, id+".log"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("silent child's log: %w", err)
	}
	defer f.Close()
	c := t.child(roleArgPrefix+marker.RoleSilent, execIDArgPrefix+id)
	c.Stdin, c.Stdout, c.Stderr = f, f, f
	if err := c.Start(); err != nil {
		return nil, fmt.Errorf("start the silent child: %w", err)
	}
	return c, nil
}

// runChatty prints to stdout every 200ms for the duration.
func (t task) runChatty() int {
	w, err := marker.Begin(t.dir, t.key, t.incarnation, marker.RoleChatty)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sqi-load noop:", err)
		return exitConfig
	}
	if !t.printFor(chattyLine, childEvery) {
		return exitBrokenStdout
	}
	return finish(w, 0)
}

// runSilent writes the task's output marker every 200ms for the duration.
func (t task) runSilent(id string) int {
	w, err := marker.BeginID(t.dir, id, t.key, t.incarnation, marker.RoleSilent)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sqi-load noop:", err)
		return exitConfig
	}
	deadline := time.Now().Add(t.duration())
	for {
		if err := marker.Output(t.dir, t.key, id, time.Now()); err != nil {
			fmt.Fprintln(os.Stderr, "sqi-load noop:", err)
		}
		if time.Until(deadline) < childEvery {
			break
		}
		time.Sleep(childEvery)
	}
	time.Sleep(time.Until(deadline))
	return finish(w, 0)
}
