// SPDX-License-Identifier: AGPL-3.0-or-later

// Package bus provides the embedded NATS JetStream message broker and the
// subject/stream definitions used for sqi-server's internal messaging.
//
// # Subject naming
//
// Each subject class follows a hierarchical naming scheme so that JetStream
// stream subject-filters and consumer subject-bindings are intuitive:
//
//	work.lease.<worker>.<queue> — worker → server: request/reply work-lease batch
//	task.status.<worker>.<job>  — worker → server: task state transitions
//	task.logs.<worker>.<task>   — worker → server: log chunk ingestion
//	worker.heartbeat.<worker>   — worker → server: periodic liveness pings
//	worker.register.<worker>    — worker → server: capability advertisement
//	worker.deregister.<worker>  — worker → server: graceful departure notification
//	task.cancel.<task>          — server → worker: interrupt a running task
//
// Every worker → server subject carries the publishing worker's ID directly
// after its class prefix. That placement is what makes the broker's per-worker
// publish permissions expressible: NATS permissions are static per credential
// and JetStream does not stamp publisher identity onto a message, so a scheme
// keyed only by job, task and queue would offer no way to say "only this
// worker's own traffic" — and no way for the server to learn who published a
// message it received. [ParseWorkerSubject] recovers the identity on the
// receiving side.
//
// The remaining tokens (<queue>, <job>, <task>) are the opaque string
// identifiers of the corresponding entity in the SQLite store. Callers build
// full subject strings using the helper functions below rather than
// constructing them by hand.
package bus

import "strings"

// Subject prefix constants.
const (
	// SubjectTaskStatusPrefix is the prefix for task-status subjects.
	// Full subject: SubjectTaskStatusPrefix + "." + workerID + "." + jobID.
	SubjectTaskStatusPrefix = "task.status"

	// SubjectTaskLogsPrefix is the prefix for task-log subjects.
	// Full subject: SubjectTaskLogsPrefix + "." + workerID + "." + taskID.
	SubjectTaskLogsPrefix = "task.logs"

	// SubjectTaskCancelPrefix is the prefix for task-cancellation subjects.
	// Full subject: SubjectTaskCancelPrefix + "." + taskID.
	// The server publishes to this subject; the worker assigned to the task
	// consumes it and interrupts the running process. This is the one subject
	// class that travels server → worker, so it carries no worker token.
	SubjectTaskCancelPrefix = "task.cancel"

	// SubjectWorkerHeartbeatPrefix is the prefix for worker liveness pings.
	// Full subject: SubjectWorkerHeartbeatPrefix + "." + workerID.
	SubjectWorkerHeartbeatPrefix = "worker.heartbeat"

	// SubjectWorkerRegisterPrefix is the prefix workers publish registration
	// messages under when they first connect or reconnect.
	// Full subject: SubjectWorkerRegisterPrefix + "." + workerID.
	SubjectWorkerRegisterPrefix = "worker.register"

	// SubjectWorkerDeregisterPrefix is the prefix workers publish under on
	// graceful shutdown so the server can mark the worker offline immediately
	// rather than waiting for heartbeat timeout. The server handler for these
	// subjects calls [store.WorkerStore.UpdateWorkerStatus] with
	// WorkerStatusOffline.
	// Full subject: SubjectWorkerDeregisterPrefix + "." + workerID.
	SubjectWorkerDeregisterPrefix = "worker.deregister"

	// SubjectWorkLeasePrefix is the prefix for worker work-lease requests.
	// Full subject: SubjectWorkLeasePrefix + "." + workerID + "." + queueID.
	// Core NATS request/reply — workers ask for work; the server replies with
	// a batch.
	SubjectWorkLeasePrefix = "work.lease"

	// WildcardQueueToken is the lease-subject queue token a queue-unaffiliated
	// worker (empty QueueIDs — "serve any queue") uses in place of a real queue
	// ID, so it requests on a valid subject (work.lease.<worker>._any) that the
	// server's work.lease.> subscription actually receives. An empty token would
	// produce an unroutable subject. The token is reserved (underscore prefix)
	// so it cannot collide with a real UUID queue ID; the server selects tasks
	// farm-wide and gates by worker eligibility, so the token's only role is
	// subject routing and wake-up bucketing.
	WildcardQueueToken = "_any"
)

// TaskStatusSubject returns the full NATS subject for task-status messages
// published by the given worker about the given job.
func TaskStatusSubject(workerID, jobID string) string {
	return SubjectTaskStatusPrefix + "." + workerID + "." + jobID
}

// TaskLogsSubject returns the full NATS subject for log-chunk messages
// published by the given worker for the given task.
func TaskLogsSubject(workerID, taskID string) string {
	return SubjectTaskLogsPrefix + "." + workerID + "." + taskID
}

// TaskCancelSubject returns the full NATS subject for a task-cancellation
// signal targeting the worker that holds the given task.
func TaskCancelSubject(taskID string) string {
	return SubjectTaskCancelPrefix + "." + taskID
}

// WorkerRegisterSubject returns the full NATS subject the given worker
// publishes its capability advertisement to.
func WorkerRegisterSubject(workerID string) string {
	return SubjectWorkerRegisterPrefix + "." + workerID
}

// WorkerHeartbeatSubject returns the full NATS subject the given worker
// publishes its liveness pings to.
func WorkerHeartbeatSubject(workerID string) string {
	return SubjectWorkerHeartbeatPrefix + "." + workerID
}

// WorkerDeregisterSubject returns the full NATS subject the given worker
// publishes its graceful-departure notification to.
func WorkerDeregisterSubject(workerID string) string {
	return SubjectWorkerDeregisterPrefix + "." + workerID
}

// WorkLeaseSubject returns the full NATS subject the given worker requests
// work on for the given queue.
func WorkLeaseSubject(workerID, queueID string) string {
	return SubjectWorkLeasePrefix + "." + workerID + "." + queueID
}

// ParseWorkerSubject splits a worker → server subject into the ID of the worker
// that published it and the trailing token identifying the entity it concerns
// (the job for task status, the task for logs, the queue for a lease request;
// empty for the three worker-lifecycle subjects, which have no trailing token).
//
// ok is false for anything that is not one of the six worker → server subject
// shapes, including subjects with an empty worker or trailing token. A message
// whose subject does not carry an identity is one the server cannot attribute
// to a worker, and callers must reject it rather than act on a partial parse.
func ParseWorkerSubject(subject string) (workerID, leaf string, ok bool) {
	tokens := strings.Split(subject, ".")
	if len(tokens) < 3 || len(tokens) > 4 {
		return "", "", false
	}
	prefix := tokens[0] + "." + tokens[1]

	if len(tokens) == 3 {
		switch prefix {
		case SubjectWorkerRegisterPrefix, SubjectWorkerHeartbeatPrefix, SubjectWorkerDeregisterPrefix:
			if tokens[2] == "" {
				return "", "", false
			}
			return tokens[2], "", true
		default:
			return "", "", false
		}
	}

	switch prefix {
	case SubjectTaskStatusPrefix, SubjectTaskLogsPrefix, SubjectWorkLeasePrefix:
		if tokens[2] == "" || tokens[3] == "" {
			return "", "", false
		}
		return tokens[2], tokens[3], true
	default:
		return "", "", false
	}
}
