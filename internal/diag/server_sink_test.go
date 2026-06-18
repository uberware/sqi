// SPDX-License-Identifier: AGPL-3.0-or-later

package diag_test

import (
	"testing"

	"github.com/uberware/sqi/internal/diag"
	sqilog "github.com/uberware/sqi/internal/log"
)

func TestServerSink_AppendsWithComponentAndParsesTs(t *testing.T) {
	b := diag.NewBuffer(10, nil)
	sink := diag.NewServerSink(b)

	sink.Emit(sqilog.SinkRecord{
		Ts:    "2026-06-17T12:00:00.000000000Z",
		Level: "ERROR",
		Msg:   "boom",
		Attrs: map[string]string{"task_id": "t1"},
	})

	got := b.Query(diag.Filter{Component: "server", Limit: 10})
	if len(got) != 1 {
		t.Fatalf("want 1 record, got %d", len(got))
	}
	if got[0].Level != "ERROR" || got[0].Msg != "boom" || got[0].Attrs["task_id"] != "t1" {
		t.Fatalf("record = %+v", got[0])
	}
	if got[0].Ts.IsZero() {
		t.Fatalf("timestamp not parsed")
	}
}

func TestServerSink_BadTimestampFallsBackToNow(t *testing.T) {
	b := diag.NewBuffer(10, nil)
	sink := diag.NewServerSink(b)
	sink.Emit(sqilog.SinkRecord{Ts: "not-a-time", Level: "INFO", Msg: "x"})
	got := b.Query(diag.Filter{Component: "server", Limit: 10})
	if len(got) != 1 || got[0].Ts.IsZero() {
		t.Fatalf("expected fallback timestamp, got %+v", got)
	}
}
