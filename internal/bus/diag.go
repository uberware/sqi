// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"

	nats "github.com/nats-io/nats.go"
)

// SubjectWorkerDiagPrefix is the prefix for worker diagnostic-log subjects.
// Full subject: SubjectWorkerDiagPrefix + "." + workerID.
//
// Unlike task/work/status subjects, diagnostic logs use CORE NATS (not
// JetStream): they are best-effort and never retained on the broker.  The
// server keeps a bounded in-memory ring buffer instead.
const SubjectWorkerDiagPrefix = "worker.diag"

// WorkerDiagSubject returns the full core-NATS subject a worker publishes its
// diagnostic log records to.
func WorkerDiagSubject(workerID string) string {
	return SubjectWorkerDiagPrefix + "." + workerID
}

// PublishWorkerDiag publishes a diagnostic log record to worker.diag.<workerID>
// over core NATS (fire-and-forget, no JetStream ack).  Errors are returned but
// callers should treat them as non-fatal: diagnostics are best-effort.
func (c *Client) PublishWorkerDiag(_ context.Context, workerID string, data []byte) error {
	return c.nc.Publish(WorkerDiagSubject(workerID), data)
}

// SubscribeWorkerDiag subscribes to all worker diagnostic subjects
// (worker.diag.>) over core NATS and invokes handler for each message with the
// concrete subject and raw payload.  The returned subscription must be
// unsubscribed/drained by the caller (the server does this at shutdown).
func (c *Client) SubscribeWorkerDiag(handler func(subject string, data []byte)) (*nats.Subscription, error) {
	return c.nc.Subscribe(SubjectWorkerDiagPrefix+".>", func(m *nats.Msg) {
		handler(m.Subject, m.Data)
	})
}
