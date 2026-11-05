// SPDX-License-Identifier: AGPL-3.0-or-later

package target

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/loadtest/internal/sleep"

	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/simworker"
)

const (
	removalRetry      = 2 * time.Second
	deleteParallelism = 8
	listPage          = 1000
)

// The teardown bounds, variables so a test can shorten them.
var (
	// workerRemovalTimeout bounds the wait for the farm's workers to go
	// offline, which a crashed simulated worker does only when the server's
	// heartbeat timeout passes.
	workerRemovalTimeout = 2 * time.Minute
	// jobDeletionTimeout bounds deleting the farm's jobs.
	jobDeletionTimeout = 5 * time.Minute
	// removalPause is the pause between two passes over the farm's jobs or
	// workers, so a listing that lags the deletes is not re-read in a tight
	// loop.
	removalPause = 200 * time.Millisecond
)

// AttachConfig configures Attach.
type AttachConfig struct {
	HTTPURL, NATSURL string
	// APIKey is sent on every call; "" when the server's auth gate is off.
	APIKey string
	// JoinToken is a reusable join token each simulated worker enrolls with.
	// It is required unless NoAuth says the broker needs no credential.
	JoinToken string
	// DataDir is the server's data directory when the harness can read it,
	// for disk measurements; "" otherwise.
	DataDir string
	TLS     simworker.TLSFiles
	// PostgresDSN is the server's PostgreSQL database when the harness can
	// reach it, for measuring its size; "" otherwise.
	PostgresDSN string
	// NoAuth says the server's broker authentication is off.
	NoAuth bool
	// Keep leaves the run's farm on the server after Close.
	Keep bool
	// SharedPools says other runs use the scenario's usage pools at the same
	// time, as the shards of one run do: Close then leaves every pool in
	// place, created by this run or not, and logs their names. Pools are
	// global, so a shard deleting one would pull it from under the others,
	// and the server refuses while any claim names it.
	SharedPools bool
	Scenario    *scenario.Scenario
	// RunID names the run's farm, load-<RunID>.
	RunID string
	// PoolRunID names the run's usage pools, load_<PoolRunID>_<pool> (see
	// [RunPoolName]); "" means RunID. The shards of one run pass the run's
	// own ID here, so they share their pools while each has a farm.
	PoolRunID string
	// Logger receives the target's own progress; nil discards it.
	Logger *slog.Logger
}

// attached is a target on a server somebody else runs. The run works only in
// its own farm, load-<RunID>.
type attached struct {
	cfg    AttachConfig
	logger *slog.Logger
	client *client.Client
	farm   farm
	creds  simworker.Credentials

	closeOnce sync.Once
	closeErr  error
}

// Attach creates the run's farm, its queues and its usage pools on a running
// server. The pools are the run's own, named load_<PoolRunID>_<pool>, so the
// run never claims slots from a pool somebody else uses; one that already
// exists fails the setup unless SharedPools says another shard of the run
// made it. If setup fails part way, Attach deletes what it created.
func Attach(ctx context.Context, cfg AttachConfig) (Target, error) {
	if err := validateAttach(cfg); err != nil {
		return nil, err
	}
	c, err := newClient(cfg.HTTPURL, cfg.APIKey, cfg.TLS)
	if err != nil {
		return nil, err
	}
	if err := c.Ready(ctx); err != nil {
		return nil, fmt.Errorf("target: server at %s is not ready: %w", cfg.HTTPURL, err)
	}
	a := &attached{
		cfg:    cfg,
		logger: discardIfNil(cfg.Logger).With(slog.String("target", "attach")),
		client: c,
		creds:  simworker.NoCredentials{},
	}
	if !cfg.NoAuth {
		a.creds = simworker.JoinTokenCredentials(c, cfg.JoinToken)
	}
	a.farm, err = setupFarm(ctx, c, farmName(cfg.RunID), cfg.Scenario.Farm, poolPolicy{name: a.PoolName, reuse: cfg.SharedPools}, a.logger)
	if err != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workerRemovalTimeout)
		defer cancel()
		if terr := a.teardown(cctx); terr != nil {
			err = errors.Join(err, terr)
		}
		return nil, err
	}
	return a, nil
}

func validateAttach(cfg AttachConfig) error {
	switch {
	case cfg.HTTPURL == "":
		return errors.New("target: attach needs the server's HTTP URL")
	case cfg.NATSURL == "":
		return errors.New("target: attach needs the server's NATS URL")
	case cfg.RunID == "":
		return errors.New("target: attach needs a run ID to name its farm")
	case cfg.Scenario == nil:
		return errors.New("target: attach has no scenario")
	case !cfg.NoAuth && cfg.JoinToken == "":
		return errors.New("target: attach needs a reusable join token for the simulated workers, or NoAuth when the broker has no authentication")
	}
	return nil
}

func (a *attached) HTTPURL() string                    { return a.cfg.HTTPURL }
func (a *attached) NATSURL() string                    { return a.cfg.NATSURL }
func (a *attached) APIKey() string                     { return a.cfg.APIKey }
func (a *attached) FarmID() string                     { return a.farm.id }
func (a *attached) QueueIDs() map[string]string        { return copyMap(a.farm.queueIDs) }
func (a *attached) DataDir() string                    { return a.cfg.DataDir }
func (a *attached) Credentials() simworker.Credentials { return a.creds }
func (a *attached) TLS() simworker.TLSFiles            { return a.cfg.TLS }

// PoolName returns the server's name for the scenario's usage pool name: the
// run's own namespaced pool.
func (a *attached) PoolName(name string) string {
	if name == "" {
		return ""
	}
	return RunPoolName(cmp.Or(a.cfg.PoolRunID, a.cfg.RunID), name)
}

