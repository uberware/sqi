// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/uberware/sqi/internal/loadtest/report"
	"github.com/uberware/sqi/internal/loadtest/runner"
)

func (a *app) sweepCmd() *cobra.Command {
	f := &runFlags{}
	var vary string
	cmd := &cobra.Command{
		Use:   "sweep --scenario <name|file> --vary path=v1,v2,... [flags]",
		Short: "Run a scenario once per value of one parameter and find the knee",
		Long: `Run a scenario once per value of one parameter, each on a fresh managed
sqi-server, in the order given (ascending), and write sweep.csv and sweep.md
with the knee: the first value whose p95 submit-to-first-lease exceeds twice
the first value's, whose invariants fail, or whose run is INVALID.

Each point's record goes to <out>/<sweep-id>/<i>-<value>/. A value cannot
contain a comma. sweep exits 0 once every point has run, whatever the knee:
points past the knee are expected to fail. It exits 2 when interrupted and 3
when a point could not be set up.`,
		Args: cobra.NoArgs,
	}
	f.register(cmd.Flags(), false)
	cmd.Flags().StringVar(&vary, "vary", "", "the parameter to sweep and its values, path=v1,v2,... (required)")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		path, values, err := parseVary(vary)
		if err != nil {
			return err
		}
		o, err := f.options(cmd, report.ModeSweepPoint)
		if err != nil {
			return err
		}
		if _, err := runner.SweepScenarios(o.Scenario, path, values); err != nil {
			return err
		}
		ctx, abort, stop := a.interruptible(cmd.Context())
		defer stop()
		o.Abort = abort
		o.Logger = a.logger()
		dir := filepath.Join(o.OutDir, o.RunID)
		fmt.Fprintf(a.stdout, "sqi-load: sweep %s of %s over %s, seed %d, writing %s\n", o.RunID, o.Scenario.Name, vary, o.Seed, dir)
		pts, err := a.r.Sweep(ctx, o, path, values)
		for _, p := range pts {
			if p.Rec != nil {
				fmt.Fprintf(a.stdout, "sqi-load: %s=%s: %s\n", path, p.Value, describe(p.Rec))
			}
		}
		fmt.Fprintf(a.stdout, "sqi-load: sweep table: %s\n", filepath.Join(dir, "sweep.md"))
		switch {
		case err != nil:
			fmt.Fprintf(a.stderr, "sqi-load: %v\n", err)
			a.code = exitUsage
		case ctx.Err() != nil:
			a.code = exitInvalid
		}
		return nil
	}
	return cmd
}

// parseVary reads path=v1,v2,...
func parseVary(s string) (path string, values []string, err error) {
	if s == "" {
		return "", nil, errors.New("--vary is required: a path and its values, like workers[0].count=500,1000")
	}
	path, list, ok := strings.Cut(s, "=")
	if !ok || path == "" || list == "" {
		return "", nil, fmt.Errorf("--vary %q: want a path and its values, like workers[0].count=500,1000", s)
	}
	for v := range strings.SplitSeq(list, ",") {
		if v = strings.TrimSpace(v); v == "" {
			return "", nil, fmt.Errorf("--vary %q: empty value", s)
		}
		values = append(values, v)
	}
	return path, values, nil
}
