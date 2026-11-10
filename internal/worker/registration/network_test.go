// SPDX-License-Identifier: AGPL-3.0-or-later

package registration_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/uberware/sqi/internal/worker/capabilities"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/registration"
)

func TestRegister_SendsInterfacesAndInstanceID(t *testing.T) {
	url := startTestNATS(t)
	nc := connectNATS(t, url)

	want := []protocol.NetInterface{
		{Name: "eth0", MAC: "aa:bb:cc:dd:ee:01", Addrs: []string{"192.168.1.20/24"}},
		{Name: "eth1", MAC: "aa:bb:cc:dd:ee:02"},
	}
	reg, err := registration.New(nc, "worker-net", minimalCfg(), fmtres.ExprLimits{},
		capabilities.Capabilities{OS: "linux"}, discardLogger(),
		registration.WithInterfaces(want), registration.WithCloudInstanceID("i-0abc"))
	if err != nil {
		t.Fatalf("registration.New: %v", err)
	}
	if err := reg.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var got protocol.RegisterMsg
	if err := json.Unmarshal(firstWorkerStreamMsg(t, nc), &got); err != nil {
		t.Fatalf("unmarshal RegisterMsg: %v", err)
	}
	if !reflect.DeepEqual(got.Interfaces, want) || got.CloudInstanceID != "i-0abc" {
		t.Fatalf("registered interfaces %+v instance %q, want %+v and i-0abc",
			got.Interfaces, got.CloudInstanceID, want)
	}
}

func TestRegister_WithInterfacesNilSendsNone(t *testing.T) {
	url := startTestNATS(t)
	nc := connectNATS(t, url)

	reg, err := registration.New(nc, "worker-nonet", minimalCfg(), fmtres.ExprLimits{},
		capabilities.Capabilities{OS: "linux"}, discardLogger(), registration.WithInterfaces(nil))
	if err != nil {
		t.Fatalf("registration.New: %v", err)
	}
	if err := reg.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var got protocol.RegisterMsg
	if err := json.Unmarshal(firstWorkerStreamMsg(t, nc), &got); err != nil {
		t.Fatalf("unmarshal RegisterMsg: %v", err)
	}
	if len(got.Interfaces) != 0 || got.CloudInstanceID != "" {
		t.Fatalf("registered interfaces %+v instance %q, want none", got.Interfaces, got.CloudInstanceID)
	}
}
