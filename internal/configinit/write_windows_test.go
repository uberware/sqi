// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package configinit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/winsvc"
)

// These tests drive writeWindows with recorded operations, so nothing here
// needs elevation or changes an ACL. %ProgramData% is pointed at a temporary
// directory, so "under %ProgramData%" never means the real one. Only
// TestConfigInitWindows_RealWriterCreatesProtectedDir uses the real
// operations, and only when elevated.

// fakeWinOps records every operation writeWindows calls, in order.
type fakeWinOps struct {
	calls       []string
	elevatedErr error
	lookupErr   error
	prepareErr  error
	checkErr    error
}

func (f *fakeWinOps) recordf(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeWinOps) ops() winOps {
	return winOps{
		requireElevated: func() error { f.recordf("requireElevated"); return f.elevatedErr },
		lookupAccount:   func(account string) error { f.recordf("lookupAccount %s", account); return f.lookupErr },
		prepareConfigDir: func(dir, account string) error {
			f.recordf("prepareConfigDir %s %s", dir, account)
			return f.prepareErr
		},
		prepareDataDir: func(dir, account string) error {
			f.recordf("prepareDataDir %s %s", dir, account)
			return nil
		},
		checkPaths: func(refs []winsvc.PathRef, account string) error {
			for _, r := range refs {
				f.recordf("checkPaths %s %s %s", r.Key, r.Path, account)
			}
			return f.checkErr
		},
		dirExists: func(dir string) (bool, error) {
			f.recordf("dirExists %s", dir)
			return dirExists(dir)
		},
		writeFile: func(target string, content []byte, replace, adminOwned bool) error {
			f.recordf("writeFile %s replace=%t admin=%t", target, replace, adminOwned)
			return writeFile(target, content, replace, false)
		},
	}
}

// fakeProgramData points %ProgramData% at a new temporary directory and
// returns it.
func fakeProgramData(t *testing.T) string {
	t.Helper()
	pd := t.TempDir()
	t.Setenv("ProgramData", pd)
	return pd
}

// runInitWindows runs the command with the recorded operations.
func runInitWindows(t *testing.T, spec Spec, f *fakeWinOps, args ...string) (string, error) {
	t.Helper()
	return runInit(t, spec, windowsWriter(f.ops()), args...)
}

func TestConfigInitWindows_ElevationFirst(t *testing.T) {
	pd := fakeProgramData(t)
	tgt := filepath.Join(pd, "sqi", "sqi-worker.yaml")
	f := &fakeWinOps{elevatedErr: winsvc.ErrNotElevated}
	err := writeWindows(tgt, []byte("x"), "", false, filepath.Join(pd, "sqi", "worker"), f.ops())
	if !errors.Is(err, winsvc.ErrNotElevated) {
		t.Fatalf("error = %v, want ErrNotElevated", err)
	}
	if want := []string{"requireElevated"}; !slices.Equal(f.calls, want) {
		t.Errorf("calls = %q, want %q", f.calls, want)
	}
	if _, err := os.Lstat(filepath.Dir(tgt)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the configuration directory exists after a refusal: %v", err)
	}
}

func TestConfigInitWindows_ElevationFirstForMissingDirAndAccount(t *testing.T) {
	fakeProgramData(t)
	tests := []struct {
		name    string
		target  string
		account string
	}{
		{"missing directory", filepath.Join(t.TempDir(), "new", "sqi-worker.yaml"), ""},
		{"account", filepath.Join(t.TempDir(), "sqi-worker.yaml"), `.\render`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeWinOps{elevatedErr: winsvc.ErrNotElevated}
			err := writeWindows(tt.target, []byte("x"), tt.account, false, filepath.Join(filepath.Dir(tt.target), "worker"), f.ops())
			if !errors.Is(err, winsvc.ErrNotElevated) {
				t.Fatalf("error = %v, want ErrNotElevated", err)
			}
			for _, c := range f.calls {
				if c != "requireElevated" && !strings.HasPrefix(c, "dirExists ") {
					t.Errorf("calls = %q: %q ran although elevation was refused", f.calls, c)
				}
			}
			if _, err := os.Lstat(tt.target); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("target written after a refusal: %v", err)
			}
		})
	}
}

