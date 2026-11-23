// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package isolation

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// TestApplyInstallsToken lives in-package (package isolation, not
// isolation_test) because Credential.token is unexported: a real
// CreateProcessAsUser call is out of scope for a unit test (see
// make test-isolation-windows for that), but this proves apply's own
// mechanical job — copying a non-zero token onto cmd.SysProcAttr.Token — the
// same way apply_unix_internal_test.go's TestApplyInstallsCredential proves
// the POSIX side.
func TestApplyInstallsToken(t *testing.T) {
	cred := &Credential{token: windows.Token(0x1234)}

	cmd := exec.CommandContext(context.Background(), "cmd")
	if err := apply(cmd, cred); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if cmd.SysProcAttr == nil {
		t.Fatal("apply must install a non-nil SysProcAttr")
	}
	if got, want := cmd.SysProcAttr.Token, syscall.Token(0x1234); got != want {
		t.Errorf("cmd.SysProcAttr.Token = %#x, want %#x", got, want)
	}
}

// TestApplyRefusesZeroToken: a Credential whose token has been zeroed
// (Close's post-condition; see Credential.Close's doc) must never reach
// syscall.StartProcess as SysProcAttr.Token == 0, because that takes the
// plain CreateProcess branch and starts the child as the worker while apply
// reports success. Production paths hand apply either a credential from a
// successful logonUserOS call, which has a non-zero token, or nil, which
// Apply handles first; a lifecycle change that lets a closed credential
// reach apply fails here.
func TestApplyRefusesZeroToken(t *testing.T) {
	cred := &Credential{} // zero value: token == 0, matching a closed credential

	cmd := exec.CommandContext(context.Background(), "cmd")
	before := cmd.SysProcAttr

	err := apply(cmd, cred)
	if !errors.Is(err, errZeroToken) {
		t.Fatalf("apply(zero-token credential) = %v, want errZeroToken", err)
	}
	if cmd.SysProcAttr != before {
		t.Error("apply must not touch SysProcAttr when it refuses the credential")
	}
}
