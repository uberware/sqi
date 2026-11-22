// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package winsvc

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

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

// Access rights by the abbreviation icacls prints for them.
const (
	rightWD   = windows.ACCESS_MASK(0x2)        // FILE_WRITE_DATA / FILE_ADD_FILE
	rightAD   = windows.ACCESS_MASK(0x4)        // FILE_APPEND_DATA / FILE_ADD_SUBDIRECTORY
	rightWEA  = windows.ACCESS_MASK(0x10)       // FILE_WRITE_EA
	rightDC   = windows.ACCESS_MASK(0x40)       // FILE_DELETE_CHILD
	rightWA   = windows.ACCESS_MASK(0x100)      // FILE_WRITE_ATTRIBUTES
	rightD    = windows.ACCESS_MASK(0x10000)    // DELETE
	rightWDAC = windows.ACCESS_MASK(0x40000)    // WRITE_DAC
	rightWO   = windows.ACCESS_MASK(0x80000)    // WRITE_OWNER
	rightGA   = windows.ACCESS_MASK(0x10000000) // GENERIC_ALL
	rightGW   = windows.ACCESS_MASK(0x40000000) // GENERIC_WRITE
	rightMA   = windows.ACCESS_MASK(0x02000000) // MAXIMUM_ALLOWED
)

// rightNames lists the rights the rules use, in the order icacls prints them.
var rightNames = []struct {
	mask windows.ACCESS_MASK
	abbr string
}{
	{rightWD, "WD"},
	{rightAD, "AD"},
	{rightWEA, "WEA"},
	{rightDC, "DC"},
	{rightWA, "WA"},
	{rightD, "D"},
	{rightWDAC, "WDAC"},
	{rightWO, "WO"},
	{rightGA, "GA"},
	{rightGW, "GW"},
	{rightMA, "MA"},
}

// rightsRule is which rights, granted to a principal outside the trusted
// set, fail a path, and how a refusal describes them.
type rightsRule struct {
	mask windows.ACCESS_MASK
	verb string
}

var (
	// objectRule judges the path itself: any right that could change it or
	// what it holds.
	objectRule = rightsRule{
		mask: rightWD | rightAD | rightWEA | rightDC | rightWA | rightD | rightWDAC | rightWO | rightGA | rightGW | rightMA,
		verb: "can write",
	}
	// ancestorRule judges each of its ancestors: any right that could move
	// the path aside, so its holder could plant their own in its place, or
	// grant that. Adding a file or folder cannot replace an existing child,
	// so FILE_ADD_FILE and FILE_ADD_SUBDIRECTORY alone pass (C:\ grants
	// Authenticated Users AD on itself).
	ancestorRule = rightsRule{
		mask: rightDC | rightD | rightWDAC | rightWO | rightGA | rightGW | rightMA,
		verb: "can replace it or its children",
	}
)

// volumeRootRule judges a volume or share root: ancestorRule without DELETE,
// since a root cannot be deleted or renamed, which is all DELETE on it would
// allow (a new data volume grants Authenticated Users Modify on its root).
var volumeRootRule = rightsRule{mask: ancestorRule.mask &^ rightD, verb: ancestorRule.verb}

