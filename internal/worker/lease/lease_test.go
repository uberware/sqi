// SPDX-License-Identifier: AGPL-3.0-or-later

package lease

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/worker/protocol"
)

type fakeTransport struct {
	mu      sync.Mutex
	replies [][]byte
	calls   int
}

func (f *fakeTransport) RequestLease(_ context.Context, _ string, _ []byte, _ time.Duration) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(f.replies) == 0 {
		out, _ := json.Marshal(reply{}) //nolint:errcheck // simple struct, never fails
		return out, nil
	}
	r := f.replies[0]
	f.replies = f.replies[1:]
	return r, nil
}

type recDispatcher struct {
	mu  sync.Mutex
	got []string
}

func (d *recDispatcher) Dispatch(_ context.Context, m *protocol.AssignMsg) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.got = append(d.got, m.TaskID)
	return nil
}

func TestLoop_DispatchesLeasedBatch(t *testing.T) {
	asn := protocol.AssignMsg{Version: protocol.ProtocolVersion, Type: protocol.TypeAssign, TaskID: "t1"}
	asnJSON, _ := json.Marshal(asn)                                          //nolint:errcheck // simple struct, never fails
	batch, _ := json.Marshal(reply{Assignments: []json.RawMessage{asnJSON}}) //nolint:errcheck // simple struct, never fails

	tr := &fakeTransport{replies: [][]byte{batch}}
	d := &recDispatcher{}
	l := New(tr, d, Config{QueueIDs: []string{"q1"}, RequestTimeout: 50 * time.Millisecond}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	l.Run(ctx)

	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.got) != 1 || d.got[0] != "t1" {
		t.Fatalf("dispatched = %v, want [t1]", d.got)
	}
}
