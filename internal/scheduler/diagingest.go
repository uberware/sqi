// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Diagnostic-log ingestion: decodes worker.diag.<id> core-NATS messages into
// the server's in-memory diagnostic ring buffer.  No persistence, no ack — this
// is best-effort recent-history telemetry.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/diag"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// StartDiag starts the worker.diag.> subscription that feeds the diagnostic
// buffer, ahead of [Scheduler.Run]. A standby server calls it so that it
// collects worker diagnostics while it waits, and has their recent history
// already when it becomes the leader. Run starts the subscription itself when
// StartDiag has not, so a server without HA need not call it. It is a no-op
// when diagnostics are disabled (diagBuf nil) or the subscription is already
// running; after an error, the next StartDiag or Run tries again.
func (s *Scheduler) StartDiag() error {
	return s.startDiag(context.Background())
}

// startDiag is [Scheduler.StartDiag], logging under ctx.
func (s *Scheduler) startDiag(ctx context.Context) error {
	if s.diagBuf == nil {
		return nil
	}
	s.diagMu.Lock()
	defer s.diagMu.Unlock()
	if s.diagStarted {
		return nil
	}
	sub, err := s.bus.SubscribeWorkerDiag(s.handleDiagMessage)
	if err != nil {
		return fmt.Errorf("scheduler: start diagnostic-log consumer: %w", err)
	}
	s.diagSub, s.diagStarted = sub, true
	s.logger.InfoContext(ctx, "scheduler: diagnostic-log consumer started")
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
			// Not s.ctx: this subscription can start before Run sets it.
			s.logger.DebugContext(context.Background(), "scheduler: malformed worker.diag message", slog.Any("error", err))
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
