// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package isolation_test

import (
	"context"
	"os/exec"
	"testing"

	"github.com/uberware/sqi/internal/worker/isolation"
)

// TestFakeProviderStripsGID0FromSupplementaryGroups is the Minor-6 fix: the
// fake previously populated no Groups at all on the returned Credential, so
// supplementary-group behavior — including the Important-1 gid-0 strip — was
// structurally invisible to every fake-based test. This proves the fake now
// applies the same stripGID0FromSupplementary logic the real provider does.
//
// Moved here (from isolation_test.go, which carries no build tag) because it
// asserts through cmd.SysProcAttr.Credential — a POSIX-only field that does
// not exist on Windows's syscall.SysProcAttr — so this test can only ever
// compile against the POSIX Credential shape.
func TestFakeProviderStripsGID0FromSupplementaryGroups(t *testing.T) {
	p := isolation.NewFake(map[string]isolation.FakeAccount{
		"render-svc": {
			UID:               1001,
			GID:               2001,
			SupplementaryGIDs: []uint32{2001, 0, 2010},
		},
	})

	cred, err := p.Resolve(context.Background(), isolation.Spec{User: "render-svc"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	cmd := exec.CommandContext(context.Background(), "true")
	if err := isolation.Apply(cmd, cred); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential == nil {
		t.Fatal("Apply must install a non-nil Credential")
	}
	for _, g := range cmd.SysProcAttr.Credential.Groups {
		if g == 0 {
			t.Fatalf("Groups = %v, want gid 0 stripped from the fake's supplementary set too", cmd.SysProcAttr.Credential.Groups)
		}
	}
}
