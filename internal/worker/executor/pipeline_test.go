// SPDX-License-Identifier: AGPL-3.0-or-later

package executor

import (
	"reflect"
	"testing"

	"github.com/uberware/sqi/internal/worker/pathmap"
	"github.com/uberware/sqi/internal/worker/protocol"
)

func TestApplyDeliveries_FlagsAndEnv(t *testing.T) {
	lookup, err := pathmap.NewLookup([]protocol.PathMapRule{
		{SourcePath: "/projects", DestinationPath: "/mnt/cloud"},
	})
	if err != nil {
		t.Fatalf("NewLookup: %v", err)
	}
	action := &protocol.Action{Command: "render", Args: []string{"/projects/shot.ma"}}
	env := map[string]string{}
	deliveries := []protocol.PathDelivery{
		{Kind: "swap_in_place"},
		{Kind: "command_flags", Pattern: "--remap {src}={dest}"},
		{Kind: "environment", Variable: "PROJECT_ROOT"},
	}

	gotAction, gotEnv := applyDeliveries(deliveries, lookup, action, env)

	wantArgs := []string{"/mnt/cloud/shot.ma", "--remap /projects=/mnt/cloud"}
	if !reflect.DeepEqual(gotAction.Args, wantArgs) {
		t.Errorf("Args = %v, want %v", gotAction.Args, wantArgs)
	}
	if gotEnv["PROJECT_ROOT"] != "/projects=/mnt/cloud" {
		t.Errorf("env PROJECT_ROOT = %q", gotEnv["PROJECT_ROOT"])
	}
}

func TestApplyDeliveries_SwapDisabledLeavesArgs(t *testing.T) {
	lookup, err := pathmap.NewLookup([]protocol.PathMapRule{
		{SourcePath: "/projects", DestinationPath: "/mnt/cloud"},
	})
	if err != nil {
		t.Fatalf("NewLookup: %v", err)
	}
	action := &protocol.Action{Command: "render", Args: []string{"/projects/shot.ma"}}
	got, _ := applyDeliveries([]protocol.PathDelivery{{Kind: "translation_file"}}, lookup, action, map[string]string{})
	if got.Args[0] != "/projects/shot.ma" {
		t.Errorf("swap should be disabled; Args[0] = %q", got.Args[0])
	}
}
