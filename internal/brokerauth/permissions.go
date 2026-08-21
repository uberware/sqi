// SPDX-License-Identifier: AGPL-3.0-or-later

package brokerauth

import natsserver "github.com/nats-io/nats-server/v2/server"

// WorkerPermissions returns the NATS permissions for one worker.
//
// Every worker→server subject carries the worker's ID as a token precisely so
// that these permissions can bind it. That is not a stylistic choice: NATS
// permissions are static per credential, JetStream does not stamp publisher
// identity onto a message, and the pre-H1 subjects were keyed by job and task
// — so there was no way to express "only this worker's own traffic" at all.
func WorkerPermissions(workerID string) *natsserver.Permissions {
	return &natsserver.Permissions{
		Publish: &natsserver.SubjectPermission{
			Allow: []string{
				"task.status." + workerID + ".*",
				"task.logs." + workerID + ".*",
				"worker.register." + workerID,
				"worker.heartbeat." + workerID,
				"worker.deregister." + workerID,
				"worker.diag." + workerID,
				"work.lease." + workerID + ".*",
			},
		},
		Subscribe: &natsserver.SubjectPermission{
			Allow: []string{
				// nats.go allocates reply inboxes under _INBOX; a request/reply
				// lease cannot work without this.
				"_INBOX.>",

				// ACCEPTED GAP. Workers subscribe per-task at assignment time
				// (internal/worker/cancel/cancel.go), so a static permission
				// cannot be narrower than the whole subtree: any enrolled
				// worker can observe any task's cancel signals. Narrowing it
				// needs a permission reload per assignment, or NATS auth
				// callout. Low severity — a cancel message for a task the
				// worker does not hold is inert — and revisited in H14.
				"task.cancel.>",
			},
		},
	}
}

// ServerPermissions returns the permissions for sqi-server's own broker
// connections, which need the full subject space: the scheduler consumes every
// worker subject and publishes cancels, and the broker's admin connection
// provisions streams.
func ServerPermissions() *natsserver.Permissions {
	return &natsserver.Permissions{
		Publish:   &natsserver.SubjectPermission{Allow: []string{">"}},
		Subscribe: &natsserver.SubjectPermission{Allow: []string{">"}},
	}
}
