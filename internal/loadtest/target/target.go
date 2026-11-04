// SPDX-License-Identifier: AGPL-3.0-or-later

// Package target provides the sqi-server a load run drives: a managed one the
// harness starts, configures and stops itself, or an attached one somebody
// else runs, on which the harness creates and later deletes its own farm.
package target

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"

	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/simworker"
	"github.com/uberware/sqi/internal/tlsutil"
)

// Target is the server a run drives and the farm it owns there.
type Target interface {
	// HTTPURL is the base URL of the REST and WebSocket API.
	HTTPURL() string
	// NATSURL is the broker URL workers dial.
	NATSURL() string
	// APIKey is the bearer key every REST and WebSocket call sends, "" when
	// the server's auth gate is off.
	APIKey() string
	// FarmID is the farm the run created.
	FarmID() string
	// QueueIDs maps each scenario queue name to its id.
	QueueIDs() map[string]string
	// DataDir is the server's data directory, "" when the harness cannot
	// reach it.
	DataDir() string
	// Credentials supplies each simulated worker's broker credential.
	Credentials() simworker.Credentials
	// TLS is the broker and API TLS material, the zero value without TLS.
	TLS() simworker.TLSFiles
	// NoopCommand is the command a job's tasks run: the no-op real workers
	// execute. Simulated workers never execute it.
	NoopCommand() scenario.Command
	// Close releases what the target created: a managed target stops every
	// process it started, an attached one deletes its farm unless kept. It is
	// safe to call more than once.
	Close(ctx context.Context) error
}

// placeholderCommand is the command of a target with no real workers. The
// path is never executed, but a job template needs one.
const placeholderCommand = "sqi-load-noop"

// NewClient returns a REST client for t, carrying its API key and trusting
// its CA.
func NewClient(t Target) (*client.Client, error) {
	return newClient(t.HTTPURL(), t.APIKey(), t.TLS())
}

func newClient(httpURL, apiKey string, files simworker.TLSFiles) (*client.Client, error) {
	hc, err := httpClient(files)
	if err != nil {
		return nil, err
	}
	opts := []client.Option{client.WithHTTPClient(hc)}
	if apiKey != "" {
		opts = append(opts, client.WithAPIKey(apiKey))
	}
	return client.New(httpURL, opts...), nil
}

// httpClient returns an HTTP client trusting files' CA, and the default
// transport's roots when it names none.
func httpClient(files simworker.TLSFiles) (*http.Client, error) {
	// No Timeout here: client.Client puts a deadline on every call but SubmitJob.
	hc := &http.Client{}
	if files.CAFile == "" && files.CertFile == "" && !files.InsecureSkipVerify {
		return hc, nil
	}
	cfg, err := tlsutil.ClientConfig(files.CAFile, files.CertFile, files.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("target: TLS client: %w", err)
	}
	cfg.InsecureSkipVerify = files.InsecureSkipVerify
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("target: default transport is not an *http.Transport")
	}
	tr = tr.Clone()
	tr.TLSClientConfig = cfg
	hc.Transport = tr
	return hc, nil
}

// farmName is the name of the farm a run creates.
func farmName(runID string) string { return "load-" + runID }

// farm is what setupFarm created.
type farm struct {
	id       string
	queueIDs map[string]string
	// poolIDs are the usage pools this run created, by name; a pool that
	// already existed on the server is reused and left out.
	poolIDs map[string]string
}

// setupFarm creates the run's farm, its queues and its usage pools. When it
// fails part way, the returned farm holds what was created, for teardown.
func setupFarm(ctx context.Context, c *client.Client, name string, spec scenario.FarmSpec, logger *slog.Logger) (farm, error) {
	f := farm{queueIDs: map[string]string{}, poolIDs: map[string]string{}}
	id, err := c.CreateFarm(ctx, name)
	if err != nil {
		return f, fmt.Errorf("target: create farm %s: %w", name, err)
	}
	f.id = id
	for _, q := range spec.Queues {
		qid, err := c.CreateQueue(ctx, id, q.Name, q.MaxConcurrentTasks)
		if err != nil {
			return f, fmt.Errorf("target: create queue %s: %w", q.Name, err)
		}
		f.queueIDs[q.Name] = qid
	}
	for _, p := range spec.UsagePools {
		pid, err := c.CreateUsagePool(ctx, p.Name, p.Limit)
		if isStatus(err, http.StatusConflict) {
			// Pools are global and a job names one by name, so an existing
			// pool of that name is the one the run's jobs claim.
			logger.WarnContext(ctx, "usage pool already exists; using it and leaving it in place",
				slog.String("pool", p.Name))
			continue
		}
		if err != nil {
			return f, fmt.Errorf("target: create usage pool %s: %w", p.Name, err)
		}
		f.poolIDs[p.Name] = pid
	}
	return f, nil
}

// isStatus reports whether err is an API error with HTTP status code.
func isStatus(err error, code int) bool {
	var apiErr *client.APIError
	return errors.As(err, &apiErr) && apiErr.Status == code
}

// copyMap returns a copy of m, so callers cannot change a target's state.
func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	maps.Copy(out, m)
	return out
}

func discardIfNil(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.New(slog.DiscardHandler)
	}
	return l
}
