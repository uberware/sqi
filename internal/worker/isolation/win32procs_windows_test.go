// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package isolation

import (
	"testing"

	"golang.org/x/sys/windows"
)

// TestWin32ProcsResolve pins every Win32 procedure this package declares by
// name to one that exists in the system DLL.
//
// A LazyProc is resolved on first call, so a misspelled export compiles,
// vets and passes every test that injects a seam, and fails only when a real
// task tears down a credential. userenv.dll, for example, exports
// LoadUserProfileA/W but a single, unsuffixed UnloadUserProfile, since
// unloading takes only handles and has no strings to encode.
//
// The check needs no privilege, accounts or elevation (resolving an export is
// a plain DLL lookup), so unlike the rest of the Windows isolation
// coverage it runs in the ordinary `go test` pass on any Windows machine, and
// in the Windows unit-test step of the isolation-integration-windows CI job.
func TestWin32ProcsResolve(t *testing.T) {
	for _, proc := range []*windows.LazyProc{
		procLogonUserW,
		procLoadUserProfileW,
		procUnloadUserProfile,
	} {
		t.Run(proc.Name, func(t *testing.T) {
			if err := proc.Find(); err != nil {
				t.Errorf("%s: %v", proc.Name, err)
			}
		})
	}
}
