// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

func TestIsWrongRevision(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"key exists", jetstream.ErrKeyExists, true},
		{"wrong last sequence", &jetstream.APIError{Code: 400, ErrorCode: jsErrWrongLastSequence}, true},
		{"write in flight on a replicated bucket", &jetstream.APIError{Code: 400, ErrorCode: jsErrWrongLastSequenceInFlight}, true},
		{"wrapped", errors.Join(errors.New("ctx"), &jetstream.APIError{Code: 400, ErrorCode: jsErrWrongLastSequenceInFlight}), true},
		{"other API error", &jetstream.APIError{Code: 503, ErrorCode: 10008}, false},
		{"other error", errors.New("boom"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isWrongRevision(tt.err); got != tt.want {
				t.Errorf("isWrongRevision(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// scriptedKV answers the guard's Delete and Get from scripts, one step per
// call, and counts the deletes.
type scriptedKV struct {
	jetstream.KeyValue

	deletes []error
	updates []error
	gets    []jetstream.KeyValueEntry
	deleted int
}

func (kv *scriptedKV) Delete(context.Context, string, ...jetstream.KVDeleteOpt) error {
	kv.deleted++
	if len(kv.deletes) == 0 {
		return nil
	}
	err := kv.deletes[0]
	kv.deletes = kv.deletes[1:]
	return err
}

func (kv *scriptedKV) Update(context.Context, string, []byte, uint64) (uint64, error) {
	if len(kv.updates) == 0 {
		return 0, errors.New("unscripted update")
	}
	err := kv.updates[0]
	kv.updates = kv.updates[1:]
	return 0, err
}

func (kv *scriptedKV) Get(context.Context, string) (jetstream.KeyValueEntry, error) {
	if len(kv.gets) == 0 {
		return nil, jetstream.ErrKeyNotFound
	}
	e := kv.gets[0]
	kv.gets = kv.gets[1:]
	if e == nil {
		return nil, jetstream.ErrKeyNotFound
	}
	return e, nil
}

type guardEntry struct {
	jetstream.KeyValueEntry

	value []byte
	rev   uint64
}

func (e guardEntry) Value() []byte    { return e.value }
func (e guardEntry) Revision() uint64 { return e.rev }

func holderValue(t *testing.T, instanceID string) []byte {
	t.Helper()
	b, err := json.Marshal(GuardHolder{InstanceID: instanceID})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestGuard_ReleaseDeletesOwnKeyAfterAnInFlightRefresh pins the release of a
// guard whose last refresh was aborted by the release but is still being
// applied on a replicated bucket. The delete at the recorded revision is
// refused as a write in flight; a read made before the refresh lands still
// shows the recorded revision, and a delete at it is refused again; once the
// refresh lands, the key is deleted at its new revision.
func TestGuard_ReleaseDeletesOwnKeyAfterAnInFlightRefresh(t *testing.T) {
	inFlight := &jetstream.APIError{Code: 400, ErrorCode: jsErrWrongLastSequenceInFlight, Description: "wrong last sequence"}
	wrongRev := &jetstream.APIError{Code: 400, ErrorCode: jsErrWrongLastSequence, Description: "wrong last sequence: 6"}
	const id = "self"
	own := holderValue(t, id)
	kv := &scriptedKV{
		deletes: []error{inFlight, wrongRev, nil},
		gets:    []jetstream.KeyValueEntry{guardEntry{value: own, rev: 5}, guardEntry{value: own, rev: 6}},
	}
	g := &Guard{kv: kv, holder: GuardHolder{InstanceID: id}, revision: 5}
	if err := g.deleteOwnKey(context.Background()); err != nil {
		t.Fatalf("deleteOwnKey: %v", err)
	}
	if kv.deleted != 3 {
		t.Errorf("deletes = %d, want 3 (refused in flight, refused at the stale read, then done)", kv.deleted)
	}
}

// TestGuard_ReleaseLeavesAnotherHoldersKey pins that the release's retry
// never deletes a key another server now holds, nor one already gone.
func TestGuard_ReleaseLeavesAnotherHoldersKey(t *testing.T) {
	inFlight := &jetstream.APIError{Code: 400, ErrorCode: jsErrWrongLastSequenceInFlight}
	tests := []struct {
		name  string
		entry jetstream.KeyValueEntry
	}{
		{"another holder", guardEntry{value: holderValue(t, "other"), rev: 6}},
		{"gone", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kv := &scriptedKV{deletes: []error{inFlight}, gets: []jetstream.KeyValueEntry{tt.entry}}
			g := &Guard{kv: kv, holder: GuardHolder{InstanceID: "self"}, revision: 5}
			if err := g.deleteOwnKey(context.Background()); err != nil {
				t.Fatalf("deleteOwnKey: %v", err)
			}
			if kv.deleted != 1 {
				t.Errorf("deletes = %d, want only the first", kv.deleted)
			}
		})
	}
}

// TestGuard_RefreshRefusedInFlightIsNotARefresh pins that a refresh the
// server refuses because another write to the key is still in flight does
// not count as one: the key still reads at the guard's revision, and the
// write in flight may be another server taking it over, so lastOK must not
// move (Healthy goes on aging) until a later refresh succeeds or finds the
// loss.
func TestGuard_RefreshRefusedInFlightIsNotARefresh(t *testing.T) {
	const id = "self"
	own := holderValue(t, id)
	kv := &scriptedKV{
		updates: []error{&jetstream.APIError{Code: 400, ErrorCode: jsErrWrongLastSequenceInFlight}},
		gets:    []jetstream.KeyValueEntry{guardEntry{value: own, rev: 5}},
	}
	before := time.Now().Add(-time.Minute)
	g := &Guard{kv: kv, holder: GuardHolder{InstanceID: id}, revision: 5, lastOK: before, logger: slog.New(slog.DiscardHandler)}
	if lost := g.refreshOnce(context.Background()); lost {
		t.Fatal("refreshOnce reported the guard lost")
	}
	if !g.lastOK.Equal(before) || g.revision != 5 {
		t.Errorf("after a refused refresh: lastOK moved %s, revision %d; want unchanged at 5", g.lastOK.Sub(before), g.revision)
	}
}
