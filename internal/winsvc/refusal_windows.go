// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package winsvc

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// installRefs is every path `service install` checks for the service name
// whose configuration is cfgPath and binary exe, in order: the configuration
// and its directory, the binary and its directory, log.file's directory and
// its parent when log.file is set, the service's own default log directory
// and its parent (the logs directory every service's own one sits in), then
// each path the configuration names. Another service's log directory is
// never judged: what its account was granted there reaches nothing of this
// one's. A relative path is resolved
// against the configuration's directory, which the service runs in.
//
// The default log directory is checked whatever log.file says: install
// prepares it (ensureLogDir) and the service writes its trace log there. It
// is left out where an earlier ref already names the same path.
func installRefs(cfgPath, exe, name string, cp ConfigPaths) []PathRef {
	base := filepath.Dir(cfgPath)
	refs := make([]PathRef, 0, 8+len(cp.Paths))
	refs = append(
		refs,
		PathRef{"config", cfgPath},
		PathRef{"config directory", base},
		PathRef{"binary", exe},
		PathRef{"binary directory", filepath.Dir(exe)},
	)
	if cp.LogFile != "" {
		logDir := filepath.Dir(resolveAgainst(base, cp.LogFile))
		refs = append(refs, PathRef{"log directory", logDir}, PathRef{"log directory parent", filepath.Dir(logDir)})
	}
	defaultLogDir := filepath.Dir(DefaultLogPath(name))
	for _, ref := range []PathRef{
		{"default log directory", defaultLogDir},
		{"default log directory parent", filepath.Dir(defaultLogDir)},
	} {
		if !slices.ContainsFunc(refs, func(r PathRef) bool { return strings.EqualFold(r.Path, ref.Path) }) {
			refs = append(refs, ref)
		}
	}
	for _, p := range cp.Paths {
		refs = append(refs, PathRef{p.Key, resolveAgainst(base, p.Path)})
	}
	return refs
}