func TestConfigInitWindows_NoElevationForExistingOwnDir(t *testing.T) {
	fakeProgramData(t)
	tgt := filepath.Join(t.TempDir(), "sqi-worker.yaml")
	f := &fakeWinOps{elevatedErr: winsvc.ErrNotElevated}
	out, err := runInitWindows(t, workerSpec, f, "--config", tgt)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "dirExists ") && !strings.HasPrefix(c, "writeFile ") {
			t.Errorf("calls = %q: %q ran for an existing directory of one's own", f.calls, c)
		}
	}
	if got := readFile(t, tgt); !strings.Contains(got, "worker:") {
		t.Errorf("file not written: %q", got)
	}
	if !strings.Contains(out, "next: sqi-worker service install --start\n") {
		t.Errorf("output %q lacks the plain next step", out)
	}
}

func TestConfigInitWindows_UserNeedsConfig(t *testing.T) {
	pd := fakeProgramData(t)
	f := &fakeWinOps{}
	_, err := runInitWindows(t, workerSpec, f, "--user", "render")
	want := `--user needs its own directory: pass --config, e.g. C:\ProgramData\sqi-render\sqi-worker.yaml`
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
	if want := []string{"requireElevated"}; !slices.Equal(f.calls, want) {
		t.Errorf("calls = %q, want %q", f.calls, want)
	}
	if _, err := os.Lstat(filepath.Join(pd, "sqi")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the default directory was created: %v", err)
	}
}

func TestConfigInitWindows_UserRefusesDefaultDir(t *testing.T) {
	pd := fakeProgramData(t)
	tests := []string{
		filepath.Join(pd, "sqi", "sqi-server.yaml"),
		strings.ToUpper(filepath.Join(pd, "SQI")) + `\other.yaml`,
		filepath.Join(pd, "sqi", ".", "sqi-server.yaml"),
	}
	for _, tgt := range tests {
		t.Run(tgt, func(t *testing.T) {
			f := &fakeWinOps{}
			_, err := runInitWindows(t, serverSpec, f, "--user", "render", "--config", tgt)
			if err == nil || !strings.HasPrefix(err.Error(), "--user needs its own directory: pass --config") {
				t.Fatalf("error = %v, want the own-directory refusal", err)
			}
			if want := []string{"requireElevated"}; !slices.Equal(f.calls, want) {
				t.Errorf("calls = %q, want %q", f.calls, want)
			}
		})
	}
}

func TestConfigInitWindows_UserAcceptsSiblingOfDefaultDir(t *testing.T) {
	pd := fakeProgramData(t)
	tgt := filepath.Join(pd, "sqiX", "sqi-worker.yaml")
	f := &fakeWinOps{}
	if err := os.Mkdir(filepath.Dir(tgt), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := runInitWindows(t, workerSpec, f, "--user", "render", "--config", tgt); err != nil {
		t.Fatalf("%s refused: %v", tgt, err)
	}
}

func TestConfigInitWindows_UnresolvedAccountChangesNothing(t *testing.T) {
	fakeProgramData(t)
	dir := filepath.Join(t.TempDir(), "sqi-render")
	tgt := filepath.Join(dir, "sqi-worker.yaml")
	f := &fakeWinOps{lookupErr: errors.New("look up account .\\nobody: no mapping")}
	_, err := runInitWindows(t, workerSpec, f, "--user", "nobody", "--config", tgt)
	if err == nil || !strings.Contains(err.Error(), "no mapping") {
		t.Fatalf("error = %v, want the lookup error", err)
	}
	if want := []string{"requireElevated", `lookupAccount .\nobody`}; !slices.Equal(f.calls, want) {
		t.Errorf("calls = %q, want %q", f.calls, want)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the directory was created for an unresolved account: %v", err)
	}
}

func TestConfigInitWindows_UserPreparesBothDirs(t *testing.T) {
	fakeProgramData(t)
	dir := t.TempDir()
	tgt := filepath.Join(dir, "sqi-worker.yaml")
	f := &fakeWinOps{}
	out, err := runInitWindows(t, workerSpec, f, "--user", "render", "--config", tgt)
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "worker")
	want := []string{
		"requireElevated",
		`lookupAccount .\render`,
		`prepareConfigDir ` + dir + ` .\render`,
		`prepareDataDir ` + data + ` .\render`,
		"writeFile " + tgt + " replace=false admin=true",
	}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	if got := readFile(t, tgt); !strings.Contains(got, "data_dir: '"+data+"'") {
		t.Errorf("worker.data_dir is not %s:\n%s", data, got)
	}
	if wantNext := "next: sqi-worker service install --start --user render --config " + tgt + "\n"; !strings.Contains(out, wantNext) {
		t.Errorf("output %q lacks %q", out, wantNext)
	}
}

