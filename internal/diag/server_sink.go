// SPDX-License-Identifier: AGPL-3.0-or-later

package diag

import (
	"time"

	sqilog "github.com/uberware/sqi/internal/log"
)

// ServerComponent is the component label for the sqi-server process's own logs.
const ServerComponent = "server"

// serverSink adapts a [sqilog.Sink] to append records to a [Buffer] under the
// fixed "server" component.  It performs no slog calls (loop-safe).
type serverSink struct {
	buf *Buffer
}

// NewServerSink returns a [sqilog.Sink] that appends sqi-server's own log
// records into buf under [ServerComponent].
func NewServerSink(buf *Buffer) sqilog.Sink {
	return &serverSink{buf: buf}
}

func (s *serverSink) Emit(r sqilog.SinkRecord) {
	ts, err := time.Parse("2006-01-02T15:04:05.000000000Z07:00", r.Ts)
	if err != nil {
		ts = time.Now().UTC()
	}
	s.buf.Append(Record{
		Ts:        ts,
		Component: ServerComponent,
		Level:     r.Level,
		Msg:       r.Msg,
		Attrs:     r.Attrs,
	})
}
