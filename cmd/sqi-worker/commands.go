// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"

	"github.com/uberware/sqi/internal/worker/commands"
	"github.com/uberware/sqi/internal/worker/session"
)

// commandsOption hands the operator's worker.commands table to the session
// manager, and records what it holds in the worker log.
//
// Every mapping is logged, and a mapping whose target does not exist is a
// warning: a typo in a path is otherwise found only when the first job names
// the command, in a task log that shows "executable file not found" with no
// hint that the operator already configured it.
func commandsOption(table commands.Table, logger *slog.Logger) session.ManagerOption {
	names := make([]string, 0, len(table))
	for name := range table {
		names = append(names, name)
	}
	slices.Sort(names)
	ctx := context.Background()
	for _, name := range names {
		logger.InfoContext(
			ctx, "worker command mapping",
			slog.String("command", name),
			slog.String("path", table[name].Path),
			slog.Any("args", table[name].Args),
		)
	}
	for _, name := range unresolvableCommands(table) {
		logger.WarnContext(
			ctx, "worker command mapping points at something that cannot be found; "+
				"a job that runs this command will fail",
			slog.String("command", name),
			slog.String("path", table[name].Path),
		)
	}
	return session.WithCommands(commands.NewResolver(table))
}

// unresolvableCommands returns, sorted, the names whose target cannot be found
// on this host: an absolute path that does not exist, or a bare name the
// worker's PATH does not resolve.
func unresolvableCommands(table commands.Table) []string {
	var out []string
	for name, c := range table {
		if filepath.IsAbs(c.Path) {
			if _, err := os.Stat(c.Path); err != nil {
				out = append(out, name)
			}
			continue
		}
		if _, err := exec.LookPath(c.Path); err != nil {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}