// ancestorRuleFor is the rule ancestor p is judged by: volumeRootRule when p
// is the root of its volume (C:\, \\server\share\, \\?\Volume{…}\), else
// ancestorRule.
func ancestorRuleFor(p string) rightsRule {
	if p == filepath.VolumeName(p)+`\` {
		return volumeRootRule
	}
	return ancestorRule
}

// PathFailure is one path CheckPaths refused.
type PathFailure struct {
	// Key and Path are the PathRef's.
	Key, Path string
	// JudgedPath is the path whose security failed: Path; its nearest
	// existing ancestor when Path does not exist; or an ancestor whose
	// controller could replace Path.
	JudgedPath string
	// Reason says what is wrong, such as "owner DESKTOP\bob is not trusted",
	// "BUILTIN\Users can write (WD,AD)" or "ancestor C:\x: BUILTIN\Users can
	// replace it or its children (DC)"; several are joined with "; ".
	Reason string
}

// UntrustedPathsError reports every path CheckPaths refused, in the order
// they were given.
type UntrustedPathsError struct {
	Failures []PathFailure
}

// Error renders one line per failure: "<key> <path>: <reason>".
func (e *UntrustedPathsError) Error() string {
	lines := make([]string, 0, len(e.Failures))
	for _, f := range e.Failures {
		lines = append(lines, fmt.Sprintf("%s %s: %s", f.Key, f.Path, f.Reason))
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
// link or other reparse point), its owner is trusted, and no allow ACE that
// applies to it grants anyone else a right that could change it or what it
// holds. A path that does not exist yet is judged by its nearest existing
// ancestor, since whoever can create children there can create it first.
// Every ancestor above the judged path, up to the volume root, must also be
// a real directory with a trusted owner whose DACL grants nobody else a right
// that could move the path aside and let them plant their own. Paths must be
// absolute.
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
	sid, err := resolveUser(account)
	if err != nil {
		return nil, err
	}
	return trustedSet(sid)
}

// trustedSet is SYSTEM, Administrators, TrustedInstaller and, when non-nil,
// account.
func trustedSet(account *windows.SID) ([]*windows.SID, error) {
	sids, err := protectedTrustees(account)
	if err != nil {
		return nil, err
	}
	ti, err := windows.StringToSid(trustedInstallerSID)
	if err != nil {
		return nil, fmt.Errorf("TrustedInstaller SID: %w", err)
	}
	return append(sids, ti), nil
}

// ResolveUser checks that account, normalized as NormalizeAccount does,
// resolves to a user account's SID; "" and LocalSystem pass. It is the check
// CheckPaths, PrepareConfigDir and PrepareDataDir make before using account,
// for a caller that must refuse an account before changing anything.
func ResolveUser(account string) error {
	_, err := resolveUser(account)
	return err
}

// resolveUser is account's SID, normalized as `service install --user`
// normalizes it, or nil when it is "" (LocalSystem). account must name a
// user: trusting or granting a group would cover every member of it.
func resolveUser(account string) (*windows.SID, error) {
	if account = NormalizeAccount(account); account == "" {
		return nil, nil
	}
	sid, _, typ, err := windows.LookupSID("", lookupName(account))
	if err != nil {
		return nil, fmt.Errorf("look up account %s: %w", account, err)
	}
	if typ != windows.SidTypeUser {
		return nil, fmt.Errorf("account %s is not a user account", account)
	}
	return sid, nil
}

// checkPaths is CheckPaths with the trusted set given. Refs share ancestors
// (%ProgramData%\sqi, %ProgramData%, the volume root), so each ancestor is
// read and judged once per call.
func checkPaths(refs []PathRef, trusted []*windows.SID) error {
	var failures []PathFailure
	ancestors := map[string][]string{}
	for _, ref := range refs {
		f, err := judgeRef(ref, trusted, ancestors)
		if err != nil {
			return fmt.Errorf("check %s %s: %w", ref.Key, ref.Path, err)
		}
		failures = append(failures, f...)
	}
	if len(failures) > 0 {
		return &UntrustedPathsError{Failures: failures}
	}
	return nil
}

// judgeRef returns ref's failures: the judged path's, then each failing
// ancestor's, nearest first. ancestors holds the reasons of each ancestor
// already judged, by path, and gains those judgeRef judges.
func judgeRef(ref PathRef, trusted []*windows.SID, ancestors map[string][]string) ([]PathFailure, error) {
	judged, reasons, err := judgePath(ref.Path, trusted)
	if err != nil {
		return nil, err
	}
	var failures []PathFailure
	if len(reasons) > 0 {
		reason := strings.Join(reasons, "; ")
		if judged != filepath.Clean(ref.Path) {
			reason = fmt.Sprintf("does not exist; its nearest existing ancestor %s: %s", judged, reason)
		}
		failures = append(failures, PathFailure{Key: ref.Key, Path: ref.Path, JudgedPath: judged, Reason: reason})
	}
	for p := judged; filepath.Dir(p) != p; {
		p = filepath.Dir(p)
		reasons, seen := ancestors[p]
		if !seen {
			var err error
			if reasons, err = judgeExisting(p, trusted, ancestorRuleFor(p)); err != nil {
				return nil, err
			}
			ancestors[p] = reasons
		}
		if len(reasons) > 0 {
			failures = append(failures, PathFailure{
				Key: ref.Key, Path: ref.Path, JudgedPath: p,
				Reason: fmt.Sprintf("ancestor %s: %s", p, strings.Join(reasons, "; ")),
			})
		}
	}
	return failures, nil
}

// judgePath finds the path to judge — path, or its nearest existing ancestor
// — and returns it with the reasons it fails by objectRule, none when it
// passes. It does not judge the ancestors above it.
func judgePath(path string, trusted []*windows.SID) (judged string, reasons []string, err error) {
	if !filepath.IsAbs(path) {
		return path, nil, fmt.Errorf("%q is not an absolute path", path)
	}
	judged = filepath.Clean(path)
	for {
		reasons, err := judgeExisting(judged, trusted, objectRule)
		if !isNotFound(err) {
			return judged, reasons, err
		}
		parent := filepath.Dir(judged)
		if parent == judged {
			return judged, nil, fmt.Errorf("no part of %s exists", path)
		}
		judged = parent
	}
}

// judgeExisting opens path itself, never a reparse point's target, and
// returns the reasons it fails rule: it is a reparse point, or its owner or
// DACL, read from that handle, fail. The open error is wrapped, so a caller
// can test for a missing path.
func judgeExisting(path string, trusted []*windows.SID, rule rightsRule) ([]string, error) {
	h, _, err := openPlain(path, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES)
	switch {
	case errors.Is(err, errReparsePoint):
		return []string{errReparsePoint.Error()}, nil
	case err != nil:
		return nil, err
	}
	defer windows.CloseHandle(h) //nolint:errcheck // best-effort close of a handle we are done with
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, fmt.Errorf("read security of %s: %w", path, err)
	}
	reasons, err := judgeSecurity(sd, trusted, rule)
	if err != nil {
		return nil, fmt.Errorf("read security of %s: %w", path, err)
	}
	return reasons, nil
}

func isNotFound(err error) bool {
	return errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}

// judgeSecurity returns the reasons sd's owner and DACL fail rule, none when
// they pass.
func judgeSecurity(sd *windows.SECURITY_DESCRIPTOR, trusted []*windows.SID, rule rightsRule) ([]string, error) {
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
	more, err := judgeDACL(dacl, trusted, rule)
	if err != nil {
		return nil, err
	}
	return append(reasons, more...), nil
}

// judgeDACL returns a reason for each untrusted principal that an allow ACE
// applying to the object itself grants a right in rule, naming every such
// right it holds, and one for each ACE of a type it cannot read. Deny ACEs
// only take access away, so they never fail a path.
func judgeDACL(dacl *windows.ACL, trusted []*windows.SID, rule rightsRule) ([]string, error) {
	var (
		holders []*windows.SID
		masks   []windows.ACCESS_MASK
		reasons []string
	)
	for e, err := range objectACEs(dacl) {
		if err != nil {
			return nil, err
		}
		switch e.typ {
		case windows.ACCESS_DENIED_ACE_TYPE, accessDeniedObjectACEType,
			accessDeniedCallbackACEType, accessDeniedCallbackObjectACEType:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE, accessAllowedCallbackACEType:
		default:
			// Object, compound and anything newer: its SID is not where an
			// ACCESS_ALLOWED_ACE keeps it, so fail closed.
			reasons = append(reasons, fmt.Sprintf("has an access control entry of unsupported type %d", e.typ))
			continue
		}
		if e.mask&rule.mask == 0 || isTrusted(e.sid, trusted) {
			continue
		}
		if j := slices.IndexFunc(holders, e.sid.Equals); j >= 0 {
			masks[j] |= e.mask & rule.mask
			continue
		}
		holders = append(holders, e.sid)
		masks = append(masks, e.mask&rule.mask)
	}
	for j, sid := range holders {
		reasons = append(reasons, fmt.Sprintf("%s %s (%s)", accountName(sid), rule.verb, rightsString(masks[j])))
	}
	return reasons, nil
}

func isTrusted(sid *windows.SID, trusted []*windows.SID) bool {
	return slices.ContainsFunc(trusted, sid.Equals)
}

// rightsString lists the rights in mask by their icacls abbreviations.
func rightsString(mask windows.ACCESS_MASK) string {
	var abbrs []string
	for _, r := range rightNames {
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
