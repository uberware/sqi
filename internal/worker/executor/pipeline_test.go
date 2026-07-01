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

// TestApplyDeliveries_CanonicalOrder verifies that deliveries are applied in the
// fixed canonical order (swap_in_place → command_flags → environment) regardless
// of the declared order. When command_flags is declared BEFORE swap_in_place the
// appended flag strings must not be double-substituted by the swap.
func TestApplyDeliveries_CanonicalOrder(t *testing.T) {
	lookup, err := pathmap.NewLookup([]protocol.PathMapRule{
		{SourcePath: "/projects", DestinationPath: "/mnt/cloud"},
	})
	if err != nil {
		t.Fatalf("NewLookup: %v", err)
	}
	action := &protocol.Action{Command: "render", Args: []string{"/projects/shot.ma"}}
	// Intentionally wrong declared order: command_flags listed BEFORE swap_in_place.
	deliveries := []protocol.PathDelivery{
		{Kind: "command_flags", Pattern: "--remap {src}={dest}"},
		{Kind: "swap_in_place"},
	}

	gotAction, _ := applyDeliveries(deliveries, lookup, action, map[string]string{})

	// swap_in_place must run first: the path arg is translated and the appended
	// flag references the original source path (not the already-translated dest).
	wantArgs := []string{"/mnt/cloud/shot.ma", "--remap /projects=/mnt/cloud"}
	if !reflect.DeepEqual(gotAction.Args, wantArgs) {
		t.Errorf("Args = %v; want %v (canonical order must apply swap before flags)", gotAction.Args, wantArgs)
	}
}

// TestApplyDeliveries_EnvironmentNilEnv guards against a nil-map panic: when an
// assignment carries no OpenJD environment variables, the env map passed in is
// nil, and the environment delivery must allocate it rather than write to nil.
func TestApplyDeliveries_EnvironmentNilEnv(t *testing.T) {
	lookup, err := pathmap.NewLookup([]protocol.PathMapRule{
		{SourcePath: "/projects", DestinationPath: "/mnt/cloud"},
	})
	if err != nil {
		t.Fatalf("NewLookup: %v", err)
	}
	action := &protocol.Action{Command: "render", Args: []string{"/projects/shot.ma"}}
	deliveries := []protocol.PathDelivery{{Kind: "environment", Variable: "PROJECT_ROOT"}}

	// env is nil (no OpenJD environment variables in the assignment).
	_, gotEnv := applyDeliveries(deliveries, lookup, action, nil)

	if gotEnv["PROJECT_ROOT"] != "/projects=/mnt/cloud" {
		t.Errorf("env PROJECT_ROOT = %q, want /projects=/mnt/cloud", gotEnv["PROJECT_ROOT"])
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
