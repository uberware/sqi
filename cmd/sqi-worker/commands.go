// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/uberware/sqi/internal/worker/capabilities"
	"github.com/uberware/sqi/internal/worker/commands"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/session"
)

// Where a command's executable was decided.
const (
	sourceConfig   = "config"   // the operator's worker.commands
	sourcePATH     = "PATH"     // the host already has the command
	sourceDetected = "detected" // the worker's own default, from an install it found
)

// commandReport says what one command name resolves to, and why.
type commandReport struct {
	Name    string
	Path    string
	Args    []string
	Source  string
	Version string // the detected install's version, when Source is sourceDetected
}

// commandPlan is the table of command mappings the worker runs with, and a
// report of every command it decided something about.
type commandPlan struct {
	// Table is what the session manager resolves job commands through: the
	// operator's entries plus any detected default.
	Table   commands.Table
	Reports []commandReport
}

// planCommands decides, for each command a detector can supply, where it
// resolves, by this precedence:
//
//  1. the operator's worker.commands entry;
//  2. a command of that name already on PATH, which the host set up and the
//     worker leaves alone;
//  3. the newest install a detector found, as a default.
//
// A default only fills a gap. It never replaces an entry the operator wrote or
// shadows a command the studio's own setup provides, and it carries no
// arguments: flags such as Nuke's --nc depend on the license, which is the
// operator's to state in worker.commands.
//
// The returned Table is a copy; operator is not changed.
func planCommands(operator commands.Table, detected []capabilities.DetectedCommand, lookPath func(string) (string, error)) commandPlan {
	plan := commandPlan{Table: maps.Clone(operator)}
	if plan.Table == nil {
		plan.Table = commands.Table{}
	}
	for name, c := range operator {
		plan.Reports = append(plan.Reports, commandReport{Name: name, Path: c.Path, Args: c.Args, Source: sourceConfig})
	}
	for _, d := range detected {
		if operator.Contains(d.Name) {
			continue
		}
		if found, err := lookPath(d.Name); err == nil {
			plan.Reports = append(plan.Reports, commandReport{Name: d.Name, Path: found, Source: sourcePATH})
			continue
		}
		plan.Table[d.Name] = commands.Command{Path: d.Path}
		plan.Reports = append(plan.Reports, commandReport{Name: d.Name, Path: d.Path, Source: sourceDetected, Version: d.Version})
	}
	slices.SortFunc(plan.Reports, func(a, b commandReport) int { return strings.Compare(a.Name, b.Name) })
	return plan
}

// hostLookPath resolves a command name through this process's PATH.
func hostLookPath(name string) (string, error) { return exec.LookPath(name) }

// detectCommandPlan runs the active detectors on env and plans the commands.
func detectCommandPlan(cfg workerconfig.WorkerConfig, env capabilities.CheckEnv, lookPath func(string) (string, error)) (commandPlan, error) {
	detected, err := capabilities.DetectCommands(cfg.Capabilities, env)
	if err != nil {
		return commandPlan{}, fmt.Errorf("detect commands: %w", err)
	}
	return planCommands(cfg.Worker.Commands, detected, lookPath), nil
}

// option hands the plan's table to the session manager, and records each
// command in the worker log.
//
// A mapping the operator wrote whose target does not exist is a warning: a typo
// in a path is otherwise found only when the first job names the command, in a
// task log that shows "executable file not found" with no hint that the
// operator already configured it.
func (p commandPlan) option(logger *slog.Logger) session.ManagerOption {
	ctx := context.Background()
	for _, r := range p.Reports {
		logger.InfoContext(
			ctx, "worker command",
			slog.String("command", r.Name),
			slog.String("path", r.Path),
			slog.Any("args", r.Args),
			slog.String("source", r.Source),
			slog.String("version", r.Version),
		)
		if r.Source == sourceConfig && !targetExists(r.Path) {
			logger.WarnContext(
				ctx, "worker command mapping points at something that cannot be found; "+
					"a job that runs this command will fail",
				slog.String("command", r.Name),
				slog.String("path", r.Path),
			)
		}
	}
	return session.WithCommands(commands.NewResolver(p.Table))
}

// targetExists reports whether a mapping's target can be found on this host: an
// absolute path that exists, or a bare name the worker's PATH resolves.
func targetExists(path string) bool {
	if filepath.IsAbs(path) {
		_, err := os.Stat(path)
		return err == nil
	}
	_, err := exec.LookPath(path)
	return err == nil
}

// write prints the plan as a table, for `sqi-worker capabilities` and
// `start --dry-run`.
func (p commandPlan) write(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "COMMAND\tRESOLVES TO\tARGS\tSOURCE\tVERSION")
	for _, r := range p.Reports {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Name, r.Path, strings.Join(r.Args, " "), r.Source, r.Version)
	}
	return tw.Flush()
}
