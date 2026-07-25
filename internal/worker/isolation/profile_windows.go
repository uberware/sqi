// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package isolation

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// LoadUserProfileW/UnloadUserProfile have no wrapper in
// golang.org/x/sys/windows v0.47.0, so they are declared here the same way
// provider_windows.go declares LogonUserW.
//
// Note the asymmetry in the names, which is NOT a typo: userenv.dll exports
// LoadUserProfileA/W but a single, unsuffixed UnloadUserProfile, because
// unloading takes only handles and so has no string-encoding variants. A
// symmetry-seeking "UnloadUserProfileW" does not exist — it panicked every
// Credential.Close (LazyProc.Call calls mustFind), taking the worker down on
// task teardown. TestWin32ProcsResolve pins all three names.
var (
	modUserenv            = windows.NewLazySystemDLL("userenv.dll")
	procLoadUserProfileW  = modUserenv.NewProc("LoadUserProfileW")
	procUnloadUserProfile = modUserenv.NewProc("UnloadUserProfile")
)

// piNoUI suppresses the profile-error dialog LoadUserProfile would otherwise
// try to display. A service has no interactive desktop, so without it a
// failure blocks instead of returning.
const piNoUI = 0x00000001

// profileInfo mirrors Win32 PROFILEINFOW. Only the fields this package sets
// are named meaningfully; the rest must still be present for the struct size
// the API validates.
type profileInfo struct {
	Size        uint32
	Flags       uint32
	UserName    *uint16
	ProfilePath *uint16
	DefaultPath *uint16
	ServerName  *uint16
	PolicyPath  *uint16
	Profile     windows.Handle
}

// mustResolve reports a missing export as an error. Every LazyProc.Call in
// this package goes through it first, because LazyProc.Call PANICS when the
// procedure cannot be found — an unrecoverable crash of the worker daemon,
// reached on a path (task teardown) that has no business being fatal. An
// export this package names wrongly, or one a future Windows drops, must
// surface as a failed task, not a dead worker.
func mustResolve(proc *windows.LazyProc) error {
	if err := proc.Find(); err != nil {
		return fmt.Errorf("resolve %s: %w", proc.Name, err)
	}
	return nil
}

// loadProfile mounts user's registry hive and ensures their profile directory
// exists, returning the handle UnloadUserProfile needs.
//
// This is not optional polish. CreateProcessAsUser does NOT load a profile:
// without this call the task gets the .DEFAULT hive rather than its own
// HKEY_CURRENT_USER, and an account that has never logged on interactively
// has no profile directory at all — which is exactly the
// GetUserProfileDirectory failure logonUserOS used to swallow into an empty
// Home. Every DCC writes preferences, license caches, and crash dumps through
// one or both of those.
//
// Requires SeBackupPrivilege and SeRestorePrivilege, which LocalSystem holds.
func loadProfile(tok windows.Token, user string) (windows.Handle, error) {
	if err := mustResolve(procLoadUserProfileW); err != nil {
		return 0, err
	}
	name, err := windows.UTF16PtrFromString(user)
	if err != nil {
		return 0, fmt.Errorf("encode username: %w", err)
	}
	pi := profileInfo{
		Flags:    piNoUI,
		UserName: name,
	}
	pi.Size = uint32(unsafe.Sizeof(pi))

	r1, _, e1 := procLoadUserProfileW.Call(uintptr(tok), uintptr(unsafe.Pointer(&pi)))
	if r1 == 0 {
		return 0, fmt.Errorf("LoadUserProfileW(%s): %w", user, e1)
	}
	return pi.Profile, nil
}

// unloadProfile releases a hive mounted by loadProfile. Called from
// Credential.Close BEFORE the token is closed: UnloadUserProfile needs the
// same token that loaded it.
func unloadProfile(tok windows.Token, h windows.Handle) error {
	if h == 0 {
		return nil
	}
	if err := mustResolve(procUnloadUserProfile); err != nil {
		return err
	}
	r1, _, e1 := procUnloadUserProfile.Call(uintptr(tok), uintptr(h))
	if r1 == 0 {
		return fmt.Errorf("UnloadUserProfile: %w", e1)
	}
	return nil
}

// canTraverseOS reports whether cred's token holds SeChangeNotifyPrivilege.
// See logonUserProvider.Resolve for why an account without it is refused
// rather than allowed to fail later with an opaque access-denied.
func canTraverseOS(cred *Credential) (bool, error) {
	if cred == nil || cred.token == 0 {
		return true, nil
	}
	return tokenHasPrivilege(cred.token, "SeChangeNotifyPrivilege")
}
