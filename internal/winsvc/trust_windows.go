// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package winsvc

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// trustedInstallerSID is NT SERVICE\TrustedInstaller, which owns the
// operating system's own files.
const trustedInstallerSID = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"

// ACE types x/sys does not export (winnt.h).
const (
	accessAllowedCallbackACEType      = 0x9
	accessDeniedObjectACEType         = 0x6
	accessDeniedCallbackACEType       = 0xA
	accessDeniedCallbackObjectACEType = 0xC
)

// changeRights are the rights that let their holder change a file or
// directory, or what it holds, each with the abbreviation icacls prints.
var changeRights = []struct {
	mask windows.ACCESS_MASK
	abbr string
}{
	{0x2, "WD"},        // FILE_WRITE_DATA / FILE_ADD_FILE
	{0x4, "AD"},        // FILE_APPEND_DATA / FILE_ADD_SUBDIRECTORY
	{0x10, "WEA"},      // FILE_WRITE_EA
	{0x40, "DC"},       // FILE_DELETE_CHILD
	{0x100, "WA"},      // FILE_WRITE_ATTRIBUTES
	{0x10000, "D"},     // DELETE
	{0x40000, "WDAC"},  // WRITE_DAC
	{0x80000, "WO"},    // WRITE_OWNER
	{0x10000000, "GA"}, // GENERIC_ALL
	{0x40000000, "GW"}, // GENERIC_WRITE
	{0x02000000, "MA"}, // MAXIMUM_ALLOWED
}

// changeMask is every right in changeRights.
var changeMask = func() windows.ACCESS_MASK {
	var m windows.ACCESS_MASK
	for _, r := range changeRights {
		m |= r.mask
	}
	return m
}()

// PathFailure is one path CheckPaths refused.
type PathFailure struct {
	// Key and Path are the PathRef's.
	Key, Path string
	// JudgedPath is the path whose security was read: Path, or, when Path
	// does not exist, its nearest existing ancestor.
	JudgedPath string
	// Reason says what is wrong, such as "owner DESKTOP\bob is not trusted"
	// or "BUILTIN\Users can write (WD,AD)"; several are joined with "; ".
	Reason string
}

// UntrustedPathsError reports every path CheckPaths refused, in the order
// they were given.
type UntrustedPathsError struct {
	Failures []PathFailure
}

