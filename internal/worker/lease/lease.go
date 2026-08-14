// SPDX-License-Identifier: AGPL-3.0-or-later

// Package lease implements the worker side of the work-lease protocol: it keeps
// exactly one outstanding lease request per queue and dispatches each assignment
// the server returns. The server gates capacity, so the worker simply runs what
// it is given. Replaces the JetStream pull loop.
package lease

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/uberware/sqi/internal/worker/protocol"
)

// Config tunes the lease loop.
type Config struct {
	QueueIDs       []string      // queues this worker serves (at least one)
	RequestTimeout time.Duration // long-poll request timeout; default 35s
	WorkerID       string        // included in each request
}

// Transport sends a lease request and returns the server's reply bytes.
type Transport interface {
	RequestLease(ctx context.Context, queueID string, data []byte, timeout time.Duration) ([]byte, error)
}

// Dispatcher executes one assignment.
type Dispatcher interface {
	Dispatch(ctx context.Context, m *protocol.AssignMsg) error
}

type request struct {
	WorkerID string `json:"worker_id"`
}

type reply struct {
	Assignments []json.RawMessage `json:"assignments"`
}

// Loop runs one lease goroutine per queue.
type Loop struct {
	transport Transport
	dispatch  Dispatcher
	cfg       Config
	logger    *slog.Logger
}

// New builds a Loop. A nil logger is replaced with a discard logger.
func New(t Transport, d Dispatcher, cfg Config, logger *slog.Logger) *Loop {
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 35 * time.Second
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Loop{transport: t, dispatch: d, cfg: cfg, logger: logger}
}

// Run blocks until ctx is canceled, running one request loop per queue.
func (l *Loop) Run(ctx context.Context) {
	queues := l.cfg.QueueIDs
	if len(queues) == 0 {
		queues = []string{""} // empty = server-side wildcard / default queue
	}
	done := make(chan struct{}, len(queues))
	for _, q := range queues {
		go func(queueID string) {
			defer func() { done <- struct{}{} }()
			l.runQueue(ctx, queueID)
		}(q)
	}
	for range queues {
		<-done
	}
}

// runQueue keeps one outstanding request for queueID, dispatching each batch.
func (l *Loop) runQueue(ctx context.Context, queueID string) {
	reqBytes, _ := json.Marshal(request{WorkerID: l.cfg.WorkerID}) //nolint:errcheck // simple struct, never fails
	for {
		if ctx.Err() != nil {
			return
		}
		data, err := l.transport.RequestLease(ctx, queueID, reqBytes, l.cfg.RequestTimeout)
		if err != nil {
			// No server / timeout / transient: brief backoff, then re-request.
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		var rep reply
		if err := json.Unmarshal(data, &rep); err != nil {
			l.logger.WarnContext(ctx, "lease: malformed reply", slog.Any("error", err))
			continue
		}
		for _, raw := range rep.Assignments {
			m, err := decodeAssignment(raw)
			if err != nil {
				l.logger.WarnContext(ctx, "lease: malformed assignment", slog.Any("error", err))
				continue
			}
			if err := l.dispatch.Dispatch(ctx, m); err != nil {
				l.logger.WarnContext(ctx, "lease: dispatch failed", slog.String("task_id", m.TaskID), slog.Any("error", err))
			}
		}
	}
}

// decodeAssignment unmarshals one assignment from a lease reply and verifies
// its protocol version matches this worker's. encoding/json silently ignores
// fields it does not recognize, so an unmarshal that merely "succeeds" is not
// proof the sender and receiver agree on the message shape — a newer server
// sending fields an older worker's protocol.AssignMsg has never heard of
// would decode cleanly and just drop them, quietly running the task as if
// those fields were absent. Rejecting on a version mismatch turns that into a
// loud, diagnosable failure instead of a silently wrong command line.
//
// A version mismatch means a partially upgraded farm (this worker's build is
// out of step with the server that leased it work), so the error is written
// for the operator who will read it, not just the caller: it names both the
// version the message carried and the version this worker expects.
func decodeAssignment(raw json.RawMessage) (*protocol.AssignMsg, error) {
	var m protocol.AssignMsg
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("lease: malformed assignment: %w", err)
	}
	if m.Version != protocol.ProtocolVersion {
		return nil, fmt.Errorf("lease: assignment %s has protocol version %q, this worker expects %q (server and worker are out of sync — upgrade one to match)",
			m.TaskID, m.Version, protocol.ProtocolVersion)
	}
	return &m, nil
}