// resolveAgainst is path as a process whose working directory is base (an
// absolute path with a drive) opens it. A rooted path without a drive is on
// base's drive. A path relative to another drive's current directory starts
// at that drive's root: a service inherits no current directory there.
func resolveAgainst(base, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	vol := filepath.VolumeName(path)
	rest := path[len(vol):]
	switch {
	case vol == "" && rest != "" && os.IsPathSeparator(rest[0]):
		return filepath.Join(filepath.VolumeName(base), rest)
	case vol == "" || strings.EqualFold(vol, filepath.VolumeName(base)):
		return filepath.Join(base, rest)
	default:
		return filepath.Join(vol+`\`, rest)
	}
}

// untrustedError explains a refusal of a service to run as account ("" for
// LocalSystem): what CheckPaths found, then the way out for each kind of
// failure. It wraps u.
func (c commands) untrustedError(u *UntrustedPathsError, exe, account string) error {
	var remedies []string
	if slices.ContainsFunc(u.Failures, isBinaryFailure) {
		dest := filepath.Join(programFilesDir(), "sqi")
		remedies = append(remedies, fmt.Sprintf(
			"Copy %s into a directory only administrators can change, such as %s, from an elevated PowerShell "+
				"(files an elevated Git Bash creates are owned by your own account, which this check refuses), "+
				"and run service install with that copy:\n"+
				"  New-Item -ItemType Directory -Force -Path '%s'\n"+
				"  Copy-Item -LiteralPath '%s' -Destination '%s'",
			filepath.Base(exe), dest, QuotePowerShell(dest), QuotePowerShell(exe), QuotePowerShell(dest),
		))
	}
	suspects, other := programDataSuspects(u)
	if len(suspects) > 0 {
		remedies = append(remedies, c.moveAsideRemedy(u, suspects))
	}
	if other {
		which := "each path above"
		if len(remedies) > 0 {
			which = "each other path above"
		}
		remedies = append(remedies, "Make "+which+" administrators-only (owned by SYSTEM or "+
			"Administrators, changeable by nobody else, and in directories nobody else can change), "+
			"or point its setting at one that is, then run service install again.")
	}
	if account == "" {
		if hint := c.userHint(u); hint != "" {
			remedies = append(remedies, hint)
		}
	}
	return fmt.Errorf("refusing to install %s: someone other than SYSTEM, Administrators and the service's "+
		"account can change what it would trust or run:\n%w\n%s", c.spec.Binary, u, strings.Join(remedies, "\n"))
}

func isBinaryFailure(f PathFailure) bool { return f.Key == "binary" || f.Key == "binary directory" }

// installKeys are the refs install adds itself rather than reads from the
// configuration (see installRefs).
var installKeys = []string{
	"config", "config directory", "binary", "binary directory",
	"log directory", "log directory parent", "default log directory", "default log directory parent",
}

// userHint is a line for a LocalSystem refusal of a path the configuration
// names that grants a user account Modify, as config init --user lays out a
// data and session directory: the configuration looks written for that
// account. It is a best-effort reading of each such path's DACL; "" when it
// finds no such account.
func (c commands) userHint(u *UntrustedPathsError) string {
	var users []string
	for _, f := range u.Failures {
		if slices.Contains(installKeys, f.Key) {
			continue
		}
		for _, name := range modifyUsers(f.JudgedPath) {
			if !slices.ContainsFunc(users, func(s string) bool { return strings.EqualFold(s, name) }) {
				users = append(users, name)
			}
		}
	}
	if len(users) == 0 {
		return ""
	}
	return fmt.Sprintf("This configuration looks like it was written for --user %s (config init --user gives the "+
		"account Modify on its data directory). If so, install it with: %s service install --user %s",
		strings.Join(users, " or --user "), c.spec.Binary, users[0])
}

// modifyUsers is the name of each user account, other than SYSTEM,
// Administrators and TrustedInstaller, that path's DACL grants at least
// Modify through allow ACEs applying to path itself; nil when it cannot be
// read.
func modifyUsers(path string) []string {
	h, _, err := openPlain(path, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(h) //nolint:errcheck // best-effort close of a handle we are done with
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return nil
	}
	sids, masks, err := untrustedAllows(dacl)
	if err != nil {
		return nil
	}
	var names []string
	for j, sid := range sids {
		if masks[j]&fileModifyAccess != fileModifyAccess {
			continue
		}
		if _, _, typ, err := sid.LookupAccount(""); err == nil && typ == windows.SidTypeUser {
			names = append(names, accountName(sid))
		}
	}
	return names
}

// untrustedAllows is each principal other than SYSTEM, Administrators and
// TrustedInstaller that an allow ACE applying to the object itself names in
// dacl, with every right those ACEs grant it.
func untrustedAllows(dacl *windows.ACL) ([]*windows.SID, []windows.ACCESS_MASK, error) {
	trusted, err := trustedSet(nil)
	if err != nil {
		return nil, nil, err
	}
	var sids []*windows.SID
	var masks []windows.ACCESS_MASK
	for i := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return nil, nil, err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if isTrusted(sid, trusted) {
			continue
		}
		if j := slices.IndexFunc(sids, sid.Equals); j >= 0 {
			masks[j] |= ace.Mask
			continue
		}
		sids = append(sids, sid)
		masks = append(masks, ace.Mask)
	}
	return sids, masks, nil
}

// programDataSuspects is the distinct judged paths under %ProgramData% (never
// %ProgramData% itself) of every failure but the binary's, which moving
// aside replaces with directories config init or the service creates again.
// other reports a failure, again not the binary's, that moving cannot fix.
func programDataSuspects(u *UntrustedPathsError) (suspects []string, other bool) {
	pd := filepath.Clean(ProgramDataDir())
	for _, f := range u.Failures {
		if isBinaryFailure(f) {
			continue
		}
		judged := filepath.Clean(f.JudgedPath)
		if !PathWithin(judged, pd) || strings.EqualFold(judged, pd) {
			other = true
			continue
		}
		if !slices.ContainsFunc(suspects, func(s string) bool { return strings.EqualFold(s, judged) }) {
			suspects = append(suspects, judged)
		}
	}
	return suspects, other
}

// moveAsideRemedy tells the operator to rename each suspect, and to write a
// new configuration when the configuration was refused.
func (c commands) moveAsideRemedy(u *UntrustedPathsError, suspects []string) string {
	var sb strings.Builder
	sb.WriteString("Move what someone else could change aside from an elevated PowerShell:\n")
	for _, s := range suspects {
		fmt.Fprintf(&sb, "  Rename-Item -LiteralPath '%s' -NewName ('%s.suspect-' + (Get-Date -Format yyyyMMddHHmmss))\n",
			QuotePowerShell(s), QuotePowerShell(filepath.Base(s)))
	}
	if slices.ContainsFunc(u.Failures, func(f PathFailure) bool { return f.Key == "config" || f.Key == "config directory" }) {
		fmt.Fprintf(&sb, "then write a new configuration with %s config init, and run service install again.", c.spec.Binary)
	} else {
		sb.WriteString("then run service install again.")
	}
	return sb.String()
}

// programFilesDir is %ProgramFiles%, or C:\Program Files when it is unset.
func programFilesDir() string {
	if d := os.Getenv("ProgramFiles"); d != "" {
		return d
	}
	return `C:\Program Files`
}

// PathWithin reports whether dir is root or inside it, on whole path
// components and ignoring case, as Windows compares paths.
func PathWithin(dir, root string) bool {
	d, r := filepath.Clean(dir), filepath.Clean(root)
	if strings.EqualFold(d, r) {
		return true
	}
	if !strings.HasSuffix(r, `\`) {
		r += `\`
	}
	return len(d) > len(r) && strings.EqualFold(d[:len(r)], r)
}

// QuotePowerShell escapes s for a PowerShell single-quoted string, which also
// ends at the typographic single quotes.
func QuotePowerShell(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch r {
		case '\'', '\u2018', '\u2019', '\u201a', '\u201b':
			sb.WriteRune(r)
		}
		sb.WriteRune(r)
	}
	return sb.String()
}
