// SPDX-License-Identifier: AGPL-3.0-or-later

// Package diaglog implements the sqi-worker diagnostic-log sink.  It adapts a
// [sqilog.Sink] to publish each of the worker's own slog records as a
// [protocol.DiagLogMsg] on the core-NATS subject worker.diag.<workerID>.
//
// All publishing is best-effort: marshal or publish errors are dropped.  The
// sink performs NO slog calls — doing so would re-enter the fan-out handler and
// loop.
package diaglog

import (
	"encoding/json"
	"time"

	"github.com/uberware/sqi/internal/bus"
	sqilog "github.com/uberware/sqi/internal/log"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// natsPublisher is the minimal subset of *nats.Conn the Publisher uses.
type natsPublisher interface {
	Publish(subject string, data []byte) error
}

// Publisher is a [sqilog.Sink] that ships worker diagnostic logs over NATS.
type Publisher struct {
	nc      natsPublisher
	subject string
}

// New returns a Publisher that publishes to worker.diag.<workerID>.
func New(nc natsPublisher, workerID string) *Publisher {
	return &Publisher{nc: nc, subject: bus.WorkerDiagSubject(workerID)}
}

// Emit serializes r and publishes it.  Errors are intentionally dropped.
func (p *Publisher) Emit(r sqilog.SinkRecord) {
	ts, err := time.Parse("2006-01-02T15:04:05.000000000Z07:00", r.Ts)
	if err != nil {
		ts = time.Now().UTC()
	}
	data, err := json.Marshal(protocol.DiagLogMsg{
		Ts:    ts,
		Level: r.Level,
		Msg:   r.Msg,
		Attrs: r.Attrs,
	})
	if err != nil {
		return
	}
	_ = p.nc.Publish(p.subject, data)
}
