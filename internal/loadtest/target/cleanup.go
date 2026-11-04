// SPDX-License-Identifier: AGPL-3.0-or-later

package target

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/simworker"
)

// CleanupConfig configures Cleanup.
type CleanupConfig struct {
	HTTPURL string
	// APIKey is sent on every call; "" when the server's auth gate is off.
	APIKey string
	TLS    simworker.TLSFiles
	// RunID is the run whose leftovers to remove: the --run-id it ran with,
	// shared by the shards of a sharded run.
	RunID string
	// Logger receives one line per thing removed; nil discards it.
	Logger *slog.Logger
}

// Cleaned is what Cleanup removed.
type Cleaned struct {
	// Farms are the names of the farms removed, with their jobs, workers and
	// queues.
	Farms []string
	// Pools are the names of the usage pools removed.
	Pools []string
}

// shardSuffix matches what a shard adds to its run ID for its farm.
var shardSuffix = regexp.MustCompile(`-s[0-9]+$`)

// Cleanup removes what an attached run left on a server, for a run whose
// teardown failed or that was kept: the run's farm, load-<RunID>, or the
// shards' farms, load-<RunID>-s<i>, each with its jobs, workers and queues,
// then the run's usage pools, the ones named load_<RunID>_ (see
// [RunPoolName]). It touches nothing else: a pool of another load run whose
// farm is still on the server is left alone even when its name starts the
// same way, and a pool a job still claims is refused by the server.
//
// Like a run's own teardown it waits, up to two minutes, for the farm's
// workers to go offline, and it retries throttled calls. It keeps going after
// an error, so it removes as much as it can, and returns the errors joined.
func Cleanup(ctx context.Context, cfg CleanupConfig) (Cleaned, error) {
	var done Cleaned
	if cfg.HTTPURL == "" || cfg.RunID == "" {
		return done, errors.New("target: cleanup needs the server's HTTP URL and the run ID")
	}
	logger := discardIfNil(cfg.Logger).With(slog.String("target", "cleanup"))
	c, err := newClient(cfg.HTTPURL, cfg.APIKey, cfg.TLS)
	if err != nil {
		return done, err
	}
	farms, err := c.ListFarms(ctx)
	if err != nil {
		return done, fmt.Errorf("target: list farms: %w", err)
	}
	var errs []error
	others := map[string]bool{} // the run IDs of other load runs still on the server
	for _, f := range farms {
		runID, ok := strings.CutPrefix(f.Name, "load-")
		if !ok {
			continue
		}
		if runID != cfg.RunID && shardSuffix.ReplaceAllString(runID, "") != cfg.RunID {
			others[shardSuffix.ReplaceAllString(runID, "")] = true
			continue
		}
		if err := cleanFarm(ctx, c, f, runID, logger); err != nil {
			errs = append(errs, err)
			continue
		}
		logger.InfoContext(ctx, "removed farm", slog.String("farm", f.Name))
		done.Farms = append(done.Farms, f.Name)
	}
	pools, err := runPools(ctx, c, cfg.RunID, others)
	if err != nil {
		return done, errors.Join(append(errs, err)...)
	}
	for _, p := range pools {
		if err := c.DeleteUsagePool(ctx, p.ID); err != nil && !isStatus(err, http.StatusNotFound) {
			errs = append(errs, fmt.Errorf("target: delete usage pool %s: %w", p.Name, err))
			continue
		}
		logger.InfoContext(ctx, "removed usage pool", slog.String("pool", p.Name))
		done.Pools = append(done.Pools, p.Name)
	}
	return done, errors.Join(errs...)
}

// cleanFarm deletes farm f, which run runID created, with everything in it.
func cleanFarm(ctx context.Context, c *client.Client, f client.Farm, runID string, logger *slog.Logger) error {
	queues, err := c.ListQueues(ctx, f.ID)
	if err != nil {
		return fmt.Errorf("target: list the queues of farm %s: %w", f.Name, err)
	}
	a := &attached{
		// SharedPools: the pools are removed by name afterwards, not here.
		cfg:    AttachConfig{RunID: runID, SharedPools: true},
		logger: logger,
		client: c,
		farm:   farm{id: f.ID, queueIDs: map[string]string{}, poolIDs: map[string]string{}},
	}
	for _, q := range queues {
		a.farm.queueIDs[q.Name] = q.ID
	}
	return a.teardown(ctx)
}

// runPools returns the usage pools of run runID: those carrying its pool
// prefix, less those carrying the longer prefix of another run in others
// (run "big" must not take the pools of a live run "big-2").
func runPools(ctx context.Context, c *client.Client, runID string, others map[string]bool) ([]client.UsagePool, error) {
	all, err := c.ListUsagePools(ctx)
	if err != nil {
		return nil, fmt.Errorf("target: list usage pools: %w", err)
	}
	prefix := PoolPrefix(runID)
	var foreign []string
	for other := range others {
		if p := PoolPrefix(other); p != prefix && strings.HasPrefix(p, prefix) {
			foreign = append(foreign, p)
		}
	}
	var out []client.UsagePool
	for _, p := range all {
		if !strings.HasPrefix(p.Name, prefix) {
			continue
		}
		if slices.ContainsFunc(foreign, func(f string) bool { return strings.HasPrefix(p.Name, f) }) {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}