func TestConfigInitWindows_ServerUserDataDir(t *testing.T) {
	fakeProgramData(t)
	dir := t.TempDir()
	tgt := filepath.Join(dir, "sqi-server.yaml")
	f := &fakeWinOps{}
	if _, err := runInitWindows(t, serverSpec, f, "--user", `HOST\render`, "--config", tgt); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data")
	if !slices.Contains(f.calls, `prepareDataDir `+data+` HOST\render`) {
		t.Errorf("calls = %q, want prepareDataDir %s", f.calls, data)
	}
	got := readFile(t, tgt)
	for _, w := range []string{"'" + filepath.Join(data, "sqi.db") + "'", "'" + filepath.Join(data, "nats") + "'"} {
		if !strings.Contains(got, w) {
			t.Errorf("starter lacks %s:\n%s", w, got)
		}
	}
}

func TestConfigInitWindows_LocalSystemIsNoAccount(t *testing.T) {
	fakeProgramData(t)
	tgt := filepath.Join(t.TempDir(), "sqi-worker.yaml")
	f := &fakeWinOps{}
	out, err := runInitWindows(t, workerSpec, f, "--user", "LocalSystem", "--config", tgt)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "lookupAccount") || strings.HasPrefix(c, "prepareDataDir") {
			t.Errorf("calls = %q: LocalSystem was treated as an account", f.calls)
		}
	}
	if strings.Contains(out, "--user") {
		t.Errorf("output %q names --user for LocalSystem", out)
	}
}

func TestConfigInitWindows_ProgramDataPreparesWithoutAccount(t *testing.T) {
	pd := fakeProgramData(t)
	dir := filepath.Join(pd, "sqi")
	tgt := filepath.Join(dir, "sqi-worker.yaml")
	if err := os.Mkdir(dir, 0o700); err != nil { // the fake prepare creates nothing
		t.Fatal(err)
	}
	f := &fakeWinOps{}
	if _, err := runInitWindows(t, workerSpec, f); err != nil {
		t.Fatal(err)
	}
	want := []string{"requireElevated", "prepareConfigDir " + dir + " ", "writeFile " + tgt + " replace=false admin=true"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls = %q, want %q", f.calls, want)
	}
}

func TestConfigInitWindows_ForceChecksTheFile(t *testing.T) {
	pd := fakeProgramData(t)
	dir := filepath.Join(pd, "sqi")
	tgt := filepath.Join(dir, "sqi-worker.yaml")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tgt, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeWinOps{}
	if _, err := runInitWindows(t, workerSpec, f, "--force"); err != nil {
		t.Fatal(err)
	}
	want := []string{"requireElevated", "prepareConfigDir " + dir + " ", "checkPaths config " + tgt + " ", "writeFile " + tgt + " replace=true admin=true"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls = %q, want %q", f.calls, want)
	}
	if got := readFile(t, tgt); !strings.Contains(got, "worker:") {
		t.Errorf("file not replaced: %q", got)
	}
}

