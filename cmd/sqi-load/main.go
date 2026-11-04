// SPDX-License-Identifier: AGPL-3.0-or-later

// Command sqi-load is the sqi load and soak harness. It runs a scenario
// against an sqi-server it starts itself or one already running, with a
// fleet of simulated workers, and writes a run record (run.json, summary.md,
// series.csv) per run.
//
// Run "sqi-load --help" for usage.
//
// Exit status:
//
//	0  the run is valid, complete, and every invariant held
//	1  an invariant failed
//	2  the run is INVALID (the generator saturated; --allow-saturated
//	   accepts that) or incomplete (interrupted)
//	3  a usage or setup error: bad flags, an unknown scenario, a target that
//	   would not start, or a teardown that failed
//
// When several apply, 1 wins over 3, which wins over 2: an invariant failure
// is a correctness finding about the server, which a saturated generator
// slows but does not excuse, so it is never hidden behind another status.
package main

import (
	"context"
	"os"

	"github.com/uberware/sqi/internal/loadtest/report"
	"github.com/uberware/sqi/internal/loadtest/runner"
	"github.com/uberware/sqi/internal/loadtest/target"
)

func main() {
	os.Exit(execute(context.Background(), os.Args[1:], os.Stdout, os.Stderr, realRunner{}))
}

// Runner is what the commands drive; the tests replace it.
type Runner interface {
	Run(ctx context.Context, o runner.Options) (*report.Record, error)
	Sweep(ctx context.Context, base runner.Options, path string, values []string) ([]report.SweepPoint, error)
}

// Cleaner is what the cleanup command drives, when the Runner provides it;
// otherwise it is target.Cleanup. The tests replace it.
type Cleaner interface {
	Cleanup(ctx context.Context, cfg target.CleanupConfig) (target.Cleaned, error)
}

// realRunner is the runner package.
type realRunner struct{}

func (realRunner) Run(ctx context.Context, o runner.Options) (*report.Record, error) {
	if o.Mode == report.ModeSoak {
		return runner.Soak(ctx, o)
	}
	return runner.Run(ctx, o)
}

func (realRunner) Sweep(ctx context.Context, base runner.Options, path string, values []string) ([]report.SweepPoint, error) {
	return runner.Sweep(ctx, base, path, values)
}
