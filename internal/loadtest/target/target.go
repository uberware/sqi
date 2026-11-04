// SPDX-License-Identifier: AGPL-3.0-or-later

// Package target provides the sqi-server a load run drives: a managed one the
// harness starts, configures and stops itself, or an attached one somebody
// else runs, on which the harness creates and later deletes its own farm.
package target

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"maps"
	"net/http"
	"strings"

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

// PIDs returns the process IDs of the processes a managed target is running,
// the server first and then its real workers, and nil for any other target.
// Call it while the target runs: Close forgets the server it stopped.
func PIDs(t Target) []int {
	m, ok := t.(*managed)
	if !ok {
		return nil
	}
	var pids []int
	if pid := m.serverPID(); pid != 0 {
		pids = append(pids, pid)
	}
	return append(pids, m.workerPIDs()...)
}

// ProcessAlive reports whether a process with this id is still running. A
// process the harness started and has since reaped is not.
func ProcessAlive(pid int) bool { return processAlive(pid) }

// PostgresDSN returns the DSN of the PostgreSQL database a managed target's
// server uses (the one it created, when it was given an administrative DSN),
// and "" for SQLite or any other target.
func PostgresDSN(t Target) string {
	if m, ok := t.(*managed); ok {
		return m.pgDSN
	}
	return ""
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

// PoolName returns the name the server knows the scenario's usage pool name
// by: on an attached server the run's own namespaced pool (see
// [RunPoolName]), and name itself on any other target.
func PoolName(t Target, name string) string {
	if pn, ok := t.(interface{ PoolName(string) string }); ok && name != "" {
		return pn.PoolName(name)
	}
	return name
}

// maxPoolName is the longest usage pool name a job template can claim: OpenJD
// caps a capability name at 100 characters, and "amount.worker.usagepool."
// takes 24 of them.
const maxPoolName = 100 - len("amount.worker.usagepool.")

// poolHashLen is the length of the hash that stands in for a pool name too
// long to fit beside its run ID.
const poolHashLen = 8

// RunPoolName returns the usage pool an attached run creates for the
// scenario's pool name: load_<runID>_<name>, with every character outside
// letters, digits and '_' turned into '_' (a capability name is made of
// identifiers), at most 76 characters. A name that does not fit beside the
// run ID is shortened and suffixed with a hash of itself; the
// load_<runID>_ prefix is always kept, which is how cleanup finds a run's
// pools. Pools are global to the server, so a run claiming them by their
// scenario names would take slots from a production pool of the same name.
func RunPoolName(runID, name string) string {
	prefix := PoolPrefix(runID)
	n := identifier(name)
	avail := maxPoolName - len(prefix)
	if len(n) <= avail {
		return prefix + n
	}
	h := fnv.New32a()
	h.Write([]byte(name))
	sum := fmt.Sprintf("%08x", h.Sum32())
	if avail <= poolHashLen {
		return prefix + sum[:avail]
	}
	return prefix + n[:avail-poolHashLen-1] + "_" + sum
}

// PoolPrefix is the prefix every usage pool of run runID carries:
// load_<runID>_, with the run ID made an identifier.
func PoolPrefix(runID string) string { return "load_" + identifier(runID) + "_" }

// identifier returns s with every character outside letters, digits and '_'
// replaced by '_'.
func identifier(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// farm is what setupFarm created.
type farm struct {
	id       string
	queueIDs map[string]string
	// poolIDs are the usage pools this run created, by scenario name; a pool
	// that already existed on the server and was reused is left out.
	poolIDs map[string]string
}

// poolPolicy says how setupFarm names the scenario's usage pools and what it
// does when one already exists.
type poolPolicy struct {
	// name maps a scenario pool name to the server's.
	name func(string) string
	// reuse accepts a pool that already exists (the shards of one run share
	// theirs); otherwise an existing pool fails the setup.
	reuse bool
}

// setupFarm creates the run's farm, its queues and its usage pools. When it
// fails part way, the returned farm holds what was created, for teardown.
func setupFarm(ctx context.Context, c *client.Client, name string, spec scenario.FarmSpec, pools poolPolicy, logger *slog.Logger) (farm, error) {
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
		server := pools.name(p.Name)
		pid, err := c.CreateUsagePool(ctx, server, p.Limit)
		if isStatus(err, http.StatusConflict) {
			if !pools.reuse {
				return f, fmt.Errorf("target: usage pool %s already exists on the server; the run creates pools of its own and never claims slots from an existing one (a leftover of an earlier run with this run ID is removed by sqi-load cleanup)", server)
			}
			// Another shard of this run created it first.
			logger.InfoContext(ctx, "usage pool already created by another shard of the run; sharing it", slog.String("pool", server))
			continue
		}
		if err != nil {
			return f, fmt.Errorf("target: create usage pool %s: %w", server, err)
		}
		f.poolIDs[p.Name] = pid
	}
	return f, nil
}

// samePoolName is the pool policy of a managed target: a fresh server, whose
// pools take the scenario's names, and on which none can exist already.
var samePoolName = poolPolicy{name: func(n string) string { return n }}

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
