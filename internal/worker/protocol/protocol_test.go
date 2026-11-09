// SPDX-License-Identifier: AGPL-3.0-or-later

package protocol_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
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

// TestAssignMsg_EXPRFieldsRoundTrip asserts that a message carrying all
// EXPR phase-3 fields (the EXPR flag, the two distinct ordered step-level
// let blocks, an environment's let block, the two parameter-type maps, and
// the job's/step's own declared names) survives json.Marshal ->
// json.Unmarshal with declaration order preserved and the step-template and
// step-script let blocks still distinguishable from one another.
func TestAssignMsg_EXPRFieldsRoundTrip(t *testing.T) {
	in := protocol.AssignMsg{
		EXPR: true,
		StepTemplateLet: []string{
			"a = 1",
			"b = a + 1",
		},
		StepScriptLet: []string{
			"c = b + 1",
			"d = c + 1",
		},
		ParameterTypes:    map[string]string{"Frame": "INT"},
		JobParameterTypes: map[string]string{"Scene": "PATH"},
		JobName:           "ExprJob",
		StepName:          "S",
		Environments: []protocol.AssignEnvironment{
			{
				Name:            "Env1",
				Let:             []string{"e = 1", "f = e + 1"},
				StepEnvironment: true,
			},
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

	if !out.EXPR {
		t.Error("EXPR = false, want true")
	}

	// Declaration order preserved.
	wantStepTemplateLet := []string{"a = 1", "b = a + 1"}
	if !slices.Equal(out.StepTemplateLet, wantStepTemplateLet) {
		t.Errorf("StepTemplateLet = %v, want %v", out.StepTemplateLet, wantStepTemplateLet)
	}
	wantStepScriptLet := []string{"c = b + 1", "d = c + 1"}
	if !slices.Equal(out.StepScriptLet, wantStepScriptLet) {
		t.Errorf("StepScriptLet = %v, want %v", out.StepScriptLet, wantStepScriptLet)
	}

	// The two step-level blocks must remain distinct: Template Schemas §3.6
	// forbids the script's block from shadowing the template's, which requires
	// them to be distinguishable after the round trip.
	if slices.Equal(out.StepTemplateLet, out.StepScriptLet) {
		t.Error("StepTemplateLet and StepScriptLet must be distinct blocks, got identical content")
	}
	for _, s := range out.StepScriptLet {
		if slices.Contains(out.StepTemplateLet, s) {
			t.Errorf("binding %q present in both StepTemplateLet and StepScriptLet after round trip", s)
		}
	}

	if got := out.ParameterTypes["Frame"]; got != "INT" {
		t.Errorf("ParameterTypes[Frame] = %q, want INT", got)
	}
	if got := out.JobParameterTypes["Scene"]; got != "PATH" {
		t.Errorf("JobParameterTypes[Scene] = %q, want PATH", got)
	}
	if out.JobName != "ExprJob" {
		t.Errorf("JobName = %q, want ExprJob", out.JobName)
	}
	if out.StepName != "S" {
		t.Errorf("StepName = %q, want S", out.StepName)
	}

	if len(out.Environments) != 1 {
		t.Fatalf("Environments = %+v, want 1 entry", out.Environments)
	}
	wantEnvLet := []string{"e = 1", "f = e + 1"}
	if !slices.Equal(out.Environments[0].Let, wantEnvLet) {
		t.Errorf("Environments[0].Let = %v, want %v", out.Environments[0].Let, wantEnvLet)
	}
	if !out.Environments[0].StepEnvironment {
		t.Error("Environments[0].StepEnvironment = false, want true")
	}
}

// TestAssignMsg_EXPRFieldsOmittedForBaseSpec asserts that a base-spec
// assignment — one that sets none of the nine EXPR-phase-3 fields —
// produces no trace of them on the wire. The fields are omitempty so that a
// base-spec assignment's wire bytes do not carry them.
func TestAssignMsg_EXPRFieldsOmittedForBaseSpec(t *testing.T) {
	in := protocol.AssignMsg{
		Version:  protocol.ProtocolVersion,
		Type:     protocol.TypeAssign,
		TaskID:   "t1",
		JobID:    "j1",
		StepID:   "s1",
		TaskName: "Render[Frame=1]",
		Parameters: map[string]string{
			"Frame": "1",
		},
		JobParameters: map[string]string{
			"Scene": "/projects/shot.ma",
		},
		OnRun: &protocol.Action{Command: "render"},
		Environments: []protocol.AssignEnvironment{
			{Name: "Env1"},
		},
	}

	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// None of the nine new keys may appear anywhere on the wire, including
	// nested inside an environment object.
	forbidden := []string{
		`"expr"`,
		`"step_template_let"`,
		`"step_script_let"`,
		`"parameter_types"`,
		`"job_parameter_types"`,
		`"let"`,
		`"job_name"`,
		`"step_name"`,
		`"step_environment"`,
	}
	s := string(data)
	for _, key := range forbidden {
		if strings.Contains(s, key) {
			t.Errorf("marshaled base-spec AssignMsg unexpectedly contains %s: %s", key, s)
		}
	}

	// Round trip back to zero values.
	var out protocol.AssignMsg
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.EXPR {
		t.Error("EXPR = true, want false")
	}
	if out.StepTemplateLet != nil || out.StepScriptLet != nil {
		t.Errorf("StepTemplateLet/StepScriptLet = %v/%v, want nil/nil", out.StepTemplateLet, out.StepScriptLet)
	}
	if out.ParameterTypes != nil || out.JobParameterTypes != nil {
		t.Errorf("ParameterTypes/JobParameterTypes = %v/%v, want nil/nil", out.ParameterTypes, out.JobParameterTypes)
	}
	if out.JobName != "" || out.StepName != "" {
		t.Errorf("JobName/StepName = %q/%q, want empty/empty", out.JobName, out.StepName)
	}
	if len(out.Environments) != 1 || out.Environments[0].Let != nil || out.Environments[0].StepEnvironment {
		t.Errorf("Environments = %+v, want Let nil and StepEnvironment false", out.Environments)
	}
}

// TestMessageWorkerShutdown_WireValue pins the wire value itself: the worker
// sends it and the server matches it, and tests on both sides compare against
// the constant, so a rename would pass them all while breaking a worker and a
// server of different versions.
func TestMessageWorkerShutdown_WireValue(t *testing.T) {
	if protocol.MessageWorkerShutdown != "worker_shutdown" {
		t.Fatalf("MessageWorkerShutdown = %q, want %q", protocol.MessageWorkerShutdown, "worker_shutdown")
	}
}

// TestRegisterMsg_InstanceIDWireKey pins the instance ID's wire key and that it
// is omitted when empty, so a registration from a worker that sets none
// carries no instance_id key (the field does not affect ProtocolVersion).
func TestRegisterMsg_InstanceIDWireKey(t *testing.T) {
	data, err := json.Marshal(protocol.RegisterMsg{WorkerID: "w1", InstanceID: "inst-1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"instance_id":"inst-1"`) {
		t.Errorf("marshaled RegisterMsg = %s, want an \"instance_id\" key", data)
	}
	empty, err := json.Marshal(protocol.RegisterMsg{WorkerID: "w1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(empty), `"instance_id"`) {
		t.Errorf("marshaled RegisterMsg without an instance ID = %s, want no \"instance_id\" key", empty)
	}
}

// TestHeartbeatMsg_AttemptFieldsWireKeys pins the wire keys of the held-attempt
// fields and that a heartbeat that sets neither carries neither (both are
// omitempty, so they do not affect ProtocolVersion).
func TestHeartbeatMsg_AttemptFieldsWireKeys(t *testing.T) {
	data, err := json.Marshal(protocol.HeartbeatMsg{AttemptsReported: true, ActiveAttemptIDs: []string{"a1", "a2"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["attempts_reported"] != true {
		t.Errorf("attempts_reported = %v, want true", m["attempts_reported"])
	}
	ids, _ := m["active_attempt_ids"].([]any) //nolint:errcheck // a failed assertion leaves ids nil and fails below
	if len(ids) != 2 || ids[0] != "a1" || ids[1] != "a2" {
		t.Errorf("active_attempt_ids = %v, want [a1 a2]", m["active_attempt_ids"])
	}

	old, err := json.Marshal(protocol.HeartbeatMsg{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"attempts_reported", "active_attempt_ids"} {
		if strings.Contains(string(old), key) {
			t.Errorf("zero heartbeat %s contains %q, want it omitted", old, key)
		}
	}
}

func TestAssignMsg_LogProgressRoundTrip(t *testing.T) {
	if protocol.ProtocolVersion != "6" { // adding an omitempty field does not change it
		t.Errorf("ProtocolVersion = %q, want 6", protocol.ProtocolVersion)
	}
	in := protocol.AssignMsg{LogProgress: &protocol.LogProgress{
		FrameParameter: "Frames",
		Progress:       []string{"a"},
		Status:         []string{"b"},
	}}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"log_progress"`, `"frame_parameter"`, `"progress"`, `"status"`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("wire missing %s: %s", key, data)
		}
	}
	var out protocol.AssignMsg
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	lp := out.LogProgress
	if lp == nil || lp.FrameParameter != "Frames" || !slices.Equal(lp.Progress, []string{"a"}) || !slices.Equal(lp.Status, []string{"b"}) {
		t.Errorf("round trip = %+v", lp)
	}
	data, err = json.Marshal(protocol.AssignMsg{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "log_progress") {
		t.Errorf("empty message carries log_progress: %s", data)
	}
}

func TestControlMsg_RoundTrip(t *testing.T) {
	tests := []struct {
		name string
		in   protocol.ControlMsg
		want string
	}{
		{
			"sleep",
			protocol.ControlMsg{Version: protocol.ProtocolVersion, Type: protocol.TypeControl, Command: protocol.ControlSleep},
			`{"version":"6","type":"control","command":"sleep"}`,
		},
		{
			"wake",
			protocol.ControlMsg{
				Version: protocol.ProtocolVersion, Type: protocol.TypeControl, Command: protocol.ControlWake,
				MACs: []string{"aa:bb:cc:dd:ee:ff"}, Targets: []string{"192.168.1.255:9"},
			},
			`{"version":"6","type":"control","command":"wake","macs":["aa:bb:cc:dd:ee:ff"],"targets":["192.168.1.255:9"]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tt.want {
				t.Errorf("json = %s, want %s", data, tt.want)
			}
			var out protocol.ControlMsg
			if err := json.Unmarshal(data, &out); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(out, tt.in) {
				t.Errorf("round trip = %+v, want %+v", out, tt.in)
			}
		})
	}
	if protocol.ControlSleep != "sleep" || protocol.ControlShutdown != "shutdown" || protocol.ControlWake != "wake" {
		t.Error("control command strings changed")
	}
}

func TestRegisterMsg_InterfacesAndCloudInstanceID(t *testing.T) {
	empty, err := json.Marshal(protocol.RegisterMsg{})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"interfaces", "cloud_instance_id"} {
		if strings.Contains(string(empty), key) {
			t.Errorf("empty RegisterMsg %s contains %q, want it omitted", empty, key)
		}
	}
	in := protocol.RegisterMsg{
		Interfaces:      []protocol.NetInterface{{Name: "eth0", MAC: "aa:bb:cc:dd:ee:ff", Addrs: []string{"192.168.1.20/24"}}},
		CloudInstanceID: "i-0abc",
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out protocol.RegisterMsg
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out.Interfaces, in.Interfaces) || out.CloudInstanceID != in.CloudInstanceID {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}
}

func TestControlReplyMsg_RoundTrip(t *testing.T) {
	data, err := json.Marshal(protocol.ControlReplyMsg{Accepted: true})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"accepted":true}` {
		t.Errorf("json = %s", data)
	}
	data, err = json.Marshal(protocol.ControlReplyMsg{Reason: "busy"})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"accepted":false,"reason":"busy"}` {
		t.Errorf("json = %s", data)
	}
}

// The refusal reasons are wire values: the server matches a worker's reply
// against them, so a change here changes what older peers understand.
func TestControlRefusalReasons(t *testing.T) {
	tests := []struct{ got, want string }{
		{protocol.ControlRefusalBusy, "busy"},
		{protocol.ControlRefusalNotAllowed, "worker.power.allow_remote_sleep is false"},
		{protocol.ControlRefusalAlreadyRetiring, "already retiring"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("refusal reason = %q, want %q", tt.got, tt.want)
		}
	}
}

// The registration caps are what the worker sends at most and the server
// keeps at most.
func TestRegistrationInterfaceCaps(t *testing.T) {
	if protocol.MaxInterfaces != 32 || protocol.MaxAddrsPerInterface != 16 {
		t.Errorf("caps = %d interfaces, %d addresses each; want 32, 16",
			protocol.MaxInterfaces, protocol.MaxAddrsPerInterface)
	}
}
