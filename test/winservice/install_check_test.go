// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build winservice && windows

package winservice

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
)

// unreachableNATS is a NATS URL nothing listens on: these tests install
// services but never start them.
const unreachableNATS = "nats://127.0.0.1:1"

// usersCanWrite is how a refusal names a path BUILTIN\Users can change.
const usersCanWrite = `BUILTIN\Users can write`

// TestWinService_ConfigInitCreatesProtectedDir pins that config init creates
// a missing configuration directory owned by Administrators with a protected
// DACL of SYSTEM and Administrators only, and writes a file the worker loads.
func TestWinService_ConfigInitCreatesProtectedDir(t *testing.T) {
	dir := filepath.Join(testDir(t), "new")
	cfg := filepath.Join(dir, "sqi-worker.yaml")
	mustRun(t, bin("sqi-worker"), "config", "init", "--nats-url", unreachableNATS, "--config", cfg)

	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if owner, _, err := sd.Owner(); err != nil || !owner.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
		t.Errorf("owner of %s = %v (%v), want BUILTIN\\Administrators", dir, owner, err)
	}
	if control, _, err := sd.Control(); err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Errorf("DACL of %s is not protected (control %#x, %v)", dir, control, err)
	}
	if got := allowedSIDs(t, sd); !sameSIDs(got, windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid) {
		t.Errorf("DACL of %s allows %v, want exactly SYSTEM and Administrators", dir, got)
	}
	mustRun(t, bin("sqi-worker"), "start", "--dry-run", "--config", cfg)
}

// TestWinService_ConfigInitRefusesSquattedDir pins that config init refuses
// an existing configuration directory a standard account owns, and leaves its
// security as it found it.
func TestWinService_ConfigInitRefusesSquattedDir(t *testing.T) {
	account, _ := throwawayAccount(t)
	dir := testDir(t)
	sid := accountSID(t, account)
	if err := setOwner(dir, sid); err != nil {
		t.Fatal(err)
	}
	before := securityString(t, dir)
	cfg := filepath.Join(dir, "sqi-worker.yaml")

	out, err := run(t.Context(), "", bin("sqi-worker"), "config", "init", "--nats-url", unreachableNATS, "--config", cfg)
	if want := "owner " + qualified(t, account) + " is not trusted"; err == nil ||
		!strings.Contains(strings.ToLower(out), strings.ToLower(want)) {
		t.Fatalf("config init in a directory %s owns: err=%v, want a refusal containing %q\n%s", account, err, want, out)
	}
	if after := securityString(t, dir); after != before {
		t.Errorf("config init changed the security of %s:\nbefore %s\nafter  %s", dir, before, after)
	}
	if _, err := os.Lstat(cfg); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("config init wrote %s despite refusing (%v)", cfg, err)
	}
}

// TestWinService_InstallRefusesUserWritableConfigDir pins that install
// refuses a configuration directory any user can write, as one made by hand
// under %ProgramData% is, and registers nothing.
func TestWinService_InstallRefusesUserWritableConfigDir(t *testing.T) {
	dir := userWritableDir(t)
	ports := freePorts(t, 2)
	cfg := filepath.Join(dir, "sqi-server.yaml")
	writeFile(t, cfg, serverYAML(ports[0], ports[1], ""))
	name := uniqueName(t, "sqi-test-open")
	cleanupService(t, bin("sqi-server"), name)

	out, err := run(t.Context(), "", bin("sqi-server"), "service", "install", "--name", name, "--config", cfg)
	requireRefusal(t, err, out, "config directory "+dir+": ", usersCanWrite)
	requireNotInstalled(t, bin("sqi-server"), name)
}

