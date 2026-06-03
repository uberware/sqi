// SPDX-License-Identifier: AGPL-3.0-only

// Package bus provides the embedded NATS JetStream message broker and the
// subject/stream definitions used for sqi-server's internal messaging.
//
// # Subject naming
//
// Each subject class follows a hierarchical naming scheme so that JetStream
// stream subject-filters and consumer subject-bindings are intuitive:
//
//	work.assign.<queue>    — server → worker(s): task assignment payload
//	task.status.<job>      — worker → server: task state transitions
//	task.logs.<task>       — worker → server: log chunk ingestion
//	worker.heartbeat       — worker → server: periodic liveness pings
//	worker.register        — worker → server: capability advertisement
//	worker.deregister      — worker → server: graceful departure notification
//
// The leaf token (<queue>, <job>, <task>) is the opaque string identifier of
// the corresponding entity in the SQLite store.  Callers build full subject
// strings using the helper functions below rather than constructing them by
// hand.
package bus

// Subject prefix and fixed-subject constants.
const (
	// SubjectWorkAssignPrefix is the prefix for task-assignment subjects.
	// Full subject: SubjectWorkAssignPrefix + "." + queueID.
	SubjectWorkAssignPrefix = "work.assign"

	// SubjectTaskStatusPrefix is the prefix for task-status subjects.
	// Full subject: SubjectTaskStatusPrefix + "." + jobID.
	SubjectTaskStatusPrefix = "task.status"

	// SubjectTaskLogsPrefix is the prefix for task-log subjects.
	// Full subject: SubjectTaskLogsPrefix + "." + taskID.
	SubjectTaskLogsPrefix = "task.logs"

	// SubjectTaskCancelPrefix is the prefix for task-cancellation subjects.
	// Full subject: SubjectTaskCancelPrefix + "." + taskID.
	// The server publishes to this subject; the worker assigned to the task
	// consumes it and interrupts the running process.
	SubjectTaskCancelPrefix = "task.cancel"

	// SubjectWorkerHeartbeat is the subject workers publish liveness pings to.
	SubjectWorkerHeartbeat = "worker.heartbeat"

	// SubjectWorkerRegister is the subject workers publish registration
	// messages to when they first connect or reconnect.
	SubjectWorkerRegister = "worker.register"

	// SubjectWorkerDeregister is the subject workers publish to on graceful
	// shutdown so the server can mark the worker offline immediately rather
	// than waiting for heartbeat timeout. The server handler for this subject
	// calls [store.WorkerStore.UpdateWorkerStatus] with WorkerStatusOffline.
	SubjectWorkerDeregister = "worker.deregister"
)

// WorkAssignSubject returns the full NATS subject for task-assignment messages
// targeting workers subscribed to the given queue.
func WorkAssignSubject(queueID string) string {
	return SubjectWorkAssignPrefix + "." + queueID
}

// TaskStatusSubject returns the full NATS subject for task-status messages
// belonging to the given job.
func TaskStatusSubject(jobID string) string {
	return SubjectTaskStatusPrefix + "." + jobID
}

// TaskLogsSubject returns the full NATS subject for log-chunk messages
// belonging to the given task.
func TaskLogsSubject(taskID string) string {
	return SubjectTaskLogsPrefix + "." + taskID
}

// TaskCancelSubject returns the full NATS subject for a task-cancellation
// signal targeting the worker that holds the given task.
func TaskCancelSubject(taskID string) string {
	return SubjectTaskCancelPrefix + "." + taskID
}
