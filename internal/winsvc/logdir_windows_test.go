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

// TestCreateLogDir_ExistingDirIsUntouched pins spec §2: an existing log
// directory's ACL is left alone.
func TestCreateLogDir_ExistingDirIsUntouched(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	before := sddl(t, dir)
	if err := createLogDir(dir); err != nil {
		t.Fatal(err)
	}
	if after := sddl(t, dir); after != before {
		t.Errorf("createLogDir changed an existing directory's DACL:\nbefore %s\nafter  %s", before, after)
	}
}

// TestCreateLogDir_ExistingJunctionIsLeftAlone pins that the runtime path's
// "already exists" branch never reaches an ACL write, even through a junction.
func TestCreateLogDir_ExistingJunctionIsLeftAlone(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "logs")
	mklinkJunction(t, link, target)
	before := daclBytes(t, target)
	if err := createLogDir(link); err != nil {
		t.Fatal(err)
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

// TestCreateLogDir_ExistingDirUnderReparsePointParentIsLeftAlone pins that
// the "already exists → change nothing" branch stays as it was: with no ACL
// change there is nothing to refuse.
func TestCreateLogDir_ExistingDirUnderReparsePointParentIsLeftAlone(t *testing.T) {
	dir, targetLogs := junctionParent(t, true)
	before := daclBytes(t, targetLogs)
	if err := createLogDir(dir); err != nil {
		t.Fatalf("createLogDir = %v, want nil for an existing directory", err)
	}
	if after := daclBytes(t, targetLogs); after != before {
		t.Errorf("createLogDir changed an existing directory's DACL:\nbefore %s\nafter  %s", before, after)
	}
}

// TestRuntimeLogDirsAreProtected pins that both runtime sites that create the
// default log directory go through createLogDir.
func TestRuntimeLogDirsAreProtected(t *testing.T) {
	t.Run("ResolveLogFile", func(t *testing.T) {
		t.Setenv("ProgramData", filepath.Join(t.TempDir(), "fresh-programdata"))
		path, err := ResolveLogFile(withService(context.Background(), "sqi-server", &stopReason{}), "")
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

// sddl renders dir's DACL for an equality check (both sides use the same
// alias rendering, so SDDL is safe here).
func sddl(t *testing.T, dir string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return sd.String()
}
