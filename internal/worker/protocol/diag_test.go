// SPDX-License-Identifier: AGPL-3.0-or-later

package protocol_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/worker/protocol"
)

func TestDiagLogMsg_JSONRoundTrip(t *testing.T) {
	in := protocol.DiagLogMsg{
		Ts:    time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC),
		Level: "ERROR",
		Msg:   "executor: task process error",
		Attrs: map[string]string{"task_id": "t1", "attempt_id": "a1"},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out protocol.DiagLogMsg
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Level != in.Level || out.Msg != in.Msg || out.Attrs["task_id"] != "t1" {
		t.Fatalf("round trip mismatch: got %+v", out)
	}
	if !out.Ts.Equal(in.Ts) {
		t.Fatalf("ts mismatch: got %v want %v", out.Ts, in.Ts)
	}
}