func (a *attached) PostgresDSN() string { return a.cfg.PostgresDSN }

// StoreLabel says what the harness knows of an attached server's store, which
// is its own whatever the scenario says: a DSN for its database is the one
// sign of which it is.
func (a *attached) StoreLabel() string {
	if a.cfg.PostgresDSN != "" {
		return "postgres (attached)"
	}
	return "attached (unknown)"
}

// NoopCommand is a placeholder: an attached run has no real workers, and
// simulated workers never execute the command.
func (*attached) NoopCommand() scenario.Command {
	return scenario.Command{Path: placeholderCommand}
}

// Close deletes the run's farm with its jobs, workers and queues, and the
// usage pools the run created, unless the run is kept. Workers still online
// are waited for, up to two minutes, since the server removes only offline
// ones.
func (a *attached) Close(ctx context.Context) error {
	a.closeOnce.Do(func() {
		if a.cfg.Keep {
			a.logger.InfoContext(ctx, "keeping the run's farm", slog.String("farm", farmName(a.cfg.RunID)), slog.String("id", a.farm.id))
			return
		}
		a.closeErr = a.teardown(ctx)
	})
	return a.closeErr
}

// teardown deletes everything setupFarm created, in dependency order. It
// keeps going after an error, so it removes as much as it can.
func (a *attached) teardown(ctx context.Context) error {
	var errs []error
	if a.farm.id != "" {
		errs = append(errs, a.deleteJobs(ctx), a.removeWorkers(ctx))
		for name, id := range a.farm.queueIDs {
			if err := a.client.DeleteQueue(ctx, id); err != nil && !isStatus(err, http.StatusNotFound) {
				errs = append(errs, fmt.Errorf("delete queue %s: %w", name, err))
			}
		}
		if err := a.client.DeleteFarm(ctx, a.farm.id); err != nil && !isStatus(err, http.StatusNotFound) {
			errs = append(errs, fmt.Errorf("delete farm: %w", err))
		}
	}
	if a.cfg.SharedPools {
		if len(a.farm.poolIDs) > 0 {
			names := make([]string, 0, len(a.farm.poolIDs))
			for n := range a.farm.poolIDs {
				names = append(names, a.PoolName(n))
			}
			slices.Sort(names)
			a.logger.InfoContext(ctx, "leaving the shared usage pools in place; once every shard has finished, remove them with sqi-load cleanup --run-id "+cmp.Or(a.cfg.PoolRunID, a.cfg.RunID),
				slog.Any("pools", names))
		}
	} else {
		for name, id := range a.farm.poolIDs {
			if err := a.client.DeleteUsagePool(ctx, id); err != nil && !isStatus(err, http.StatusNotFound) {
				errs = append(errs, fmt.Errorf("delete usage pool %s: %w", a.PoolName(name), err))
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("target: farm %s (%s) may be left on the server: %w", farmName(a.cfg.RunID), a.farm.id, err)
	}
	return nil
}

// deleteJobs deletes every job of the farm, several at a time, until the
// farm lists none or jobDeletionTimeout passes. The server cancels a job's
// unfinished tasks as it deletes it.
func (a *attached) deleteJobs(ctx context.Context) error {
	deadline := time.Now().Add(jobDeletionTimeout)
	for {
		page, err := a.client.ListJobs(ctx, a.farm.id, listPage, 0)
		if err != nil {
			return fmt.Errorf("list jobs: %w", err)
		}
		if len(page.Items) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d jobs still listed after %s", page.Total, jobDeletionTimeout)
		}
		ids := make([]string, len(page.Items))
		for i, j := range page.Items {
			ids[i] = j.ID
		}
		if err := a.deleteAll(ctx, ids); err != nil {
			return err
		}
		// A listing that has not caught up with the deletes yet would
		// otherwise be re-read, and re-deleted, as fast as the server answers.
		if !sleep.For(ctx, removalPause) {
			return fmt.Errorf("delete jobs: %w", ctx.Err())
		}
	}
}

// deleteAll deletes jobs ids with deleteParallelism calls in flight. A job
// already gone counts as deleted.
func (a *attached) deleteAll(ctx context.Context, ids []string) error {
	sem := make(chan struct{}, deleteParallelism)
	errs := make([]error, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := a.client.DeleteJob(ctx, id); err != nil && !isStatus(err, http.StatusNotFound) {
				errs[i] = fmt.Errorf("delete job %s: %w", id, err)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// removeWorkers removes every worker of the farm, retrying the ones the
// server still counts as online, until the farm lists none or
// workerRemovalTimeout passes. Every pass pauses: removalRetry when a worker
// is still online (it goes offline only when the server's heartbeat timeout
// passes), removalPause otherwise.
func (a *attached) removeWorkers(ctx context.Context) error {
	deadline := time.Now().Add(workerRemovalTimeout)
	for {
		page, err := a.client.ListWorkers(ctx, a.farm.id, listPage, 0)
		if err != nil {
			return fmt.Errorf("list workers: %w", err)
		}
		if len(page.Items) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d workers still listed after %s", page.Total, workerRemovalTimeout)
		}
		busy := 0
		var errs []error
		for _, w := range page.Items {
			err := a.client.RemoveWorker(ctx, w.ID)
			switch {
			case err == nil, isStatus(err, http.StatusNotFound):
			case isStatus(err, http.StatusConflict):
				busy++
			default:
				errs = append(errs, fmt.Errorf("remove worker %s: %w", w.ID, err))
			}
		}
		if len(errs) > 0 {
			return errors.Join(errs...)
		}
		pause := removalPause
		if busy > 0 {
			pause = removalRetry
		}
		if !sleep.For(ctx, pause) {
			return fmt.Errorf("remove workers: %w", ctx.Err())
		}
	}
}
