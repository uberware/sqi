// SPDX-License-Identifier: AGPL-3.0-or-later

package protocol_test

import (
	"encoding/json"
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

// TestAssignMsg_EXPRFieldsRoundTrip asserts that a message carrying all three
// EXPR-phase-3 additions (the EXPR flag, the two distinct ordered step-level
// let blocks, an environment's let block, and the two parameter-type maps)
// survives json.Marshal -> json.Unmarshal with declaration order preserved
// and the step-template and step-script let blocks still distinguishable
// from one another.
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
		Environments: []protocol.AssignEnvironment{
			{
				Name: "Env1",
				Let:  []string{"e = 1", "f = e + 1"},
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

	// The two step-level blocks must remain distinct, not concatenated —
	// Template Schemas §3.6 forbids the script's block from shadowing the
	// template's, which only means something if they are still
	// distinguishable after the round trip.
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

	if len(out.Environments) != 1 {
		t.Fatalf("Environments = %+v, want 1 entry", out.Environments)
	}
	wantEnvLet := []string{"e = 1", "f = e + 1"}
	if !slices.Equal(out.Environments[0].Let, wantEnvLet) {
		t.Errorf("Environments[0].Let = %v, want %v", out.Environments[0].Let, wantEnvLet)
	}
}

// TestAssignMsg_EXPRFieldsOmittedForBaseSpec asserts that a base-spec
// assignment — one that sets none of the six EXPR-phase-3 fields — produces
// no trace of them on the wire. The fields are omitempty specifically so
// that a base-spec assignment's wire bytes do not grow; this test asserts
// that property rather than assuming it.
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

	// None of the six new keys may appear anywhere on the wire, including
	// nested inside an environment object.
	forbidden := []string{
		`"expr"`,
		`"step_template_let"`,
		`"step_script_let"`,
		`"parameter_types"`,
		`"job_parameter_types"`,
		`"let"`,
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
	if len(out.Environments) != 1 || out.Environments[0].Let != nil {
		t.Errorf("Environments = %+v, want Let nil", out.Environments)
	}
}
