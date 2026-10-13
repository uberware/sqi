// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore

import (
	"errors"
	"testing"
)

func TestLockOrderCheck(t *testing.T) {
	tests := []struct {
		name    string
		calls   [][]Anchor // the transaction's lockAnchors calls, in order
		wantErr bool
	}{
		{"global order", [][]Anchor{{
			sharedAnchor(workerAnchor("w")), sharedAnchor(jobAnchor("j")),
			queueAnchor("q"), farmAnchor("f"), poolAnchor("a"), poolAnchor("b"),
		}}, false},
		{"a worker after a job", [][]Anchor{{jobAnchor("j"), workerAnchor("w")}}, true},
		{"a worker after an earlier call's job", [][]Anchor{{jobAnchor("j")}, {workerAnchor("w")}}, true},
		{"jobs out of id order", [][]Anchor{{jobAnchor("b"), jobAnchor("a")}}, true},
		{"jobs out of id order across calls", [][]Anchor{{jobAnchor("b")}, {jobAnchor("a")}}, true},
		{"jobs out of id order, not adjacent", [][]Anchor{{jobAnchor("b"), sharedAnchor(jobAnchor("c")), jobAnchor("a")}}, true},
		{"jobs in id order across calls", [][]Anchor{{jobAnchor("a")}, {jobAnchor("b")}}, false},
		{"own job, then a lower-id upstream shared", [][]Anchor{{jobAnchor("b")}, {sharedAnchor(jobAnchor("a"))}}, false},
		{"the same row twice", [][]Anchor{{jobAnchor("a")}, {jobAnchor("a")}}, false},
		{"not an anchor table", [][]Anchor{{{Table: "tasks", ID: "t"}}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := newLockOrder()
			var err error
			for _, call := range tt.calls {
				if err = o.check(call); err != nil {
					break
				}
			}
			if got := errors.Is(err, errLockOrder); got != tt.wantErr {
				t.Fatalf("check = %v; want an errLockOrder: %v", err, tt.wantErr)
			}
		})
	}
}

// TestDBTxLockAnchors_ChecksBeforeLocking pins that lockAnchors refuses an
// anchor out of order before the dialect is asked to lock anything (the nil
// dialect would panic if it were).
func TestDBTxLockAnchors_ChecksBeforeLocking(t *testing.T) {
	tx := newDBTx(nil, nil)
	if err := tx.order.check([]Anchor{jobAnchor("j")}); err != nil {
		t.Fatal(err)
	}
	if err := tx.lockAnchors(t.Context(), workerAnchor("w")); !errors.Is(err, errLockOrder) {
		t.Fatalf("lockAnchors = %v; want errLockOrder", err)
	}
}
