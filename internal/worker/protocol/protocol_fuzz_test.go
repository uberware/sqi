// SPDX-License-Identifier: AGPL-3.0-only

package protocol_test

import (
	"encoding/json"
	"testing"

	"github.com/uberware/sqi/internal/worker/protocol"
)

// FuzzDecodeAssignMsg fuzzes the AssignMsg JSON decoder with arbitrary byte
// sequences sourced from the network.  The fuzzer verifies that:
//
//  1. json.Unmarshal never panics on arbitrary input.
//  2. Any successfully decoded message can be re-encoded without error, and
//     the re-encoded bytes can be decoded again without error (no panic, no
//     decode failure).  Semantic equality is not asserted because empty vs nil
//     slices/maps are indistinguishable in JSON when fields carry omitempty.
//
// Run with: go test -fuzz=FuzzDecodeAssignMsg ./internal/worker/protocol/.
func FuzzDecodeAssignMsg(f *testing.F) {
	// Seed corpus: valid messages, edge-case variants, and malformed JSON.
	f.Add([]byte(`{"version":"1","type":"assign","task_id":"t1","job_id":"j1","step_id":"s1","attempt_id":"a1","task_name":"Task 0","worker_id":"w1","assigned_at":"2024-01-01T00:00:00Z"}`))
	f.Add([]byte(`{"version":"1","type":"assign","on_run":{"command":"echo","args":["hello"],"timeout_seconds":60}}`))
	f.Add([]byte(`{"version":"1","type":"assign","on_run":{"command":"render","cancelation":{"mode":"NOTIFY_THEN_TERMINATE","notify_period_seconds":30}}}`))
	f.Add([]byte(`{"version":"1","type":"assign","path_map":[{"source_path_format":"s3://bucket/prefix","destination_path":"/mnt/bucket"}]}`))
	f.Add([]byte(`{"version":"1","type":"assign","environments":[{"name":"env1","on_enter":{"command":"setup.sh"},"on_exit":{"command":"teardown.sh"},"variables":{"FOO":"bar"}}]}`))
	f.Add([]byte(`{"version":"1","type":"assign","parameters":{"frame":"42"},"job_parameters":{"fps":"24"}}`))
	f.Add([]byte(`{"version":"1","type":"assign","embedded_files":[{"name":"f","filename":"f.sh","data":"#!/bin/sh\necho hi","runnable":true,"end_of_line":"LF"}]}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`[]`))
	f.Add([]byte(``))
	f.Add([]byte(`{"version":""}`))
	f.Add([]byte(`{"assigned_at":"not-a-time"}`))
	f.Add([]byte(`{"on_run":null}`))
	f.Add([]byte(`{"path_map":[]}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var msg protocol.AssignMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			// Unmarshal failure on arbitrary input is expected and safe.
			return
		}

		// Round-trip: re-encode and re-decode must not fail or panic.
		data2, err := json.Marshal(msg)
		if err != nil {
			t.Errorf("FuzzDecodeAssignMsg: re-marshal failed: %v", err)
			return
		}
		var msg2 protocol.AssignMsg
		if err := json.Unmarshal(data2, &msg2); err != nil {
			t.Errorf("FuzzDecodeAssignMsg: re-unmarshal of round-tripped data failed: %v", err)
		}
	})
}
