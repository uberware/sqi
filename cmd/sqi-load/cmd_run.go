// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/pprof"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/uberware/sqi/internal/loadtest/report"
	"github.com/uberware/sqi/internal/loadtest/runner"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/simworker"
	"github.com/uberware/sqi/internal/loadtest/target"
)

// runFlags are the flags of run, soak and sweep.
type runFlags struct {
	scenario       string
	sets           []string
	duration       string
	seed           int64
	out            string
	runID          string
	shard          string
	serverBin      string
	workerBin      string
	postgresDSN    string
	server         string
	nats           string
	apiKey         string
	joinToken      string
	noAuth         bool
	dataDir        string
	caFile         string
	keep           bool
	allowSaturated bool
	sample         time.Duration
	profileEvery   time.Duration
	cpuProfile     string
}

// attachOnly are the flags that describe an attached server; managedOnly
// those that describe a managed one.
var (
	attachOnly  = []string{"nats", "api-key", "join-token", "no-auth", "data-dir", "ca-file", "keep"}
	managedOnly = []string{"server-bin", "worker-bin"}
	// secretFlags are recorded in run.json without their values.
	secretFlags = []string{"api-key", "join-token", "postgres-dsn"}
)

func (f *runFlags) register(fs *pflag.FlagSet, attach bool) {
	fs.StringVar(&f.scenario, "scenario", "", "built-in scenario name or scenario file (required); see sqi-load scenarios")
	fs.StringArrayVar(&f.sets, "set", nil, "override a scenario field, path=value (repeatable), e.g. workers[0].count=500")
	fs.StringVar(&f.duration, "duration", "", "override the scenario's duration, e.g. 30m")
	fs.Int64Var(&f.seed, "seed", 0, "seed for every random choice (default: derived from the run ID)")
	fs.StringVar(&f.out, "out", runner.DefaultOutDir, "directory the run directories go in")
	fs.StringVar(&f.runID, "run-id", "", "run ID (default: <UTC yyyymmdd-hhmmss>-<4 hex>); shards of one run share it")
	fs.StringVar(&f.shard, "shard", "", "run shard i of n of the scenario, as i/n")
	fs.StringVar(&f.serverBin, "server-bin", "", "sqi-server binary of a managed run (default: build it)")
	fs.StringVar(&f.workerBin, "worker-bin", "", "sqi-worker binary for real workers (default: build it)")
	fs.StringVar(&f.postgresDSN, "postgres-dsn", "", "PostgreSQL DSN: a managed run's store (a DSN naming database postgres creates a throwaway one), or an attached server's database to measure")
	fs.BoolVar(&f.allowSaturated, "allow-saturated", false, "exit 0 for an INVALID run whose generator saturated")
	fs.DurationVar(&f.sample, "sample", runner.DefaultSample, "observation sample interval")
	fs.DurationVar(&f.profileEvery, "profile-every", 0, "heap and goroutine profile period (default: 15m in a soak, start and end only otherwise)")
	fs.StringVar(&f.cpuProfile, "cpuprofile", "", "write a CPU profile of sqi-load itself (the generator, not the server) to this file")
	if !attach {
		return
	}
	fs.StringVar(&f.server, "server", "", "attach to the sqi-server at this URL instead of starting one")
	fs.StringVar(&f.nats, "nats", "", "the attached server's NATS URL")
	fs.StringVar(&f.apiKey, "api-key", "", "API key for an attached server with the auth gate on")
	fs.StringVar(&f.joinToken, "join-token", "", "reusable join token the simulated workers enroll with on an attached server")
	fs.BoolVar(&f.noAuth, "no-auth", false, "the attached server's broker needs no credentials")
	fs.StringVar(&f.dataDir, "data-dir", "", "the attached server's data directory, when readable, for disk measurements")
	fs.StringVar(&f.caFile, "ca-file", "", "CA certificate the attached server's TLS certificates chain to")
	fs.BoolVar(&f.keep, "keep", false, "leave the run's farm on the attached server")
}

func (a *app) runCmd(mode string) *cobra.Command {
	f := &runFlags{}
	cmd := &cobra.Command{
		Use:   "run --scenario <name|file> [flags]",
		Short: "Run a scenario once",
		Long: `Run a scenario once against a managed sqi-server (started, by default, from
binaries built from this source tree) or an attached one (--server and --nats),
and write its record to <out>/<run-id>/.`,
		Args: cobra.NoArgs,
	}
	if mode == report.ModeSoak {
		cmd.Use = "soak --scenario <name|file> [flags]"
		cmd.Short = "Run a scenario as a soak: long, with periodic profiles and growth slopes"
		cmd.Long = `Run a scenario as a soak: like run, with heap and goroutine profiles every
15 minutes and the record marked as a soak. Any --duration is accepted.`
	}
	f.register(cmd.Flags(), true)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		o, err := f.options(cmd, mode)
		if err != nil {
			return err
		}
		stop, err := startCPUProfile(f.cpuProfile)
		if err != nil {
			return err
		}
		defer stop()
		return a.runOnce(cmd.Context(), o, f.allowSaturated)
	}
	return cmd
}

// runOnce runs o and sets the exit status from its record.
func (a *app) runOnce(ctx context.Context, o runner.Options, allowSaturated bool) error {
	ctx, abort, stop := a.interruptible(ctx)
	defer stop()
	o.Abort = abort
	if o.Logger == nil {
		o.Logger = a.logger()
	}
	fmt.Fprintf(a.stdout, "sqi-load: %s %s, seed %d, scenario %s, writing %s\n", o.Mode, o.RunID, o.Seed, o.Scenario.Name, o.Directory())
	rec, err := a.r.Run(ctx, o)
	a.code = verdict(rec, allowSaturated)
	if rec != nil {
		fmt.Fprintf(a.stdout, "sqi-load: %s\nsqi-load: summary: %s\n", describe(rec), filepath.Join(o.Directory(), "summary.md"))
	}
	if err != nil {
		fmt.Fprintf(a.stderr, "sqi-load: %v\n", err)
		if a.code != exitInvariant {
			a.code = exitUsage
		}
	}
	return nil
}

