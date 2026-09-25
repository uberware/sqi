// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package winsvc

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ErrNotElevated is returned by RequireElevated.
var ErrNotElevated = errors.New("this command must be run from an elevated (Administrator) shell")

// advapi32 LSA and logon calls have no wrappers in golang.org/x/sys/windows
// v0.48.0; they are declared here the way internal/worker/isolation declares
// LogonUserW.
var (
	modAdvapi32               = windows.NewLazySystemDLL("advapi32.dll")
	procLsaOpenPolicy         = modAdvapi32.NewProc("LsaOpenPolicy")
	procLsaAddAccountRights   = modAdvapi32.NewProc("LsaAddAccountRights")
	procLsaClose              = modAdvapi32.NewProc("LsaClose")
	procLsaNtStatusToWinError = modAdvapi32.NewProc("LsaNtStatusToWinError")
	procLogonUserW            = modAdvapi32.NewProc("LogonUserW")
)

const (
	policyCreateAccount    = 0x00000010
	policyLookupNames      = 0x00000800
	logon32LogonService    = 5
	logon32ProviderDefault = 0
	seServiceLogonRight    = "SeServiceLogonRight"
)

type lsaUnicodeString struct {
	Length, MaximumLength uint16
	Buffer                *uint16
}

type lsaObjectAttributes struct {
	Length                   uint32
	RootDirectory            windows.Handle
	ObjectName               *lsaUnicodeString
	Attributes               uint32
	SecurityDescriptor       uintptr
	SecurityQualityOfService uintptr
}

// RequireElevated fails unless the process token is elevated.
func RequireElevated() error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return ErrNotElevated
	}
	return nil
}

func windowsComputerName() (string, error) { return windows.ComputerName() }

// lookupName rewrites the SCM's ".\name" into "HOST\name", which
// LookupAccountName requires.
func lookupName(account string) string {
	if rest, ok := strings.CutPrefix(account, `.\`); ok {
		if host, err := windowsComputerName(); err == nil {
			return host + `\` + rest
		}
	}
	return account
}

// splitAccount splits DOMAIN\user for LogonUserW; a UPN has no domain part.
func splitAccount(account string) (domain, user string) {
	if d, u, ok := strings.Cut(account, `\`); ok {
		return d, u
	}
	return "", account
}

// GrantServiceLogonRight grants "Log on as a service" to account. The SCM does
// not do this itself (services.msc does), and without it the service fails to
// start with error 1069. Granting a right the account already holds succeeds.
func GrantServiceLogonRight(account string) error {
	sid, _, _, err := windows.LookupSID("", lookupName(account))
	if err != nil {
		return fmt.Errorf("look up account %s: %w", account, err)
	}
	var attrs lsaObjectAttributes
	attrs.Length = uint32(unsafe.Sizeof(attrs))
	var policy windows.Handle
	//nolint:errcheck // LazyProc.Call's error is GetLastError, unrelated to the NTSTATUS return
	st, _, _ := procLsaOpenPolicy.Call(0, uintptr(unsafe.Pointer(&attrs)),
		policyCreateAccount|policyLookupNames, uintptr(unsafe.Pointer(&policy)))
	if st != 0 {
		return ntError("LsaOpenPolicy", st)
	}
	defer procLsaClose.Call(uintptr(policy)) //nolint:errcheck // handle cleanup; nothing to do on failure

	right, err := windows.UTF16FromString(seServiceLogonRight)
	if err != nil {
		return err
	}
	us := lsaUnicodeString{
		Length:        uint16((len(right) - 1) * 2), //nolint:gosec // G115: fixed 19-char right name; bytes without the NUL
		MaximumLength: uint16(len(right) * 2),       //nolint:gosec // G115: as above
		Buffer:        &right[0],
	}
	//nolint:errcheck // LazyProc.Call's error is GetLastError, unrelated to the NTSTATUS return
	st, _, _ = procLsaAddAccountRights.Call(uintptr(policy), uintptr(unsafe.Pointer(sid)),
		uintptr(unsafe.Pointer(&us)), 1)
	if st != 0 {
		return ntError("LsaAddAccountRights", st)
	}
	return nil
}

func ntError(op string, status uintptr) error {
	code, _, _ := procLsaNtStatusToWinError.Call(status) //nolint:errcheck // LazyProc.Call's error is GetLastError, unrelated to the NTSTATUS return
	return fmt.Errorf("%s: %w", op, syscall.Errno(code))
}

// ValidateCredentials performs a service-type logon as account, proving both
// the password and the logon right before the service is registered. Call it
// after GrantServiceLogonRight.
func ValidateCredentials(account, password string) error {
	domain, user := splitAccount(account)
	u, err := windows.UTF16PtrFromString(user)
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(password)
	if err != nil {
		return err
	}
	var d *uint16
	if domain != "" {
		if d, err = windows.UTF16PtrFromString(domain); err != nil {
			return err
		}
	}
	var token windows.Token
	r1, _, e1 := procLogonUserW.Call(uintptr(unsafe.Pointer(u)), uintptr(unsafe.Pointer(d)),
		uintptr(unsafe.Pointer(p)), logon32LogonService, logon32ProviderDefault, uintptr(unsafe.Pointer(&token)))
	if r1 == 0 {
		// Proc.Call's error is a syscall.Errno, never nil; Errno(0) means no code.
		var errno syscall.Errno
		if errors.As(e1, &errno) && errno != 0 {
			return fmt.Errorf("log on as %s: %w", account, errno)
		}
		return fmt.Errorf("log on as %s: failed with no error code", account)
	}
	return token.Close()
}
