// SPDX-License-Identifier: AGPL-3.0-or-later

package protocol_test

import (
	"encoding/json"
	"testing"

	"github.com/uberware/sqi/internal/worker/protocol"
)

func TestAssignMsg_PathDeliveriesRoundTrip(t *testing.T) {
	in := protocol.AssignMsg{
		PathDeliveries: []protocol.PathDelivery{
			{Kind: "swap_in_place"},
			{Kind: "command_flags", Pattern: "--remap {src}={dest}"},
			{Kind: "environment", Variable: "PROJECT_ROOT"},
		},
		Staging: []protocol.StageEntry{
			{Path: "/projects/showA", Direction: "IN", ObjectType: "DIRECTORY"},
		},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out protocol.AssignMsg
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.PathDeliveries) != 3 || out.PathDeliveries[1].Pattern != "--remap {src}={dest}" {
		t.Errorf("PathDeliveries = %+v", out.PathDeliveries)
	}
	if len(out.Staging) != 1 || out.Staging[0].Direction != "IN" {
		t.Errorf("Staging = %+v", out.Staging)
	}
}
