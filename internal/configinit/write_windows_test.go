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

	"golang.org/x/sys/windows"

	"github.com/uberware/sqi/internal/fsutil/fsutiltest"
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
		makeDir: func(dir string) error {
			f.recordf("makeDir %s", dir)
			return makeDir(dir)
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
	err := writeWindows(writeRequest{Target: tgt, Content: []byte("x"), DataDir: filepath.Join(pd, "sqi", "worker")}, f.ops())
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

// TestConfigInitWindows_ElevationBeforeExistingFile pins that an unelevated
// run on a file that is already there, in a directory that needs elevation,
// fails with the elevation error: the existence check comes after it, so it
// never reports what an administrators-only directory denies.
func TestConfigInitWindows_ElevationBeforeExistingFile(t *testing.T) {
	pd := fakeProgramData(t)
	dir := filepath.Join(pd, "sqi")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sqi-worker.yaml"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeWinOps{elevatedErr: winsvc.ErrNotElevated}
	if _, err := runInitWindows(t, workerSpec, f); !errors.Is(err, winsvc.ErrNotElevated) {
		t.Fatalf("error = %v, want ErrNotElevated", err)
	}
}

// TestConfigInitWindows_RefusesExistingWithoutForce pins the Windows writer's
// refusal of a file already there, with nothing prepared or written.
func TestConfigInitWindows_RefusesExistingWithoutForce(t *testing.T) {
	fakeProgramData(t)
	tgt := filepath.Join(t.TempDir(), "sqi-worker.yaml")
	if err := os.WriteFile(tgt, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeWinOps{}
	_, err := runInitWindows(t, workerSpec, f, "--config", tgt)
	if want := tgt + " exists; pass --force to replace it"; err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "dirExists ") {
			t.Errorf("calls = %q: %q ran for an existing file", f.calls, c)
		}
	}
	if got := readFile(t, tgt); got != "mine" {
		t.Errorf("existing file changed to %q", got)
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
			req := writeRequest{
				Target: tt.target, Content: []byte("x"), Account: tt.account,
				DataDir: filepath.Join(filepath.Dir(tt.target), "worker"),
			}
			err := writeWindows(req, f.ops())
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
		if !strings.HasPrefix(c, "dirExists ") && !strings.HasPrefix(c, "makeDir ") && !strings.HasPrefix(c, "writeFile ") {
			t.Errorf("calls = %q: %q ran for an existing directory of one's own", f.calls, c)
		}
	}
	if got := readFile(t, tgt); !strings.Contains(got, "worker:") {
		t.Errorf("file not written: %q", got)
	}
	if !strings.Contains(out, " service install --config '"+tgt+"' --start\n") {
		t.Errorf("output %q lacks the next step", out)
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

// TestConfigInitWindows_UserRefusesInsideDefaultDir pins that --user refuses
// a configuration directory inside %ProgramData%\sqi as it refuses that
// directory itself, by spelling or through a junction, before it changes
// anything: service install --user refuses a configuration there, which only
// SYSTEM and Administrators can read.
func TestConfigInitWindows_UserRefusesInsideDefaultDir(t *testing.T) {
	pd := fakeProgramData(t)
	sqiDir := filepath.Join(pd, "sqi")
	tests := map[string]func(t *testing.T) string{
		"by spelling": func(*testing.T) string { return filepath.Join(sqiDir, "render", "sqi-worker.yaml") },
		"through a junction": func(t *testing.T) string {
			if err := os.Mkdir(sqiDir, 0o700); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(t.TempDir(), "elsewhere")
			fsutiltest.Junction(t, link, sqiDir)
			return filepath.Join(link, "render", "sqi-worker.yaml")
		},
	}
	for name, target := range tests {
		t.Run(name, func(t *testing.T) {
			tgt := target(t)
			f := &fakeWinOps{}
			_, err := runInitWindows(t, workerSpec, f, "--user", "render", "--config", tgt)
			want := `--user needs its own directory: pass --config, e.g. C:\ProgramData\sqi-render\sqi-worker.yaml`
			if err == nil || err.Error() != want {
				t.Fatalf("error = %v, want %q", err, want)
			}
			if want := []string{"requireElevated"}; !slices.Equal(f.calls, want) {
				t.Errorf("calls = %q, want %q", f.calls, want)
			}
			if _, err := os.Lstat(filepath.Join(sqiDir, "render")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the configuration directory was created: %v", err)
			}
		})
	}
}

// shortPath is p's 8.3 spelling, or "" when the volume has none for it.
func shortPath(t *testing.T, p string) string {
	t.Helper()
	src, err := windows.UTF16PtrFromString(p)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetShortPathName(src, &buf[0], uint32(len(buf)))
	if err != nil {
		t.Fatalf("GetShortPathName(%s): %v", p, err)
	}
	if s := windows.UTF16ToString(buf[:n]); !strings.EqualFold(s, p) {
		return s
	}
	return ""
}

// spellingsOf is other ways of writing the existing directory dir that
// Win32 opens as dir. The 8.3 row is left out when the volume has no short
// names.
func spellingsOf(t *testing.T, dir string) map[string]string {
	t.Helper()
	parent, leaf := filepath.Dir(dir), filepath.Base(dir)
	s := map[string]string{
		"exact":           dir,
		"case":            strings.ToUpper(dir),
		"dot component":   filepath.Join(dir, ".") + `\.`,
		"trailing dot":    parent + `\` + leaf + `.`,
		"trailing space":  parent + `\` + leaf + ` `,
		"dots and spaces": parent + `\` + leaf + `. .`,
	}
	if short := shortPath(t, parent); short != "" {
		s["8.3 parent"] = short + `\` + leaf
	} else {
		t.Log("no 8.3 names on this volume; skipping the 8.3 row")
	}
	return s
}

func TestConfigInitWindows_UserRefusesDefaultDir(t *testing.T) {
	pd := fakeProgramData(t)
	dir := filepath.Join(pd, "sqi")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, spelling := range spellingsOf(t, dir) {
		t.Run(name, func(t *testing.T) {
			f := &fakeWinOps{}
			_, err := runInitWindows(t, serverSpec, f, "--user", "render", "--config", spelling+`\sqi-server.yaml`)
			if err == nil || !strings.HasPrefix(err.Error(), "--user needs its own directory: pass --config") {
				t.Fatalf("%q: error = %v, want the own-directory refusal", spelling, err)
			}
			if want := []string{"requireElevated"}; !slices.Equal(f.calls, want) {
				t.Errorf("calls = %q, want %q", f.calls, want)
			}
		})
	}
}

func TestConfigInitWindows_ProgramDataSpellingsNeedElevation(t *testing.T) {
	pd := fakeProgramData(t)
	dir := filepath.Join(pd, "sqi")
	if err := os.Mkdir(dir, 0o700); err != nil { // existing, so only "under %ProgramData%" asks for elevation
		t.Fatal(err)
	}
	for name, spelling := range spellingsOf(t, dir) {
		t.Run(name, func(t *testing.T) {
			f := &fakeWinOps{elevatedErr: winsvc.ErrNotElevated}
			_, err := runInitWindows(t, workerSpec, f, "--config", spelling+`\sqi-worker.yaml`)
			if !errors.Is(err, winsvc.ErrNotElevated) {
				t.Fatalf("%q: error = %v, want ErrNotElevated", spelling, err)
			}
			if want := []string{"requireElevated"}; !slices.Equal(f.calls, want) {
				t.Errorf("calls = %q, want %q", f.calls, want)
			}
		})
	}
}

func TestConfigInitWindows_ShortProgramDataEnvironment(t *testing.T) {
	pd := fakeProgramData(t)
	short := shortPath(t, pd)
	if short == "" {
		t.Skip("no 8.3 names on this volume")
	}
	t.Setenv("ProgramData", short)
	dir := filepath.Join(pd, "sqi")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := &fakeWinOps{elevatedErr: winsvc.ErrNotElevated}
	_, err := runInitWindows(t, workerSpec, f, "--user", "render", "--config", filepath.Join(dir, "sqi-worker.yaml"))
	if !errors.Is(err, winsvc.ErrNotElevated) {
		t.Fatalf("error = %v, want ErrNotElevated", err)
	}
	f = &fakeWinOps{}
	_, err = runInitWindows(t, workerSpec, f, "--user", "render", "--config", filepath.Join(dir, "sqi-worker.yaml"))
	if err == nil || !strings.HasPrefix(err.Error(), "--user needs its own directory") {
		t.Errorf("error = %v, want the own-directory refusal", err)
	}
}

func TestConfigInitWindows_RefusesDevicePaths(t *testing.T) {
	pd := fakeProgramData(t)
	long := filepath.Join(pd, "sqi", "sqi-worker.yaml")
	for _, tgt := range []string{`\\?\` + long, `\\.\` + long, `//?/` + filepath.ToSlash(long), `\??\` + long} {
		for _, args := range [][]string{{"--config", tgt}, {"--user", "render", "--config", tgt}} {
			t.Run(strings.Join(args, " "), func(t *testing.T) {
				f := &fakeWinOps{}
				_, err := runInitWindows(t, workerSpec, f, args...)
				if err == nil || !strings.Contains(err.Error(), `\\?\ or \\.\`) {
					t.Fatalf("error = %v, want the device-path refusal", err)
				}
				if len(f.calls) != 0 {
					t.Errorf("calls = %q, want none", f.calls)
				}
			})
		}
	}
}

func TestConfigInitWindows_JudgesAndWritesTheCanonicalPath(t *testing.T) {
	fakeProgramData(t)
	dir := filepath.Join(t.TempDir(), "sqi-render")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := &fakeWinOps{}
	spelled := dir + `. \sqi-worker.yaml`
	out, err := runInitWindows(t, workerSpec, f, "--user", "render", "--config", spelled)
	if err != nil {
		t.Fatal(err)
	}
	tgt := filepath.Join(dir, "sqi-worker.yaml")
	data := filepath.Join(dir, "worker")
	want := []string{
		"requireElevated",
		`lookupAccount .\render`,
		`prepareConfigDir ` + dir + ` .\render`,
		`prepareDataDir ` + filepath.Join(dir, "sessions") + ` .\render`,
		`prepareDataDir ` + data + ` .\render`,
		"writeFile " + tgt + " replace=false admin=true",
	}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	if got := readFile(t, tgt); !strings.Contains(got, "data_dir: '"+data+"'") {
		t.Errorf("worker.data_dir is not %s:\n%s", data, got)
	}
	if !strings.HasPrefix(out, "wrote "+tgt+"\n") || !strings.Contains(out, "--config '"+tgt+"'\n") {
		t.Errorf("output %q does not name %s", out, tgt)
	}
}

func TestCanonicalPath(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "a"), 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		in, want string
	}{
		{root + `\a\x.yaml`, root + `\a\x.yaml`},
		{root + `\a.\x.yaml`, root + `\a\x.yaml`},
		{root + `\a \x.yaml`, root + `\a\x.yaml`},
		{root + `\a\x.yaml. `, root + `\a\x.yaml`},
		{root + `\a\missing. \deeper \x.yaml`, root + `\a\missing\deeper\x.yaml`},
		{root + `\a\..\a\x.yaml`, root + `\a\x.yaml`},
		{root + `/a/x.yaml`, root + `\a\x.yaml`},
	}
	if short := shortPath(t, root); short != "" {
		tests = append(tests, struct{ in, want string }{short + `\a\x.yaml`, root + `\a\x.yaml`})
	}
	for _, tt := range tests {
		got, err := canonicalPath(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("canonicalPath(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
	for _, bad := range []string{root + `\a\...\x.yaml`, `\\?\` + root, `\\.\C:`} {
		if got, err := canonicalPath(bad); err == nil {
			t.Errorf("canonicalPath(%q) = %q, want an error", bad, got)
		}
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
	sessions := filepath.Join(dir, "sessions")
	want := []string{
		"requireElevated",
		`lookupAccount .\render`,
		`prepareConfigDir ` + dir + ` .\render`,
		`prepareDataDir ` + sessions + ` .\render`,
		`prepareDataDir ` + data + ` .\render`,
		"writeFile " + tgt + " replace=false admin=true",
	}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	got := readFile(t, tgt)
	if !strings.Contains(got, "data_dir: '"+data+"'") {
		t.Errorf("worker.data_dir is not %s:\n%s", data, got)
	}
	// The default session root is under the administrators-only
	// %ProgramData%\sqi, which the account cannot write.
	if !strings.Contains(got, "session_dir: '"+sessions+"'") {
		t.Errorf("worker.session_dir is not %s:\n%s", sessions, got)
	}
	if wantNext := " service install --user 'render' --config '" + tgt + "'\n"; !strings.Contains(out, wantNext) {
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
	if slices.ContainsFunc(f.calls, func(c string) bool { return strings.Contains(c, "sessions") }) {
		t.Errorf("calls = %q: a server has no session directory", f.calls)
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
	want := []string{
		"requireElevated", "prepareConfigDir " + dir + " ",
		"prepareDataDir " + filepath.Join(dir, "worker") + " ", "writeFile " + tgt + " replace=false admin=true",
	}
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
	want := []string{
		"requireElevated", "prepareConfigDir " + dir + " ", "checkPaths config " + tgt + " ",
		"prepareDataDir " + filepath.Join(dir, "worker") + " ", "writeFile " + tgt + " replace=true admin=true",
	}
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
	data := filepath.Join(dir, "worker")
	if st, err := os.Lstat(data); err != nil || !st.IsDir() {
		t.Fatalf("data directory %s: %v", data, err)
	}
	refs := []winsvc.PathRef{{Key: "config directory", Path: dir}, {Key: "config", Path: tgt}, {Key: "worker.data_dir", Path: data}}
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

func TestConfigInitWindows_CreatesTheDataDirectory(t *testing.T) {
	tests := []struct {
		name    string
		spec    Spec
		leaf    string
		account []string // --user args
		// protected: under the fake %ProgramData% (prepareDataDir);
		// otherwise an existing directory of one's own (makeDir).
		protected bool
	}{
		{"server, protected", serverSpec, "data", nil, true},
		{"worker, protected", workerSpec, "worker", nil, true},
		{"server, --user", serverSpec, "data", []string{"--user", "render"}, true},
		{"worker, --user", workerSpec, "worker", []string{"--user", "render"}, true},
		{"server, own directory", serverSpec, "data", nil, false},
		{"worker, own directory", workerSpec, "worker", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pd := fakeProgramData(t)
			dir := filepath.Join(t.TempDir(), "own")
			if tt.protected && tt.account == nil {
				dir = filepath.Join(pd, "sqi-test")
			}
			if err := os.Mkdir(dir, 0o700); err != nil { // the fake prepareConfigDir creates nothing
				t.Fatal(err)
			}
			tgt := filepath.Join(dir, string(tt.spec.Binary)+".yaml")
			data := filepath.Join(dir, tt.leaf)
			f := &fakeWinOps{}
			if _, err := runInitWindows(t, tt.spec, f, append([]string{"--config", tgt}, tt.account...)...); err != nil {
				t.Fatal(err)
			}
			account := ""
			if tt.account != nil {
				account = `.\render`
			}
			step := "makeDir " + data
			if tt.protected {
				step = "prepareDataDir " + data + " " + account
			}
			i := slices.Index(f.calls, step)
			if i < 0 || i+1 >= len(f.calls) || !strings.HasPrefix(f.calls[i+1], "writeFile "+tgt+" ") {
				t.Fatalf("calls = %q, want %q directly before the write", f.calls, step)
			}
			if tt.protected && !slices.ContainsFunc(f.calls[:i], func(c string) bool { return strings.HasPrefix(c, "prepareConfigDir "+dir+" ") }) {
				t.Errorf("calls = %q, want the configuration directory prepared before the data directory", f.calls)
			}
			if !tt.protected {
				if st, err := os.Stat(data); err != nil || !st.IsDir() {
					t.Errorf("data directory %s was not created: %v", data, err)
				}
			}
		})
	}
}

func TestMakeDir_AcceptsAnExistingDirectoryOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	for range 2 {
		if err := makeDir(dir); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := makeDir(file); err == nil {
		t.Error("makeDir accepted a file")
	}
}
