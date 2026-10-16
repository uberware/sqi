// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
)

func mkTaskLog(a store.TaskAttempt, natsSeq int64, data string) store.TaskLog {
	return store.TaskLog{
		ID: uuid.NewString(), TaskID: a.TaskID, AttemptID: a.ID, SeqNum: natsSeq, NATSSeq: natsSeq,
		At: time.Now().UTC().Truncate(time.Millisecond), ReceivedAt: time.Now().UTC(), Stream: store.LogStreamStdout, Data: data,
	}
}

func natsSeqs(logs []store.TaskLog) []int64 {
	out := make([]int64, 0, len(logs))
	for _, l := range logs {
		out = append(out, l.NATSSeq)
	}
	return out
}

func TestParity_CreateTaskLog(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedStepA(t, st, graphOpts{}, store.TaskStatusReady)
			a := storetest.Running(t, st, leaseReq(g.Tasks["a"][0]))
			want := mkTaskLog(a, 7, "frame 1 done\n")
			want.Stream = store.LogStreamStderr
			got, err := st.CreateTaskLog(t.Context(), want)
			if err != nil || got.ID != want.ID || got.TaskID != want.TaskID || got.AttemptID != want.AttemptID ||
				got.SeqNum != 7 || got.NATSSeq != 7 || !got.At.Equal(want.At) || got.Stream != store.LogStreamStderr ||
				got.Data != want.Data {
				t.Fatalf("CreateTaskLog = (%+v, %v), want %+v", got, err, want)
			}
			listed, err := st.ListTaskLogs(t.Context(), a.ID, 0, 10)
			if err != nil || len(listed) != 1 || listed[0].ID != want.ID || listed[0].Data != want.Data {
				t.Fatalf("ListTaskLogs after create = (%+v, %v), want the one chunk", listed, err)
			}
		})
	}
}

func TestParity_ListTaskLogs(t *testing.T) {
	cases := []struct {
		name  string
		after int64
		limit int
		want  []int64
	}{
		{"from the start", 0, 10, []int64{1, 2, 3, 5}},
		{"after a cursor", 2, 10, []int64{3, 5}},
		{"after a gap", 4, 10, []int64{5}},
		{"limited", 0, 2, []int64{1, 2}},
		{"limited after a cursor", 1, 2, []int64{2, 3}},
		{"past the end", 5, 10, []int64{}},
		{"zero limit uses the default", 0, 0, []int64{1, 2, 3, 5}},
	}
	for name, st := range newStores(t) {
		g := seedStepA(t, st, graphOpts{}, store.TaskStatusReady, store.TaskStatusReady)
		a := storetest.Running(t, st, leaseReq(g.Tasks["a"][0]))
		b := storetest.Running(t, st, leaseReq(g.Tasks["a"][1]))
		// Inserted out of order; b's chunks interleave with a's.
		for _, seq := range []int64{3, 1, 5, 2} {
			if _, err := st.CreateTaskLog(t.Context(), mkTaskLog(a, seq, "a")); err != nil {
				t.Fatalf("CreateTaskLog: %v", err)
			}
		}
		if _, err := st.CreateTaskLog(t.Context(), mkTaskLog(b, 4, "b")); err != nil {
			t.Fatalf("CreateTaskLog: %v", err)
		}
		for _, tc := range cases {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				got, err := st.ListTaskLogs(t.Context(), a.ID, tc.after, tc.limit)
				if err != nil || !slices.Equal(natsSeqs(got), tc.want) {
					t.Fatalf("ListTaskLogs(after %d, limit %d) = (%v, %v), want %v", tc.after, tc.limit, natsSeqs(got), err, tc.want)
				}
				for _, l := range got {
					if l.AttemptID != a.ID {
						t.Fatalf("chunk %+v belongs to another attempt", l)
					}
				}
			})
		}
		t.Run(name+"/unknown attempt", func(t *testing.T) {
			if got, err := st.ListTaskLogs(t.Context(), uuid.NewString(), 0, 10); err != nil || len(got) != 0 {
				t.Fatalf("unknown attempt = (%v, %v), want none", natsSeqs(got), err)
			}
		})
	}
}

func TestParity_CreateTaskLog_DuplicateChunkIsAConflict(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedStepA(t, st, graphOpts{}, store.TaskStatusReady)
			a := storetest.Running(t, st, leaseReq(g.Tasks["a"][0]))
			first := mkTaskLog(a, 1, "frame 1\n")
			if _, err := st.CreateTaskLog(t.Context(), first); err != nil {
				t.Fatal(err)
			}
			// A redelivery: same attempt and worker sequence, a new row ID
			// (ingest mints one per delivery) and a different NATS sequence.
			dup := mkTaskLog(a, 1, "frame 1\n")
			dup.NATSSeq = 99
			if _, err := st.CreateTaskLog(t.Context(), dup); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("duplicate CreateTaskLog = %v, want ErrConflict", err)
			}
			logs, err := st.ListTaskLogs(t.Context(), a.ID, 0, 10)
			if err != nil || len(logs) != 1 || logs[0].ID != first.ID {
				t.Fatalf("logs = %+v, %v; want only the first chunk", logs, err)
			}
		})
	}
}