// options turns the flags into runner options, rejecting what does not go
// together, and normalizes them: the run ID, seed and directory are known
// before the run starts.
func (f *runFlags) options(cmd *cobra.Command, mode string) (runner.Options, error) {
	attach := f.server != ""
	if err := f.checkTarget(cmd.Flags(), attach); err != nil {
		return runner.Options{}, err
	}
	sc, err := loadScenario(f.scenario, f.sets, f.duration)
	if err != nil {
		return runner.Options{}, err
	}
	var shard report.ShardSpec
	if f.shard != "" {
		if shard, err = report.ParseShard(f.shard); err != nil {
			return runner.Options{}, err
		}
	}
	o := runner.Options{
		Scenario: sc, Mode: mode, RunID: f.runID, OutDir: f.out, Seed: f.seed,
		Sample: f.sample, ProfileEvery: f.profileEvery, Shard: shard, AllowSaturated: f.allowSaturated,
		Flags: recordedFlags(cmd),
	}
	if attach {
		o.Attach = &target.AttachConfig{
			HTTPURL: f.server, NATSURL: f.nats, APIKey: f.apiKey, JoinToken: f.joinToken,
			NoAuth: f.noAuth, DataDir: f.dataDir, PostgresDSN: f.postgresDSN, Keep: f.keep,
		}
		if f.caFile != "" {
			o.Attach.TLS = simworker.TLSFiles{Enabled: "true", CAFile: f.caFile}
		}
	} else {
		if f.postgresDSN != "" && sc.Server.Store != "postgres" {
			return runner.Options{}, fmt.Errorf("--postgres-dsn needs --set server.store=postgres: scenario %s runs on %s, which would ignore the DSN", sc.Name, sc.Server.Store)
		}
		o.Managed = &target.ManagedConfig{ServerBin: f.serverBin, WorkerBin: f.workerBin, PostgresDSN: f.postgresDSN}
	}
	if err := o.Normalize(); err != nil {
		return runner.Options{}, err
	}
	return o, nil
}

// checkTarget rejects flags of the other kind of target.
func (f *runFlags) checkTarget(fs *pflag.FlagSet, attach bool) error {
	if attach {
		if f.nats == "" {
			return errors.New("--server needs --nats, the attached server's NATS URL")
		}
		for _, name := range managedOnly {
			if fs.Changed(name) {
				return fmt.Errorf("--%s applies to a managed server, not one attached with --server", name)
			}
		}
		return nil
	}
	for _, name := range attachOnly {
		if fs.Lookup(name) != nil && fs.Changed(name) {
			return fmt.Errorf("--%s applies to an attached server and needs --server", name)
		}
	}
	if f.shard != "" {
		// Managed shards would each start a server of their own, and merge
		// would then add up counters from different servers.
		return errors.New("--shard needs --server: the shards of a run drive one shared, attached server")
	}
	return nil
}

// loadScenario loads a built-in or a scenario file, applies the --set
// overrides and then --duration, and fills the defaults. Normalizing the run
// options validates the result for the target.
func loadScenario(name string, sets []string, duration string) (*scenario.Scenario, error) {
	if name == "" {
		return nil, errors.New("--scenario is required (built-ins: " + strings.Join(scenario.BuiltinNames(), ", ") + ")")
	}
	var (
		sc  *scenario.Scenario
		err error
	)
	switch {
	case isFile(name):
		data, rerr := os.ReadFile(name)
		if rerr != nil {
			return nil, fmt.Errorf("scenario file: %w", rerr)
		}
		if sc, err = scenario.Parse(data); err != nil {
			return nil, fmt.Errorf("scenario file %s: %w", name, err)
		}
	case slices.Contains(scenario.BuiltinNames(), name):
		if sc, err = scenario.Builtin(name); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown scenario %q: not a scenario file and not a built-in (built-ins: %s)",
			name, strings.Join(scenario.BuiltinNames(), ", "))
	}
	for _, kv := range sets {
		path, value, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("--set %q: want path=value", kv)
		}
		if err := sc.Set(path, value); err != nil {
			return nil, fmt.Errorf("--set %s: %w", kv, err)
		}
	}
	if duration != "" {
		if err := sc.Set("duration", duration); err != nil {
			return nil, fmt.Errorf("--duration: %w", err)
		}
	}
	sc.ApplyDefaults()
	return sc, nil
}

func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// recordedFlags is the command line for the record: the subcommand and the
// flags given, with secrets redacted.
func recordedFlags(cmd *cobra.Command) []string {
	out := []string{cmd.Name()}
	cmd.Flags().Visit(func(fl *pflag.Flag) {
		v := fl.Value.String()
		if slices.Contains(secretFlags, fl.Name) {
			v = "REDACTED"
		}
		if sv, ok := fl.Value.(pflag.SliceValue); ok {
			for _, item := range sv.GetSlice() {
				out = append(out, "--"+fl.Name+"="+item)
			}
			return
		}
		out = append(out, "--"+fl.Name+"="+v)
	})
	return out
}

// startCPUProfile profiles sqi-load itself into path until the returned stop
// is called; an empty path profiles nothing.
func startCPUProfile(path string) (stop func(), err error) {
	if path == "" {
		return func() {}, nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("--cpuprofile: %w", err)
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("--cpuprofile: %w", err)
	}
	return func() {
		pprof.StopCPUProfile()
		f.Close()
	}, nil
}