func TestConfigInitWindows_ForceRefusesUntrustedFile(t *testing.T) {
	pd := fakeProgramData(t)
	dir := filepath.Join(pd, "sqi")
	tgt := filepath.Join(dir, "sqi-worker.yaml")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tgt, []byte("planted"), 0o600); err != nil {
		t.Fatal(err)
	}
	untrusted := &winsvc.UntrustedPathsError{Failures: []winsvc.PathFailure{{
		Key: "config", Path: tgt, JudgedPath: tgt, Reason: `owner HOST\mallory is not trusted`,
	}}}
	f := &fakeWinOps{checkErr: untrusted}
	_, err := runInitWindows(t, workerSpec, f, "--force")
	if got, ok := errors.AsType[*winsvc.UntrustedPathsError](err); !ok || got != untrusted {
		t.Fatalf("error = %v, want the *UntrustedPathsError", err)
	}
	for _, w := range []string{`owner HOST\mallory is not trusted`, "Rename-Item -LiteralPath '" + tgt + "'", "'sqi-worker.yaml.suspect-'"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error lacks %q:\n%v", w, err)
		}
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "writeFile") {
			t.Errorf("calls = %q: the planted file was written", f.calls)
		}
	}
	if b := readFile(t, tgt); b != "planted" {
		t.Errorf("planted file changed to %q", b)
	}
}

func TestConfigInitWindows_RefusalPrintsMoveAside(t *testing.T) {
	pd := fakeProgramData(t)
	dir := filepath.Join(pd, "sqi")
	f := &fakeWinOps{prepareErr: &winsvc.UntrustedPathsError{Failures: []winsvc.PathFailure{{
		Key: "config directory", Path: dir, JudgedPath: dir, Reason: "BUILTIN\\Users can write (WD,AD)",
	}}}}
	_, err := runInitWindows(t, workerSpec, f)
	if err == nil {
		t.Fatal("a failing directory was accepted")
	}
	if _, ok := errors.AsType[*winsvc.UntrustedPathsError](err); !ok {
		t.Errorf("error %v does not wrap the *UntrustedPathsError", err)
	}
	msg := err.Error()
	for _, w := range []string{
		"BUILTIN\\Users can write (WD,AD)",
		"Rename-Item -LiteralPath '" + dir + "' -NewName ('sqi.suspect-' + (Get-Date -Format yyyyMMddHHmmss))",
		"run config init again",
	} {
		if !strings.Contains(msg, w) {
			t.Errorf("message lacks %q:\n%s", w, msg)
		}
	}
	if slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "writeFile") }) {
		t.Errorf("calls = %q: written after a refusal", f.calls)
	}
}

func TestConfigInitWindows_AncestorRefusalDoesNotSuggestRenamingIt(t *testing.T) {
	fakeProgramData(t)
	parent := t.TempDir()
	dir := filepath.Join(parent, "sqi-render")
	f := &fakeWinOps{prepareErr: &winsvc.UntrustedPathsError{Failures: []winsvc.PathFailure{{
		Key: "config directory", Path: dir, JudgedPath: parent, Reason: `owner HOST\bob is not trusted`,
	}}}}
	_, err := runInitWindows(t, workerSpec, f, "--user", "render", "--config", filepath.Join(dir, "sqi-worker.yaml"))
	if err == nil {
		t.Fatal("a failing ancestor was accepted")
	}
	if msg := err.Error(); strings.Contains(msg, "Rename-Item") || !strings.Contains(msg, "--config") {
		t.Errorf("message should point at another --config, not renaming %s:\n%s", parent, msg)
	}
}

