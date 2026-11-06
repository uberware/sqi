// SPDX-License-Identifier: AGPL-3.0-or-later

// Package cancel receives the server's cancel signals for sqi-worker.
//
// When a job or task is canceled on sqi-server the scheduler publishes a
// [protocol.TaskCancelMsg] for each run that was assigned or running, to the
// worker holding it. [Subscribe] receives those messages and routes each to
// the executor via [TaskCanceler.Cancel].
//
// # Subject scheme
//
// The server publishes to task.cancel.<workerID>, the subject of the worker
// holding the task. The worker subscribes once, before it asks for work, so a
// cancel committed after any lease it takes finds the subscription in place.
// The message names the task and, when the server stops one stale attempt
// rather than the task, the attempt ([protocol.TaskCancelMsg.AttemptID]).
//
// # Durability
//
// A core subscription misses messages published while the worker is
// disconnected, and nothing on the worker reads the SQI_CANCEL stream's copy.
// The server repairs that from the worker's own reports: a "running" report
// for an attempt the server has closed is answered with a cancel naming it,
// and so is every heartbeat sweep while the worker's heartbeats still list
// the attempt. The same repairs cover a cancel that arrives between the lease
// reply and the executor recording the run, which finds no run to stop.
//
// # Usage
//
//	sub, err := cancel.Subscribe(nc, workerID, exec, logger)
package cancel

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	nats "github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// TaskCanceler is implemented by the executor to cancel a specific in-progress
// task.  [*executor.Executor] implements this interface.
type TaskCanceler interface {
	// Cancel requests cancellation of the task identified by taskID. A
	// non-empty attemptID limits it to that attempt: a run of another attempt
	// is left alone. An empty taskID with a non-empty attemptID stops that
	// attempt whatever its task (the server names no task for an attempt its
	// store has no record of). Returns true if a matching run was found and
	// cancellation was initiated, false otherwise (already finished, never
	// dispatched, or another attempt).
	Cancel(taskID, attemptID string) bool
}

// Applies reports whether a cancel naming taskID and attemptID applies to a run
// of attempt runAttemptID of task runTaskID: each name the cancel gives must
// match, and it must give at least one. [TaskCanceler] implementations use it
// to pick the runs a cancel stops.
func Applies(taskID, attemptID, runTaskID, runAttemptID string) bool {
	if taskID == "" && attemptID == "" {
		return false
	}
	return (taskID == "" || taskID == runTaskID) && (attemptID == "" || attemptID == runAttemptID)
}

// natsConn is the subset of [*nats.Conn] Subscribe uses. Defined as an
// interface so unit tests can inject a stub without a live NATS server.
type natsConn interface {
	Subscribe(subj string, cb nats.MsgHandler) (*nats.Subscription, error)
	Flush() error
}

// Subscribe subscribes to workerID's task.cancel.<workerID> and delivers each
// cancel to canceler. It returns once the server has the subscription, so a
// worker that calls it before its first lease request receives every cancel
// for work it is leased while connected. The subscription lasts until it is
// unsubscribed or nc is closed, and nats.go restores it across reconnects.
func Subscribe(nc natsConn, workerID string, canceler TaskCanceler, logger *slog.Logger) (*nats.Subscription, error) {
	subj := bus.TaskCancelSubject(workerID)
	sub, err := nc.Subscribe(subj, func(m *nats.Msg) { handleCancel(m, canceler, logger) })
	if err != nil {
		return nil, fmt.Errorf("cancel: subscribe %q: %w", subj, err)
	}
	if err := nc.Flush(); err != nil {
		if uerr := sub.Unsubscribe(); uerr != nil {
			logger.WarnContext(context.Background(), "cancel: unsubscribe after failed flush", slog.Any("error", uerr))
		}
		return nil, fmt.Errorf("cancel: flush subscription %q: %w", subj, err)
	}
	return sub, nil
}

// handleCancel is the NATS message callback for task.cancel.<workerID>.
//
// It unmarshals the cancel payload, calls canceler for the task and attempt it
// names, and logs the outcome. A message that cannot be parsed, or names
// neither a task nor an attempt, is logged and ignored; a run already finished
// is ignored quietly.
func handleCancel(m *nats.Msg, canceler TaskCanceler, logger *slog.Logger) {
	bg := context.Background()
	var msg protocol.TaskCancelMsg
	if err := json.Unmarshal(m.Data, &msg); err != nil || (msg.TaskID == "" && msg.AttemptID == "") {
		logger.WarnContext(
			bg,
			"cancel: malformed cancel message — ignoring",
			slog.String("subject", m.Subject),
			slog.Any("error", err),
		)
		return
	}

	if canceler.Cancel(msg.TaskID, msg.AttemptID) {
		logger.InfoContext(
			bg,
			"cancel: task cancellation initiated",
			slog.String("task_id", msg.TaskID),
			slog.String("attempt_id", msg.AttemptID),
			slog.String("job_id", msg.JobID),
		)
		return
	}
	// Already finished, never dispatched here, or another attempt.
	logger.DebugContext(
		bg,
		"cancel: no matching run — ignoring cancel signal",
		slog.String("task_id", msg.TaskID),
		slog.String("attempt_id", msg.AttemptID),
		slog.String("job_id", msg.JobID),
	)
}
