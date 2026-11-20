// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"errors"
	"log/slog"
	"testing"
)

// The formats below are nats-server v2.15.3's own (stream.go
// setWriteErrLocked, filestore.go fileStore.error, jetstream.go
// handleOutOfSpace). TestRecovery_RealDescriptorExhaustion pins them against
// the real server on Linux.
func TestParseBreak(t *testing.T) {
	tests := []struct {
		name   string
		format string
		v      []any
		want   streamBreak
		ok     bool
	}{
		{
			name:   "stream write error",
			format: "JetStream stream '%s > %s' critical write error: %v",
			v:      []any{"SQI", "SQI_TASK", errors.New("lmb missing")},
			want:   streamBreak{Stream: "SQI_TASK", Err: "lmb missing"}, ok: true,
		},
		{
			name:   "file store write error",
			format: "Filestore [SQI_WORKER] Critical write error: %v",
			v:      []any{errors.New("no space left on device")},
			want:   streamBreak{Stream: "SQI_WORKER", Err: "no space left on device"}, ok: true,
		},
		{
			name:   "jetstream out of resources",
			format: "JetStream out of %s resources, will be DISABLED",
			v:      []any{"file"},
			want:   streamBreak{Err: "JetStream out of file resources, will be DISABLED"}, ok: true,
		},
		{
			name:   "jetstream out of resources, no store type",
			format: "JetStream out of resources, will be DISABLED",
			want:   streamBreak{Err: "JetStream out of resources, will be DISABLED"}, ok: true,
		},
		{
			name:   "a failed store is not a break",
			format: "JetStream failed to store a msg on stream '%s > %s': %v",
			v:      []any{"SQI", "SQI_TASK", errors.New("lmb missing")},
		},
		{name: "slow consumer", format: "%s - Slow Consumer Detected: WriteDeadline of %v exceeded", v: []any{"cid:1", "10s"}},
		{name: "stream format with too few arguments", format: "JetStream stream '%s > %s' critical write error: %v", v: []any{"SQI"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseBreak(tt.format, tt.v)
			if ok != tt.ok || got != tt.want {
				t.Errorf("parseBreak = %+v, %v; want %+v, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestNATSLogger_ErrorfReportsBreakAtEveryLevel pins that detection does not
// depend on log.level: a server logging nothing at ERROR still recovers.
func TestNATSLogger_ErrorfReportsBreakAtEveryLevel(t *testing.T) {
	h := &recordHandler{floor: slog.LevelError + 4}
	l := newNATSLogger(slog.New(h))
	var got []streamBreak
	l.onBreak = func(b streamBreak) { got = append(got, b) }

	l.Errorf("JetStream stream '%s > %s' critical write error: %v", "SQI", "SQI_TASK", errors.New("lmb missing"))
	l.Errorf("JetStream failed to store a msg on stream '%s > %s': %v", "SQI", "SQI_TASK", errors.New("lmb missing"))

	if len(got) != 1 || got[0].Stream != "SQI_TASK" {
		t.Errorf("breaks = %+v, want one for SQI_TASK", got)
	}
	if n := len(h.records()); n != 0 {
		t.Errorf("logged %d records with ERROR disabled", n)
	}
}
