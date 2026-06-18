// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/diag"
	"github.com/uberware/sqi/internal/worker/protocol"
)

func TestHandleDiagMessage_AppendsWithWorkerComponent(t *testing.T) {
	buf := diag.NewBuffer(10, nil)
	s := &Scheduler{diagBuf: buf}

	msg := protocol.DiagLogMsg{
		Ts:    time.Now().UTC(),
		Level: "ERROR",
		Msg:   "boom",
		Attrs: map[string]string{"task_id": "t1"},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	s.handleDiagMessage("worker.diag.w1", data)

	got := buf.Query(diag.Filter{Component: "worker:w1", Limit: 10})
	if len(got) != 1 || got[0].Msg != "boom" || got[0].Attrs["task_id"] != "t1" {
		t.Fatalf("record = %+v", got)
	}
}

func TestHandleDiagMessage_IgnoresMalformed(t *testing.T) {
	buf := diag.NewBuffer(10, nil)
	s := &Scheduler{diagBuf: buf}
	s.handleDiagMessage("worker.diag.w1", []byte("not json"))
	if got := buf.Query(diag.Filter{Limit: 10}); len(got) != 0 {
		t.Fatalf("malformed message should be dropped: %+v", got)
	}
}

func TestHandleDiagMessage_NilBufferNoPanic(_ *testing.T) {
	s := &Scheduler{} // diagnostics disabled → diagBuf nil
	s.handleDiagMessage("worker.diag.w1", []byte(`{"msg":"x"}`))
}
