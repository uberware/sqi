// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// TestBuildAssignPayload_ExportedSeamMatchesUnexported pins the exported
// wrapper to the unexported implementation byte for byte. internal/presettest
// calls the exported one; if the two ever diverge, every preset golden in the
// repo becomes a snapshot of something no worker runs.
func TestBuildAssignPayload_ExportedSeamMatchesUnexported(t *testing.T) {
	st := fake.New()
	task, worker, job, step, queue := buildFixture(t, minimalJobJSON, store.TemplateFormatJSON, "Render")
	attemptID := uuid.NewString()

	want, err := buildAssignPayload(t.Context(), task, worker, job, step, queue, attemptID, st)
	if err != nil {
		t.Fatalf("buildAssignPayload: %v", err)
	}
	got, err := BuildAssignPayload(t.Context(), task, worker, job, step, queue, attemptID, st)
	if err != nil {
		t.Fatalf("BuildAssignPayload: %v", err)
	}

	// AssignedAt is time.Now() inside the builder, so the two payloads differ
	// in exactly that field. Compare everything else.
	var wantMsg, gotMsg protocol.AssignMsg
	if err := json.Unmarshal(want, &wantMsg); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	if err := json.Unmarshal(got, &gotMsg); err != nil {
		t.Fatalf("unmarshal got: %v", err)
	}
	gotMsg.AssignedAt = wantMsg.AssignedAt
	wantJSON, err := json.Marshal(wantMsg)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	gotJSON, err := json.Marshal(gotMsg)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("exported seam diverged from unexported:\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
}
