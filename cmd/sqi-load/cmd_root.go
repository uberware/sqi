// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/uberware/sqi/internal/loadtest/report"
	"github.com/uberware/sqi/internal/loadtest/target"
)

// Exit statuses; see the package documentation.
const (
	exitOK        = 0
	exitInvariant = 1
	exitInvalid   = 2
	exitUsage     = 3
)

// app is one invocation of sqi-load.
type app struct {
	r              Runner
	cleanup        func(context.Context, target.CleanupConfig) (target.Cleaned, error)
	stdout, stderr io.Writer
	args           []string
	// code is the exit status a command that succeeded decided on.
	code int
}

// execute runs sqi-load with args and returns its exit status.
func execute(ctx context.Context, args []string, stdout, stderr io.Writer, r Runner) int {
	a := &app{r: r, stdout: stdout, stderr: stderr, args: args, cleanup: target.Cleanup}
	if c, ok := r.(Cleaner); ok {
		a.cleanup = c.Cleanup
	}
	root := a.rootCmd()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(stderr, "sqi-load: %v\n", err)
		if ce, ok := errors.AsType[*codeError](err); ok {
			return ce.code
		}
		return exitUsage
	}
	return a.code
}

// codeError is an error with its own exit status.
type codeError struct {
	code int
	err  error
}

func (e *codeError) Error() string { return e.err.Error() }
func (e *codeError) Unwrap() error { return e.err }

func (a *app) rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "sqi-load",
		Short: "sqi load and soak harness",
		Long: `sqi-load runs a load scenario against an sqi-server, with a fleet of
simulated workers, and writes a run record: run.json, summary.md and
series.csv in runs/<run-id>/.

Use "sqi-load scenarios" to list the built-in scenarios and
"sqi-load run --scenario smoke" for a one-minute check of every axis.

Exit status: 0 valid and every invariant held, 1 an invariant failed,
2 INVALID (generator saturated, or no submission accepted) or interrupted,
3 usage, setup or teardown error. An invariant failure wins over the
others. Findings, errors included, never change the exit status.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		a.runCmd(report.ModeLoad),
		a.runCmd(report.ModeSoak),
		a.sweepCmd(),
		a.compareCmd(),
		a.mergeCmd(),
		a.cleanupCmd(),
		a.scenariosCmd(),
		versionCmd(),
	)
	return root
}

// logger is the harness's own progress log, on standard error.
func (a *app) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(a.stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// verdict is the exit status a run record earns. allowSaturated accepts a run
// INVALID because the generator saturated, but not an incomplete one, nor one
// whose submissions the server all refused. Findings never change it.
func verdict(rec *report.Record, allowSaturated bool) int {
	switch {
	case rec == nil:
		return exitUsage
	case invariantFailed(rec):
		return exitInvariant
	case !rec.Complete:
		return exitInvalid
	case !rec.Valid && (!allowSaturated || rec.NoneAccepted()):
		return exitInvalid
	}
	return exitOK
}

func invariantFailed(rec *report.Record) bool {
	for _, inv := range rec.Invariants {
		if !inv.OK {
			return true
		}
	}
	return false
}

// describe is a one-line account of a run record.
func describe(rec *report.Record) string {
	state := "complete"
	if !rec.Complete {
		state = "INCOMPLETE"
	}
	validity := "VALID"
	if !rec.Valid {
		validity = "INVALID (" + rec.InvalidReason + ")"
	}
	held := 0
	var failed []string
	for _, inv := range rec.Invariants {
		if inv.OK {
			held++
		} else {
			failed = append(failed, inv.Name)
		}
	}
	s := fmt.Sprintf("run %s: %s, %s, %d of %d invariants held", rec.RunID, state, validity, held, len(rec.Invariants))
	if len(failed) > 0 {
		s += fmt.Sprintf(" (failed: %v)", failed)
	}
	return s
}
