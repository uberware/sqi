// SPDX-License-Identifier: AGPL-3.0-or-later

package diaglog_test

import (
	"encoding/json"
	"sync"
	"testing"

	sqilog "github.com/uberware/sqi/internal/log"
	"github.com/uberware/sqi/internal/worker/diaglog"
	"github.com/uberware/sqi/internal/worker/protocol"
)

type fakePublisher struct {
	mu   sync.Mutex
	subj string
	data []byte
	err  error
}

func (f *fakePublisher) Publish(subj string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subj, f.data = subj, data
	return f.err
}

func TestPublisher_Emit_PublishesDiagLogMsg(t *testing.T) {
	fp := &fakePublisher{}
	p := diaglog.New(fp, "w1")

	p.Emit(sqilog.SinkRecord{
		Ts:    "2026-06-17T12:00:00.000000000Z",
		Level: "ERROR",
		Msg:   "boom",
		Attrs: map[string]string{"task_id": "t1"},
	})

	fp.mu.Lock()
	defer fp.mu.Unlock()
	if fp.subj != "worker.diag.w1" {
		t.Fatalf("subject = %q", fp.subj)
	}
	var msg protocol.DiagLogMsg
	if err := json.Unmarshal(fp.data, &msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg.Level != "ERROR" || msg.Msg != "boom" || msg.Attrs["task_id"] != "t1" {
		t.Fatalf("msg = %+v", msg)
	}
}

func TestPublisher_Emit_PublishErrorDropped(_ *testing.T) {
	fp := &fakePublisher{err: boomError{}}
	p := diaglog.New(fp, "w1")
	p.Emit(sqilog.SinkRecord{Ts: "2026-06-17T12:00:00.000000000Z", Level: "INFO", Msg: "x"})
	// Must not panic or block; error swallowed.
}

type boomError struct{}

func (boomError) Error() string { return "boom" }