// Error renders one line per failure: "<key> <path>: <reason>", followed by
// the judged path when it is an ancestor of path.
func (e *UntrustedPathsError) Error() string {
	lines := make([]string, 0, len(e.Failures))
	for _, f := range e.Failures {
		line := fmt.Sprintf("%s %s: %s", f.Key, f.Path, f.Reason)
		if f.JudgedPath != f.Path {
			line += fmt.Sprintf(" (it does not exist; judged by its nearest existing ancestor %s)", f.JudgedPath)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// CheckPaths refuses any of refs that someone other than the trusted
// principals could change: SYSTEM, BUILTIN\Administrators,
// NT SERVICE\TrustedInstaller and, unless it is "" (LocalSystem), account,
// the account a service will run as — it changing its own files is not an
// escalation.
//
// A path passes when it is a real file or directory (not a junction, symbolic
// link or other reparse point, and not reached through one), its owner is
// trusted, and no allow ACE that applies to it grants anyone else a right
// that could change it or what it holds. A path that does not exist yet is
// judged by its nearest existing ancestor, since whoever can create children
// there can create it first. Paths must be absolute.
//
// It returns nil, an *UntrustedPathsError naming every refused path, or the
// error that stopped it reading one.
func CheckPaths(refs []PathRef, account string) error {
	trusted, err := trustedPrincipals(account)
	if err != nil {
		return err
	}
	return checkPaths(refs, trusted)
}

// trustedPrincipals is SYSTEM, Administrators, TrustedInstaller and account's
// SID when account is not LocalSystem. account must name a user: trusting a
// group would trust every member of it.
func trustedPrincipals(account string) ([]*windows.SID, error) {
	sids := make([]*windows.SID, 0, 4)
	for _, wk := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid} {
		sid, err := windows.CreateWellKnownSid(wk)
		if err != nil {
			return nil, fmt.Errorf("well-known SID: %w", err)
		}
		sids = append(sids, sid)
	}
	ti, err := windows.StringToSid(trustedInstallerSID)
	if err != nil {
		return nil, fmt.Errorf("TrustedInstaller SID: %w", err)
	}
	sids = append(sids, ti)
	if account = NormalizeAccount(account); account != "" {
		sid, _, typ, err := windows.LookupSID("", lookupName(account))
		if err != nil {
			return nil, fmt.Errorf("look up account %s: %w", account, err)
		}
		if typ != windows.SidTypeUser {
			return nil, fmt.Errorf("account %s is not a user account", account)
		}
		sids = append(sids, sid)
	}
	return sids, nil
}

// checkPaths is CheckPaths with the trusted set given.
func checkPaths(refs []PathRef, trusted []*windows.SID) error {
	var failures []PathFailure
	for _, ref := range refs {
		judged, reasons, err := judgePath(ref.Path, trusted)
		if err != nil {
			return fmt.Errorf("check %s %s: %w", ref.Key, ref.Path, err)
		}
		if len(reasons) > 0 {
			failures = append(failures, PathFailure{
				Key: ref.Key, Path: ref.Path, JudgedPath: judged, Reason: strings.Join(reasons, "; "),
			})
		}
	}
	if len(failures) > 0 {
		return &UntrustedPathsError{Failures: failures}
	}
	return nil
}

// judgePath finds the path to judge — path, or its nearest existing ancestor
// — and returns it with the reasons it fails, none when it passes.
func judgePath(path string, trusted []*windows.SID) (judged string, reasons []string, err error) {
	if !filepath.IsAbs(path) {
		return path, nil, fmt.Errorf("%q is not an absolute path", path)
	}
	judged = filepath.Clean(path)
	for {
		h, _, err := openPlain(judged, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES)
		switch {
		case err == nil:
			reasons, err := judgeOpened(h, judged, trusted)
			windows.CloseHandle(h) //nolint:errcheck // best-effort close of a handle we are done with
			return judged, reasons, err
		case errors.Is(err, errReparsePoint):
			return judged, []string{errReparsePoint.Error()}, nil
		case !isNotFound(err):
			return judged, nil, err
		}
		parent := filepath.Dir(judged)
		if parent == judged {
			return judged, nil, fmt.Errorf("no part of %s exists", path)
		}
		judged = parent
	}
}

// judgeOpened judges the path h was opened on: its ancestors, then its owner
// and DACL, read from h.
func judgeOpened(h windows.Handle, judged string, trusted []*windows.SID) ([]string, error) {
	reasons, err := ancestorReparsePoints(judged)
	if err != nil {
		return nil, err
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, fmt.Errorf("read security of %s: %w", judged, err)
	}
	more, err := judgeSecurity(sd, trusted)
	if err != nil {
		return nil, fmt.Errorf("read security of %s: %w", judged, err)
	}
	return append(reasons, more...), nil
}

// ancestorReparsePoints names every ancestor of path that is a junction,
// symbolic link or other reparse point. Opening path follows them, so whoever
// can retarget one decides what path names.
func ancestorReparsePoints(path string) ([]string, error) {
	var reasons []string
	for p := filepath.Dir(path); ; p = filepath.Dir(p) {
		h, _, err := openPlain(p, windows.FILE_READ_ATTRIBUTES)
		switch {
		case err == nil:
			windows.CloseHandle(h) //nolint:errcheck // only its attributes were needed
		case errors.Is(err, errReparsePoint):
			reasons = append(reasons, p+" "+errReparsePoint.Error())
		default:
			return nil, err
		}
		if filepath.Dir(p) == p {
			return reasons, nil
		}
	}
}

func isNotFound(err error) bool {
	return errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}

// judgeSecurity returns the reasons sd's owner and DACL fail, none when they
// pass.
func judgeSecurity(sd *windows.SECURITY_DESCRIPTOR, trusted []*windows.SID) ([]string, error) {
	var reasons []string
	owner, _, err := sd.Owner()
	if err != nil {
		return nil, fmt.Errorf("read owner: %w", err)
	}
	switch {
	case owner == nil:
		reasons = append(reasons, "has no owner")
	case !isTrusted(owner, trusted):
		reasons = append(reasons, fmt.Sprintf("owner %s is not trusted", accountName(owner)))
	}
	dacl, _, err := sd.DACL()
	switch {
	case errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) || (err == nil && dacl == nil):
		// No DACL, or a NULL one, grants everyone full control.
		return append(reasons, "has a NULL DACL (everyone has full control)"), nil
	case err != nil:
		return nil, fmt.Errorf("read DACL: %w", err)
	}
	more, err := judgeDACL(dacl, trusted)
	if err != nil {
		return nil, err
	}
	return append(reasons, more...), nil
}

// judgeDACL returns a reason for each untrusted principal that an allow ACE
// applying to the object itself grants a change right, with every such
// right it holds, and one for each ACE of a type it cannot read. Deny ACEs
// only take access away, so they never fail a path.
func judgeDACL(dacl *windows.ACL, trusted []*windows.SID) ([]string, error) {
	var (
		holders []*windows.SID
		masks   []windows.ACCESS_MASK
		reasons []string
	)
	for i := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return nil, fmt.Errorf("read ACE %d: %w", i, err)
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue // applies only to children
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE, accessDeniedObjectACEType,
			accessDeniedCallbackACEType, accessDeniedCallbackObjectACEType:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE, accessAllowedCallbackACEType:
		default:
			// Object, compound and anything newer: its SID is not where an
			// ACCESS_ALLOWED_ACE keeps it, so fail closed.
			reasons = append(reasons, fmt.Sprintf("has an access control entry of unsupported type %d", ace.Header.AceType))
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if ace.Mask&changeMask == 0 || isTrusted(sid, trusted) {
			continue
		}
		if j := slices.IndexFunc(holders, sid.Equals); j >= 0 {
			masks[j] |= ace.Mask
			continue
		}
		holders = append(holders, sid)
		masks = append(masks, ace.Mask)
	}
	for j, sid := range holders {
		reasons = append(reasons, fmt.Sprintf("%s can write (%s)", accountName(sid), rightsString(masks[j])))
	}
	return reasons, nil
}

func isTrusted(sid *windows.SID, trusted []*windows.SID) bool {
	return slices.ContainsFunc(trusted, sid.Equals)
}

// rightsString lists the change rights in mask by their icacls
// abbreviations.
func rightsString(mask windows.ACCESS_MASK) string {
	var abbrs []string
	for _, r := range changeRights {
		if mask&r.mask != 0 {
			abbrs = append(abbrs, r.abbr)
		}
	}
	return strings.Join(abbrs, ",")
}

// accountName is sid's DOMAIN\name, or its SID string when it does not
// resolve.
func accountName(sid *windows.SID) string {
	name, domain, _, err := sid.LookupAccount("")
	switch {
	case err != nil:
		return sid.String()
	case domain == "":
		return name
	default:
		return domain + `\` + name
	}
}
