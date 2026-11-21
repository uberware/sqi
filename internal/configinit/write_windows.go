// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package configinit

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/uberware/sqi/internal/winsvc"
)

// winOps is every step writeWindows takes that checks or changes the
// system, so a test can record them.
type winOps struct {
	requireElevated  func() error
	lookupAccount    func(account string) error
	prepareConfigDir func(dir, account string) error
	prepareDataDir   func(dir, account string) error
	checkPaths       func([]winsvc.PathRef, string) error
	dirExists        func(dir string) (bool, error)
	writeFile        func(target string, content []byte, replace, adminOwned bool) error
}

func realWinOps() winOps {
	return winOps{
		requireElevated:  winsvc.RequireElevated,
		lookupAccount:    winsvc.ResolveUser,
		prepareConfigDir: winsvc.PrepareConfigDir,
		prepareDataDir:   winsvc.PrepareDataDir,
		checkPaths:       winsvc.CheckPaths,
		dirExists:        dirExists,
		writeFile:        writeFile,
	}
}

// platformWriter writes through the real file system and security APIs.
func platformWriter(_ Spec) writer {
	return windowsWriter(realWinOps())
}

func windowsWriter(ops winOps) writer {
	return func(req writeRequest) error {
		return writeWindows(req.Target, req.Content, req.Account, req.Force, req.DataDir, ops)
	}
}

// writeWindows puts content at target. A directory under %ProgramData%, a
// missing one, or one for an account is made administrators-only (granting
// account read) or must already be, which needs an elevated shell, checked
// before anything else. An existing directory of one's own elsewhere is
// written into as it is. account, when not "", must resolve to a user, may
// not use the default %ProgramData%\sqi, and gets Modify on dataDir. target
// must be in canonicalPath's spelling, as run gives it, so the directory
// judged is the one written; %ProgramData% is compared in that spelling too.
func writeWindows(target string, content []byte, account string, force bool, dataDir string, ops winOps) error {
	pd, err := canonicalPath(winsvc.ProgramDataDir())
	if err != nil {
		return fmt.Errorf("%%ProgramData%%: %w", err)
	}
	dir := filepath.Dir(target)
	protect, err := needsProtection(dir, pd, account, ops)
	if err != nil {
		return err
	}
	if protect {
		if err := ops.requireElevated(); err != nil {
			return err
		}
	}
	if account != "" {
		if err := checkAccount(target, pd, account, ops); err != nil {
			return err
		}
	}
	if protect {
		if err := ops.prepareConfigDir(dir, account); err != nil {
			return refusal("cannot use configuration directory "+dir, dir, pd, err)
		}
	}
	replace, err := checkReplace(target, pd, account, force, protect, ops)
	if err != nil {
		return err
	}
	if account != "" {
		if err := ops.prepareDataDir(dataDir, account); err != nil {
			return refusal("cannot use data directory "+dataDir, dataDir, pd, err)
		}
	}
	return ops.writeFile(target, content, replace, protect)
}

// checkReplace reports whether a file at target is to be replaced: with
// force, when one is there. In a protected directory it must pass the check
// first. The new file replaces the old one rather than overwriting it, so it
// takes none of the old ACL; but a file someone else can change, in a
// directory that passed, means they once could write here, and what else
// they left is unknown. It is refused, never papered over.
func checkReplace(target, pd, account string, force, protect bool, ops winOps) (bool, error) {
	if !force {
		return false, nil
	}
	exists, err := fileExists(target)
	if err != nil || !exists || !protect {
		return exists, err
	}
	if err := ops.checkPaths([]winsvc.PathRef{{Key: "config", Path: target}}, account); err != nil {
		return false, refusal("cannot replace "+target, target, pd, err)
	}
	return true, nil
}

// needsProtection reports whether dir must be made, or already be,
// administrators-only: when it is under pd (%ProgramData%), is missing, or
// is for an account.
func needsProtection(dir, pd, account string, ops winOps) (bool, error) {
	if account != "" || underDir(dir, pd) {
		return true, nil
	}
	exists, err := ops.dirExists(dir)
	return !exists, err
}

// checkAccount refuses the default pd\sqi (pd is %ProgramData%) for an
// account, which would let it read every other service's configuration, and
// an account that does not resolve to a user.
func checkAccount(target, pd, account string, ops winOps) error {
	if strings.EqualFold(filepath.Dir(target), filepath.Join(pd, "sqi")) {
		return fmt.Errorf(`--user needs its own directory: pass --config, e.g. C:\ProgramData\sqi-render\%s`, filepath.Base(target))
	}
	return ops.lookupAccount(account)
}

