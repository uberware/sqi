// SPDX-License-Identifier: AGPL-3.0-or-later

package lease

import (
	"context"
	"encoding/json"
	"strings"
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

func TestLease_RejectsAssignmentWithWrongProtocolVersion(t *testing.T) {
	msg := protocol.AssignMsg{
		Version: "999",
		Type:    protocol.TypeAssign,
		TaskID:  "t1",
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	_, err = decodeAssignment(raw)
	if err == nil {
		t.Fatal("a wrong-version assignment was accepted")
	}
	if !strings.Contains(err.Error(), "999") || !strings.Contains(err.Error(), protocol.ProtocolVersion) {
		t.Errorf("error %q names neither the received nor the expected version", err)
	}
}

func TestLease_AcceptsCurrentProtocolVersion(t *testing.T) {
	msg := protocol.AssignMsg{
		Version: protocol.ProtocolVersion,
		Type:    protocol.TypeAssign,
		TaskID:  "t1",
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if _, err := decodeAssignment(raw); err != nil {
		t.Fatalf("a current-version assignment was rejected: %v", err)
	}
}

// TestLoop_SkipsWrongVersionAssignmentButDispatchesRest pins the batch
// semantics: a wrong-version message is dropped, but sibling assignments in
// the same reply still dispatch. A mixed-version batch cannot come from one
// server, so this is a defense-in-depth choice, not a scenario expected in
// practice.
func TestLoop_SkipsWrongVersionAssignmentButDispatchesRest(t *testing.T) {
	bad := protocol.AssignMsg{Version: "999", Type: protocol.TypeAssign, TaskID: "bad"}
	good := protocol.AssignMsg{Version: protocol.ProtocolVersion, Type: protocol.TypeAssign, TaskID: "good"}
	badJSON, _ := json.Marshal(bad)                                                    //nolint:errcheck // simple struct, never fails
	goodJSON, _ := json.Marshal(good)                                                  //nolint:errcheck // simple struct, never fails
	batch, _ := json.Marshal(reply{Assignments: []json.RawMessage{badJSON, goodJSON}}) //nolint:errcheck // simple struct, never fails

	tr := &fakeTransport{replies: [][]byte{batch}}
	d := &recDispatcher{}
	l := New(tr, d, Config{QueueIDs: []string{"q1"}, RequestTimeout: 50 * time.Millisecond}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	l.Run(ctx)

	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.got) != 1 || d.got[0] != "good" {
		t.Fatalf("dispatched = %v, want [good]", d.got)
	}
}
