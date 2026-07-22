// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package isolation

import (
	"context"
	"os/exec"
	"testing"
)

// TestApplyInstallsCredential lives in-package (package isolation, not
// isolation_test) because Credential.cred is unexported: an apply
// implementation that did nothing but "return nil" would still pass every
// black-box test in this package, since none of them can see inside the
// syscall.Credential it's supposed to attach. This is the one assertion that
// actually looks inside.
func TestApplyInstallsCredential(t *testing.T) {
	p := NewFake(map[string]FakeAccount{
		"render-svc": {UID: 1001, GID: 2001},
	})
	cred, err := p.Resolve(context.Background(), Spec{User: "render-svc"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	cmd := exec.CommandContext(context.Background(), "true")
	if err := Apply(cmd, cred); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential == nil {
		t.Fatal("Apply must install a non-nil Credential on cmd.SysProcAttr")
	}
	if got, want := cmd.SysProcAttr.Credential.Uid, uint32(1001); got != want {
		t.Errorf("cmd.SysProcAttr.Credential.Uid = %d, want %d", got, want)
	}
}
