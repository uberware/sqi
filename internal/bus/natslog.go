// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"fmt"
	"log/slog"
)

// natsLogger is the embedded NATS server's logger. It logs a fatal error
// through slog and records the first one, so [Broker.Start] can fail with it:
// nats-server reports a listener or JetStream failure only through its logger,
// and otherwise just never becomes ready. Notices, warnings, errors, debug and
// trace output are dropped; the broker logs its own lifecycle events.
type natsLogger struct {
	log   *slog.Logger
	fatal chan error // the first fatal error; buffered, never blocks the server
}

func newNATSLogger(log *slog.Logger) *natsLogger {
	return &natsLogger{log: log, fatal: make(chan error, 1)}
}

func (l *natsLogger) Fatalf(format string, v ...any) {
	err := fmt.Errorf(format, v...) //nolint:err113 // nats-server's own message
	l.log.ErrorContext(context.Background(), "bus: nats server fatal error", slog.Any("error", err))
	select {
	case l.fatal <- err:
	default:
	}
}

func (*natsLogger) Noticef(string, ...any) {}

func (*natsLogger) Warnf(string, ...any) {}

func (*natsLogger) Errorf(string, ...any) {}

func (*natsLogger) Debugf(string, ...any) {}

func (*natsLogger) Tracef(string, ...any) {}
