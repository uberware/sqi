// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package winsvc

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/uberware/sqi/internal/fsutil/fsutiltest"
)

// TestCreateLogDir_MissingDirGetsProtectedDACL pins that a service creating
// its own default log directory at runtime (a hand-registered service never
// ran `service install`) does not inherit C:\ProgramData's BUILTIN\Users
// access: the directory is protected, for SYSTEM, Administrators and the
// process's own account only.
func TestCreateLogDir_MissingDirGetsProtectedDACL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sqi", "logs")
	if err := createLogDir(dir); err != nil {
		t.Fatal(err)
	}
	acl, protected := daclOf(t, dir)
	if !protected {
		t.Error("a log directory created at runtime has an unprotected DACL")
	}
	me, _ := currentAccount(t)
	assertOnlyFullControl(t, aclEntries(t, acl), me)
}

// TestCreateLogDir_ExistingDirIsUntouched pins that an existing log
// directory's ACL is left alone.
func TestCreateLogDir_ExistingDirIsUntouched(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	before := securityOf(t, dir, windows.DACL_SECURITY_INFORMATION)
	if err := createLogDir(dir); err != nil {
		t.Fatal(err)
	}
	if after := securityOf(t, dir, windows.DACL_SECURITY_INFORMATION); after != before {
		t.Errorf("createLogDir changed an existing directory's DACL:\nbefore %s\nafter  %s", before, after)
	}
}

// TestCreateLogDir_RefusesExistingJunction pins that a running service
// refuses an existing junctioned log directory, as `service install` does: it
// would otherwise write, rename and delete its log files in the target. The
// target's DACL is never touched either.
func TestCreateLogDir_RefusesExistingJunction(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "logs")
	fsutiltest.Junction(t, link, target)
	before := daclBytes(t, target)
	if err := createLogDir(link); !errors.Is(err, errReparsePoint) {
		t.Errorf("createLogDir(<junction>) = %v, want errReparsePoint", err)
	}
	if after := daclBytes(t, target); after != before {
		t.Errorf("createLogDir changed the junction target's DACL:\nbefore %s\nafter  %s", before, after)
	}
}

// TestCreateLogDir_RefusesReparsePointParent pins that a service creating its
// default log directory at runtime refuses a junction parent (and so fails
// with a trace) rather than creating the directory inside the target.
func TestCreateLogDir_RefusesReparsePointParent(t *testing.T) {
	dir, targetLogs := junctionParent(t, false)
	if err := createLogDir(dir); !errors.Is(err, errReparsePoint) {
		t.Errorf("createLogDir(<junction>\\logs) = %v, want errReparsePoint", err)
	}
	if _, err := os.Lstat(targetLogs); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a logs directory was created in the junction's target (lstat: %v)", err)
	}
}

// TestCreateLogDir_RefusesExistingDirUnderReparsePointParent pins that an
// existing logs directory reached through a junctioned parent is refused too,
// and left unchanged.
func TestCreateLogDir_RefusesExistingDirUnderReparsePointParent(t *testing.T) {
	dir, targetLogs := junctionParent(t, true)
	before := daclBytes(t, targetLogs)
	if err := createLogDir(dir); !errors.Is(err, errReparsePoint) {
		t.Errorf("createLogDir(<junction>\\logs) = %v, want errReparsePoint", err)
	}
	if after := daclBytes(t, targetLogs); after != before {
		t.Errorf("createLogDir changed an existing directory's DACL:\nbefore %s\nafter  %s", before, after)
	}
}

// TestCreateLogDir_RefusesJunctionedSqiDir pins that a per-service log
// directory two levels below %ProgramData%\sqi is not reached through a
// junction at %ProgramData%\sqi itself, whether or not the target already
// holds logs\<name>: nothing is created in the target and its DACLs do not
// change.
func TestCreateLogDir_RefusesJunctionedSqiDir(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "existing"}[existing], func(t *testing.T) {
			pd := t.TempDir()
			t.Setenv("ProgramData", pd)
			target := filepath.Join(t.TempDir(), "target")
			targetLogs := filepath.Join(target, "logs")
			if err := os.MkdirAll(filepath.Join(targetLogs, "svc"), 0o750); err != nil {
				t.Fatal(err)
			}
			if !existing {
				if err := os.Remove(filepath.Join(targetLogs, "svc")); err != nil {
					t.Fatal(err)
				}
			}
			before := daclBytes(t, targetLogs)
			fsutiltest.Junction(t, filepath.Join(pd, "sqi"), target)
			dir := filepath.Join(pd, "sqi", "logs", "svc")
			if err := createLogDir(dir); !errors.Is(err, errReparsePoint) {
				t.Errorf("createLogDir(<junction>\\logs\\svc) = %v, want errReparsePoint", err)
			}
			_, account := currentAccount(t)
			if err := EnsureLogDir(dir, account); !errors.Is(err, errReparsePoint) {
				t.Errorf("EnsureLogDir(<junction>\\logs\\svc) = %v, want errReparsePoint", err)
			}
			if after := daclBytes(t, targetLogs); after != before {
				t.Errorf("the junction target's logs DACL changed:\nbefore %s\nafter  %s", before, after)
			}
			if _, err := os.Lstat(filepath.Join(targetLogs, "svc")); existing == errors.Is(err, fs.ErrNotExist) {
				t.Errorf("logs\\svc in the junction's target: lstat %v, want it to exist only if it did before", err)
			}
		})
	}
}

// TestRuntimeLogDirsAreProtected pins that both runtime sites that create the
// default log directory go through createLogDir.
func TestRuntimeLogDirsAreProtected(t *testing.T) {
	t.Run("ResolveLogFile", func(t *testing.T) {
		t.Setenv("ProgramData", filepath.Join(t.TempDir(), "fresh-programdata"))
		path, err := ResolveLogFile(withService(context.Background(), "sqi-server"), "")
		if err != nil {
			t.Fatal(err)
		}
		if _, protected := daclOf(t, filepath.Dir(path)); !protected {
			t.Error("ResolveLogFile created an unprotected log directory")
		}
	})
	t.Run("appendTrace", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "logs", "svc.log")
		if err := appendTrace(path, errors.New("boom")); err != nil {
			t.Fatal(err)
		}
		if _, protected := daclOf(t, filepath.Dir(path)); !protected {
			t.Error("appendTrace created an unprotected log directory")
		}
	})
}