// TestWinService_InstallRefusesUserWritableBinary pins that install refuses
// a binary in a directory any user can write: they could replace what
// LocalSystem runs.
func TestWinService_InstallRefusesUserWritableBinary(t *testing.T) {
	exe := filepath.Join(userWritableDir(t), "sqi-worker.exe")
	b, err := os.ReadFile(bin("sqi-worker"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, b, 0o600); err != nil { // Windows ignores the mode's execute bits
		t.Fatal(err)
	}
	dir := testDir(t)
	cfg := filepath.Join(dir, "sqi-worker.yaml")
	writeFile(t, cfg, workerYAML(t, unreachableNATS, filepath.Join(dir, "data")))
	name := uniqueName(t, "sqi-test-bin")
	cleanupService(t, bin("sqi-worker"), name)

	out, err := run(t.Context(), "", exe, "service", "install", "--name", name, "--config", cfg)
	requireRefusal(t, err, out, "binary directory "+filepath.Dir(exe)+": ", usersCanWrite)
	requireNotInstalled(t, bin("sqi-worker"), name)
}

// TestWinService_InstallRefusesUserWritableTLSCA pins that install refuses a
// configuration naming a NATS CA any user can replace: they could point the
// worker at their own server.
func TestWinService_InstallRefusesUserWritableTLSCA(t *testing.T) {
	ca := filepath.Join(userWritableDir(t), "ca.pem")
	writeFile(t, ca, "not checked before the refusal\n")
	// A file made in such a directory inherits only read for Users; this one
	// is theirs to replace, as a CA copied in by hand and then edited may be.
	if out, err := run(t.Context(), "", "icacls", ca, "/grant", "*"+usersSID+":M"); err != nil {
		t.Fatalf("icacls: %v\n%s", err, out)
	}
	dir := testDir(t)
	cfg := filepath.Join(dir, "sqi-worker.yaml")
	writeFile(t, cfg, "nats:\n  url: "+strconv.Quote(unreachableNATS)+
		"\n  tls_ca_file: "+strconv.Quote(ca)+
		"\ndiscovery:\n  enable_mdns: false\nworker:\n  data_dir: "+strconv.Quote(filepath.Join(dir, "data"))+"\n")
	name := uniqueName(t, "sqi-test-ca")
	cleanupService(t, bin("sqi-worker"), name)

	out, err := run(t.Context(), "", bin("sqi-worker"), "service", "install", "--name", name, "--config", cfg)
	requireRefusal(t, err, out, "nats.tls_ca_file "+ca+": ", usersCanWrite)
	requireNotInstalled(t, bin("sqi-worker"), name)
}

// TestWinService_InstallRefusesSquattedSqiDirWithConfigElsewhere pins that
// install refuses an existing %ProgramData%\sqi any user can write even when
// the configuration lives elsewhere: the service still logs there as
// LocalSystem. %ProgramData% is redirected, for the install only, to a
// directory made here, so the real one is never touched. install leaves that
// sqi as it found it and prepares no log directory in it.
func TestWinService_InstallRefusesSquattedSqiDirWithConfigElsewhere(t *testing.T) {
	root := userWritableDir(t)
	sqi := filepath.Join(root, "sqi")
	if err := os.Mkdir(sqi, 0o750); err != nil { // inherits ProgramData's Users write
		t.Fatal(err)
	}
	before := securityString(t, sqi)
	ports := freePorts(t, 2)
	cfg := filepath.Join(testDir(t), "sqi-server.yaml")
	writeFile(t, cfg, serverYAML(ports[0], ports[1], ""))
	name := uniqueName(t, "sqi-test-squat")
	cleanupService(t, bin("sqi-server"), name)

	out, err := runEnv(t.Context(), []string{"ProgramData=" + root}, "",
		bin("sqi-server"), "service", "install", "--name", name, "--config", cfg)
	// The default log directory's parent, sqi\logs, does not exist yet: it is
	// judged by sqi.
	requireRefusal(t, err, out, "default log directory parent "+filepath.Join(sqi, "logs")+": ", usersCanWrite)
	requireNotInstalled(t, bin("sqi-server"), name)
	if after := securityString(t, sqi); after != before {
		t.Errorf("install changed the security of %s:\nbefore %s\nafter  %s", sqi, before, after)
	}
	if _, err := os.Lstat(filepath.Join(sqi, "logs")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("install prepared a log directory in the refused %s (%v)", sqi, err)
	}
}

// TestWinService_ConfigInitUserAccountCanReadAndWrite pins what config init
// --user gives the account, checked as the account itself: it reads the
// configuration but cannot change it, and writes its data directory and the
// session root the configuration implies, as every task needs. install
// --user then accepts the layout.
func TestWinService_ConfigInitUserAccountCanReadAndWrite(t *testing.T) {
	account, password := throwawayAccount(t)
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(os.Getenv("ProgramData"), "sqi winservice user "+hex.EncodeToString(b))
	cleanupDir(t, dir)
	cfg := filepath.Join(dir, "sqi-worker.yaml")
	mustRun(t, bin("sqi-worker"), "config", "init", "--user", account, "--nats-url", unreachableNATS, "--config", cfg)
	// The session root the worker would use with the written configuration
	// (cmd/sqi-worker's default on Windows when worker.session_dir is unset).
	loaded, err := workerconfig.Load(cfg, workerconfig.FlagOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	sessionRoot := loaded.Worker.SessionDir
	if sessionRoot == "" {
		sessionRoot = filepath.Join(os.Getenv("ProgramData"), "sqi", "worker", "sessions")
	}

	token := logon(t, account, password)
	err = asAccount(token, func() error {
		var errs []error
		// As session.Manager does for each task: the root, then a session
		// directory and a file in it.
		session := filepath.Join(sessionRoot, "probe-session")
		if err := os.MkdirAll(session, 0o700); err != nil {
			errs = append(errs, fmt.Errorf("create a session directory: %w", err))
		} else if err := os.WriteFile(filepath.Join(session, "probe"), []byte("x"), 0o600); err != nil {
			errs = append(errs, fmt.Errorf("write in a session directory: %w", err))
		}
		if _, err := os.ReadFile(cfg); err != nil {
			errs = append(errs, fmt.Errorf("read the configuration: %w", err))
		}
		switch f, err := os.OpenFile(cfg, os.O_WRONLY|os.O_APPEND, 0); {
		case err == nil:
			f.Close()
			errs = append(errs, errors.New("opened the configuration for writing"))
		case !errors.Is(err, fs.ErrPermission):
			errs = append(errs, fmt.Errorf("open the configuration for writing: %w, want access denied", err))
		}
		if err := os.WriteFile(filepath.Join(dir, "worker", "probe"), []byte("x"), 0o600); err != nil {
			errs = append(errs, fmt.Errorf("write the data directory: %w", err))
		}
		return errors.Join(errs...)
	})
	if err != nil {
		t.Errorf("as %s: %v", account, err)
	}

	name := uniqueName(t, "sqi-test-init-user")
	cleanupService(t, bin("sqi-worker"), name)
	if out, err := run(t.Context(), password+"\n", bin("sqi-worker"), "service", "install", "--name", name,
		"--config", cfg, "--user", account); err != nil {
		t.Fatalf("install --user over the config init --user layout: %v\n%s", err, out)
	}
}

// userWritableDir is a directory under %ProgramData% made as an operator's
// own mkdir makes one: inheriting ProgramData's DACL, which lets every user
// create and write files in it. It is removed when the test ends.
func userWritableDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(os.Getenv("ProgramData"), "sqi winservice open ") //nolint:usetesting // must inherit %ProgramData%'s DACL
	if err != nil {
		t.Fatal(err)
	}
	cleanupDir(t, dir)
	return dir
}

// requireRefusal fails unless install failed with the check's refusal and a
// line of it starting with failure ("<key> <path>: ") holds every one of
// reasons.
func requireRefusal(t *testing.T, err error, out, failure string, reasons ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("install succeeded, want a refusal:\n%s", out)
	}
	if !strings.Contains(out, "refusing to install") {
		t.Fatalf("install failed but not with the check's refusal:\n%s", out)
	}
	for line := range strings.Lines(out) {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, failure) &&
			!slices.ContainsFunc(reasons, func(r string) bool { return !strings.Contains(line, r) }) {
			return
		}
	}
	t.Errorf("refusal has no line starting %q that holds %q:\n%s", failure, reasons, out)
}