// underDir reports whether dir is root or inside it, on whole path
// components and ignoring case, as Windows compares paths.
func underDir(dir, root string) bool {
	d, r := filepath.Clean(dir), filepath.Clean(root)
	if strings.EqualFold(d, r) {
		return true
	}
	if !strings.HasSuffix(r, `\`) {
		r += `\`
	}
	return len(d) > len(r) && strings.EqualFold(d[:len(r)], r)
}

// refusal adds to an *UntrustedPathsError about path the way out: moving
// path aside and running config init again, or choosing another --config
// when the failure is in a directory above it (or path is not one to move).
// Any other error is returned with what as its context.
func refusal(what, path, pd string, err error) error {
	u, ok := errors.AsType[*winsvc.UntrustedPathsError](err)
	if !ok {
		return fmt.Errorf("%s: %w", what, err)
	}
	self, above := false, false
	for _, f := range u.Failures {
		if strings.EqualFold(filepath.Clean(f.JudgedPath), path) {
			self = true
		} else {
			above = true
		}
	}
	var remedy string
	if self && movable(path, pd) {
		remedy = fmt.Sprintf("Someone other than SYSTEM and Administrators can change it. Move it aside from an elevated PowerShell:\n"+
			"  Rename-Item -LiteralPath '%s' -NewName ('%s.suspect-' + (Get-Date -Format yyyyMMddHHmmss))\n"+
			"then run config init again.", psQuote(path), psQuote(filepath.Base(path)))
	}
	if above || (self && !movable(path, pd)) {
		if remedy != "" {
			remedy += "\n"
		}
		remedy += "Pass --config naming another directory: it and every directory above it must pass this check."
	}
	return fmt.Errorf("%s:\n%w\n%s", what, err, remedy)
}

// movable reports whether config init may suggest renaming path: never pd
// (%ProgramData%) itself or a volume root.
func movable(path, pd string) bool {
	return filepath.Dir(path) != path && !strings.EqualFold(path, pd)
}

// psQuote escapes s for a PowerShell single-quoted string, which also ends
// at the typographic single quotes.
func psQuote(s string) string {
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

func dirExists(dir string) (bool, error) {
	st, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("check %s: %w", dir, err)
	case !st.IsDir():
		return false, fmt.Errorf("%s is not a directory", dir)
	}
	return true, nil
}

func fileExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("check %s: %w", path, err)
	}
}

// writeFile writes content to a new file beside target, which takes the
// directory's inheritable ACL, and moves it to target. With adminOwned the
// new file is owned by BUILTIN\Administrators, as a protected directory's
// are: an elevated token's default owner can be the user who started the
// shell. Without replace, a file already at target is left alone and
// refused; with it, it is replaced by the new file rather than overwritten,
// so it keeps none of its own ACL. A link at target is replaced, never
// followed.
func writeFile(target string, content []byte, replace, adminOwned bool) error {
	f, err := createTemp(filepath.Dir(target), adminOwned)
	if err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	tmp := f.Name()
	_, err = f.Write(content)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = moveFile(tmp, target, replace)
	}
	if err != nil {
		os.Remove(tmp) // best effort: the write already failed
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
			return fmt.Errorf("%s exists; pass --force to replace it", target)
		}
		return fmt.Errorf("write %s: %w", target, err)
	}
	return nil
}

// createTemp creates a new file in dir, owned by BUILTIN\Administrators when
// adminOwned (else by the token's default owner), with the DACL dir's
// inheritable ACEs give it.
func createTemp(dir string, adminOwned bool) (*os.File, error) {
	sd, err := windows.NewSecurityDescriptor()
	if err != nil {
		return nil, err
	}
	if adminOwned {
		admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
		if err != nil {
			return nil, fmt.Errorf("well-known SID: %w", err)
		}
		if err := sd.SetOwner(admins, false); err != nil {
			return nil, err
		}
	}
	sa := windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(sa))
	for range 10 {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		name := filepath.Join(dir, ".sqi-config-"+hex.EncodeToString(b[:]))
		p, err := windows.UTF16PtrFromString(name)
		if err != nil {
			return nil, err
		}
		h, err := windows.CreateFile(p, windows.GENERIC_WRITE, 0, &sa, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if errors.Is(err, windows.ERROR_FILE_EXISTS) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(h), name), nil
	}
	return nil, errors.New("no unused temporary file name")
}

func moveFile(from, to string, replace bool) error {
	src, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	dst, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	flags := uint32(windows.MOVEFILE_WRITE_THROUGH)
	if replace {
		flags |= windows.MOVEFILE_REPLACE_EXISTING
	}
	return windows.MoveFileEx(src, dst, flags)
}
