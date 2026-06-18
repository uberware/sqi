// SPDX-License-Identifier: AGPL-3.0-or-later

package log_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	sqilog "github.com/uberware/sqi/internal/log"
)

type captureSink struct {
	mu   sync.Mutex
	recs []sqilog.SinkRecord
}

func (s *captureSink) Emit(r sqilog.SinkRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = append(s.recs, r)
}

func TestNewWithSink_WritesStderrAndSink(t *testing.T) {
	var buf bytes.Buffer
	sink := &captureSink{}
	logger, err := sqilog.NewWithSink("info", "json", &buf, sink)
	if err != nil {
		t.Fatalf("NewWithSink: %v", err)
	}

	logger.With(slog.String("task_id", "t1")).
		ErrorContext(context.Background(), "boom", slog.String("attempt_id", "a1"))

	if !strings.Contains(buf.String(), "boom") {
		t.Fatalf("stderr output missing message: %q", buf.String())
	}
	if len(sink.recs) != 1 {
		t.Fatalf("sink got %d records, want 1", len(sink.recs))
	}
	got := sink.recs[0]
	if got.Level != "ERROR" || got.Msg != "boom" {
		t.Fatalf("sink record = %+v", got)
	}
	if got.Attrs["task_id"] != "t1" || got.Attrs["attempt_id"] != "a1" {
		t.Fatalf("sink attrs = %+v (want With + inline attrs merged)", got.Attrs)
	}
}

func TestNewWithSink_RespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	sink := &captureSink{}
	logger, err := sqilog.NewWithSink("warn", "json", &buf, sink)
	if err != nil {
		t.Fatalf("NewWithSink: %v", err)
	}

	logger.InfoContext(context.Background(), "ignored")

	if len(sink.recs) != 0 {
		t.Fatalf("info record reached sink under warn level: %+v", sink.recs)
	}
}

func TestNewWithSink_NilSinkBehavesLikeNew(t *testing.T) {
	var buf bytes.Buffer
	logger, err := sqilog.NewWithSink("info", "text", &buf, nil)
	if err != nil {
		t.Fatalf("NewWithSink nil sink: %v", err)
	}
	logger.InfoContext(context.Background(), "hello")
	if !strings.Contains(buf.String(), "hello") {
		t.Fatalf("stderr output missing: %q", buf.String())
	}
}