// requireNotInstalled fails unless the SCM has no service name.
func requireNotInstalled(t *testing.T, exe, name string) {
	t.Helper()
	if out, err := run(t.Context(), "", exe, "service", "status", "--name", name); err == nil ||
		!strings.Contains(out, serviceNotFound) {
		t.Errorf("service %s was registered despite the refusal: err=%v\n%s", name, err, out)
	}
}

// qualified is account as SID lookups name it, HOST\account.
func qualified(t *testing.T, account string) string {
	t.Helper()
	host, err := windows.ComputerName()
	if err != nil {
		t.Fatal(err)
	}
	return host + `\` + account
}

func accountSID(t *testing.T, account string) *windows.SID {
	t.Helper()
	sid, _, _, err := windows.LookupSID("", qualified(t, account))
	if err != nil {
		t.Fatalf("look up %s: %v", account, err)
	}
	return sid
}

// securityString is path's owner and DACL in SDDL.
func securityString(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return sd.String()
}

// allowedSIDs is the SID of every allow ACE in sd's DACL; it fails on any
// other kind of ACE.
func allowedSIDs(t *testing.T, sd *windows.SECURITY_DESCRIPTOR) []*windows.SID {
	t.Helper()
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("read DACL: %v (nil: %t)", err, dacl == nil)
	}
	sids := make([]*windows.SID, 0, dacl.AceCount)
	for i := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatalf("read ACE %d: %v", i, err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Fatalf("ACE %d has type %d, want only allow ACEs", i, ace.Header.AceType)
		}
		sids = append(sids, (*windows.SID)(unsafe.Pointer(&ace.SidStart)))
	}
	return sids
}

// sqiDirTraverse is the one right a --user account gets on each shared
// directory above its own log directory: read attributes, traverse and
// synchronize.
const sqiDirTraverse = windows.ACCESS_MASK(windows.FILE_READ_ATTRIBUTES | windows.FILE_TRAVERSE | windows.SYNCHRONIZE)

// inheritFlags are the ACE flags that pass an entry on to children.
const inheritFlags = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE

// aceFor is every allow ACE in path's DACL for sid.
func aceFor(t *testing.T, path string, sid *windows.SID) []*windows.ACCESS_ALLOWED_ACE {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("read DACL of %s: %v (nil: %t)", path, err, dacl == nil)
	}
	var out []*windows.ACCESS_ALLOWED_ACE
	for i := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatalf("read ACE %d of %s: %v", i, path, err)
		}
		if ace.Header.AceType == windows.ACCESS_ALLOWED_ACE_TYPE && (*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(sid) {
			out = append(out, ace)
		}
	}
	return out
}

// sameSIDs reports whether got holds each of want, and nothing else.
func sameSIDs(got []*windows.SID, want ...windows.WELL_KNOWN_SID_TYPE) bool {
	is := func(w windows.WELL_KNOWN_SID_TYPE) func(*windows.SID) bool {
		return func(sid *windows.SID) bool { return sid.IsWellKnown(w) }
	}
	for _, w := range want {
		if !slices.ContainsFunc(got, is(w)) {
			return false
		}
	}
	return !slices.ContainsFunc(got, func(sid *windows.SID) bool {
		return !slices.ContainsFunc(want, func(w windows.WELL_KNOWN_SID_TYPE) bool { return sid.IsWellKnown(w) })
	})
}

// setOwner makes owner the owner of path. Giving an object to another
// account needs SeRestorePrivilege, which an elevated token holds but has not
// enabled; it is enabled on a thread of its own, so the test process's token
// is left as it was.
func setOwner(path string, owner *windows.SID) error {
	return onOwnThread(func() error {
		if err := windows.ImpersonateSelf(windows.SecurityImpersonation); err != nil {
			return fmt.Errorf("impersonate self: %w", err)
		}
		var token windows.Token
		if err := windows.OpenThreadToken(windows.CurrentThread(),
			windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, false, &token); err != nil {
			return fmt.Errorf("open thread token: %w", err)
		}
		defer token.Close()
		name, err := windows.UTF16PtrFromString("SeRestorePrivilege")
		if err != nil {
			return err
		}
		privs := windows.Tokenprivileges{PrivilegeCount: 1}
		privs.Privileges[0].Attributes = windows.SE_PRIVILEGE_ENABLED
		if err := windows.LookupPrivilegeValue(nil, name, &privs.Privileges[0].Luid); err != nil {
			return fmt.Errorf("look up SeRestorePrivilege: %w", err)
		}
		if err := windows.AdjustTokenPrivileges(token, false, &privs, 0, nil, nil); err != nil {
			return fmt.Errorf("enable SeRestorePrivilege: %w", err)
		}
		if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION,
			owner, nil, nil, nil); err != nil {
			return fmt.Errorf("set the owner of %s: %w", path, err)
		}
		return nil
	})
}

var procLogonUserW = modAdvapi32.NewProc("LogonUserW")

const (
	logon32LogonInteractive = 2 // LOGON32_LOGON_INTERACTIVE: Users hold "Allow log on locally"
	logon32ProviderDefault  = 0
)

// logon logs account on interactively and returns its primary token, closed
// when the test ends.
func logon(t *testing.T, account, password string) windows.Token {
	t.Helper()
	u, err := windows.UTF16PtrFromString(account)
	if err != nil {
		t.Fatal(err)
	}
	d, err := windows.UTF16PtrFromString(".")
	if err != nil {
		t.Fatal(err)
	}
	p, err := windows.UTF16PtrFromString(password)
	if err != nil {
		t.Fatal(err)
	}
	var token windows.Token
	r1, _, e1 := procLogonUserW.Call(uintptr(unsafe.Pointer(u)), uintptr(unsafe.Pointer(d)), uintptr(unsafe.Pointer(p)),
		logon32LogonInteractive, logon32ProviderDefault, uintptr(unsafe.Pointer(&token)))
	if r1 == 0 {
		t.Fatalf("log on as %s: %v", account, e1)
	}
	t.Cleanup(func() { token.Close() })
	return token
}

// asAccount runs fn impersonating token's account.
func asAccount(token windows.Token, fn func() error) error {
	return onOwnThread(func() error {
		var imp windows.Token
		if err := windows.DuplicateTokenEx(token, windows.MAXIMUM_ALLOWED, nil,
			windows.SecurityImpersonation, windows.TokenImpersonation, &imp); err != nil {
			return fmt.Errorf("duplicate token: %w", err)
		}
		defer imp.Close()
		if err := windows.SetThreadToken(nil, imp); err != nil {
			return fmt.Errorf("impersonate: %w", err)
		}
		return fn()
	})
}

// onOwnThread runs fn on a goroutine locked to its OS thread and never
// unlocked, so whatever fn leaves on the thread — an impersonation token, an
// enabled privilege — ends with it: Go terminates a thread whose goroutine
// exits while locked to it.
func onOwnThread(fn func() error) error {
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		done <- fn()
	}()
	return <-done
}
