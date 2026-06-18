// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Diagnostic-log ingestion: decodes worker.diag.<id> core-NATS messages into
// the server's in-memory diagnostic ring buffer.  No persistence, no ack — this
// is best-effort recent-history telemetry (design transport choice A1).

import (
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/diag"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// startDiagConsumer subscribes to worker.diag.> and feeds the buffer.  It is a
// no-op when diagnostics are disabled (diagBuf nil).  Called once from Run.
func (s *Scheduler) startDiagConsumer() error {
	if s.diagBuf == nil {
		return nil
	}
	sub, err := s.bus.SubscribeWorkerDiag(s.handleDiagMessage)
	if err != nil {
		return err
	}
	s.diagSub = sub
	return nil
}

// handleDiagMessage decodes a worker diagnostic record and appends it to the
// buffer under the "worker:<id>" component derived from the subject.
func (s *Scheduler) handleDiagMessage(subject string, data []byte) {
	if s.diagBuf == nil {
		return
	}
	var m protocol.DiagLogMsg
	if err := json.Unmarshal(data, &m); err != nil {
		if s.logger != nil {
			s.logger.DebugContext(s.ctx, "scheduler: malformed worker.diag message", slog.Any("error", err))
		}
		return
	}
	workerID := strings.TrimPrefix(subject, bus.SubjectWorkerDiagPrefix+".")
	s.diagBuf.Append(diag.Record{
		Ts:        m.Ts,
		Component: "worker:" + workerID,
		Level:     m.Level,
		Msg:       m.Msg,
		Attrs:     m.Attrs,
	})
}
