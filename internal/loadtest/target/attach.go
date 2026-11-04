// SPDX-License-Identifier: AGPL-3.0-or-later

package target

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/simworker"
)

const (
	// workerRemovalTimeout bounds the wait for the farm's workers to go
	// offline, which a crashed simulated worker does only when the server's
	// heartbeat timeout passes.
	workerRemovalTimeout = 2 * time.Minute
	removalRetry         = 2 * time.Second
	deleteParallelism    = 8
	listPage             = 1000
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
	// NoAuth says the server's broker authentication is off.
	NoAuth bool
	// Keep leaves the run's farm on the server after Close.
	Keep     bool
	Scenario *scenario.Scenario
	RunID    string
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
// server. A usage pool that already exists there is used as it is and never
// deleted. If setup fails part way, Attach deletes what it created.
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
	a.farm, err = setupFarm(ctx, c, farmName(cfg.RunID), cfg.Scenario.Farm, a.logger)
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
	for name, id := range a.farm.poolIDs {
		if err := a.client.DeleteUsagePool(ctx, id); err != nil && !isStatus(err, http.StatusNotFound) {
			errs = append(errs, fmt.Errorf("delete usage pool %s: %w", name, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("target: farm %s (%s) may be left on the server: %w", farmName(a.cfg.RunID), a.farm.id, err)
	}
	return nil
}

// deleteJobs deletes every job of the farm, several at a time. The server
// cancels a job's unfinished tasks as it deletes it.
func (a *attached) deleteJobs(ctx context.Context) error {
	for {
		page, err := a.client.ListJobs(ctx, a.farm.id, listPage, 0)
		if err != nil {
			return fmt.Errorf("list jobs: %w", err)
		}
		if len(page.Items) == 0 {
			return nil
		}
		ids := make([]string, len(page.Items))
		for i, j := range page.Items {
			ids[i] = j.ID
		}
		if err := a.deleteAll(ctx, ids); err != nil {
			return err
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
// server still counts as online until workerRemovalTimeout passes.
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
		if busy == 0 {
			continue // the next page, if any
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d workers still online after %s", busy, workerRemovalTimeout)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("remove workers: %w", ctx.Err())
		case <-time.After(removalRetry):
		}
	}
}