func TestConfigInitWindows_ProgramDataItselfIsNeverRenamed(t *testing.T) {
	pd := fakeProgramData(t)
	f := &fakeWinOps{prepareErr: &winsvc.UntrustedPathsError{Failures: []winsvc.PathFailure{{
		Key: "config directory", Path: pd, JudgedPath: pd, Reason: "BUILTIN\\Users can write (WD,AD)",
	}}}}
	_, err := runInitWindows(t, workerSpec, f, "--config", filepath.Join(pd, "sqi-worker.yaml"))
	if err == nil || strings.Contains(err.Error(), "Rename-Item") {
		t.Errorf("error = %v, want a refusal that does not suggest renaming %%ProgramData%%", err)
	}
}

func TestUnderDir(t *testing.T) {
	tests := []struct {
		dir, root string
		want      bool
	}{
		{`C:\ProgramData`, `C:\ProgramData`, true},
		{`C:\ProgramData\sqi`, `C:\ProgramData`, true},
		{`c:\programdata\SQI\x`, `C:\ProgramData`, true},
		{`C:\ProgramData\sqi\..\sqi`, `C:\ProgramData\`, true},
		{`C:\ProgramDataX`, `C:\ProgramData`, false},
		{`C:\ProgramDataX\sqi`, `C:\ProgramData`, false},
		{`C:\Program`, `C:\ProgramData`, false},
		{`D:\ProgramData`, `C:\ProgramData`, false},
		{`C:\x`, `C:\`, true},
	}
	for _, tt := range tests {
		if got := underDir(tt.dir, tt.root); got != tt.want {
			t.Errorf("underDir(%q, %q) = %v, want %v", tt.dir, tt.root, got, tt.want)
		}
	}
}

func TestWriteFile_NoReplaceRefusesAnExistingFile(t *testing.T) {
	tgt := filepath.Join(t.TempDir(), "sqi-worker.yaml")
	if err := os.WriteFile(tgt, []byte("planted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(tgt, []byte("new"), false, false); err == nil {
		t.Error("writeFile without replace overwrote an existing file")
	}
	if got := readFile(t, tgt); got != "planted" {
		t.Errorf("file changed to %q", got)
	}
	entries, err := os.ReadDir(filepath.Dir(tgt))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want the temporary file removed", len(entries))
	}
	if err := writeFile(tgt, []byte("new"), true, false); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, tgt); got != "new" {
		t.Errorf("file = %q after replace", got)
	}
}

// TestConfigInitWindows_RealWriterCreatesProtectedDir runs the real writer
// against a new directory under the real %ProgramData%.
func TestConfigInitWindows_RealWriterCreatesProtectedDir(t *testing.T) {
	if err := winsvc.RequireElevated(); err != nil {
		t.Skip("needs an elevated shell")
	}
	dir := filepath.Join(winsvc.ProgramDataDir(), fmt.Sprintf("sqi-configinit-test-%d", os.Getpid()))
	if _, err := os.Lstat(dir); err == nil {
		t.Fatalf("%s already exists", dir)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove %s: %v", dir, err)
		}
	})
	tgt := filepath.Join(dir, "sqi-worker.yaml")
	if _, err := runInit(t, workerSpec, windowsWriter(realWinOps()), "--config", tgt); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, tgt); !strings.Contains(got, "worker:") {
		t.Errorf("file not written: %q", got)
	}
	refs := []winsvc.PathRef{{Key: "config directory", Path: dir}, {Key: "config", Path: tgt}}
	if err := winsvc.CheckPaths(refs, ""); err != nil {
		t.Errorf("the created directory fails the check:\n%v", err)
	}
	// Replacing it with --force goes through the file check and still passes.
	if _, err := runInit(t, workerSpec, windowsWriter(realWinOps()), "--config", tgt, "--force"); err != nil {
		t.Fatal(err)
	}
	if err := winsvc.CheckPaths(refs, ""); err != nil {
		t.Errorf("the replaced file fails the check:\n%v", err)
	}
}
