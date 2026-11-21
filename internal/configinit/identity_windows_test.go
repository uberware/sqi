// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package configinit

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/fsutil/fsutiltest"
	"github.com/uberware/sqi/internal/winsvc"
)

// These tests point %ProgramData% at a temporary stand-in and reach its sqi
// directory through aliases a string comparison cannot see.

func TestCanonicalPath_RefusesStreamsAndUNC(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sqi"), 0o700); err != nil {
		t.Fatal(err)
	}
	vol := filepath.VolumeName(root) // "C:"
	rest := root[len(vol)+1:]        // without the leading backslash
	drive := strings.TrimSuffix(vol, ":") + "$"
	tests := map[string]string{
		"index allocation":      root + `\sqi::$INDEX_ALLOCATION\sqi-worker.yaml`,
		"named index":           root + `\sqi:$I30:$INDEX_ALLOCATION\sqi-worker.yaml`,
		"stream on a parent":    root + `\sqi:x\deeper\sqi-worker.yaml`,
		"stream on the file":    root + `\sqi\sqi-worker.yaml:x`,
		"localhost admin share": `\\localhost\` + drive + `\` + rest + `\sqi\sqi-worker.yaml`,
		"loopback admin share":  `\\127.0.0.1\` + drive + `\` + rest + `\sqi\sqi-worker.yaml`,
		"forward-slash UNC":     `//localhost/` + drive + `/` + filepath.ToSlash(rest) + `/sqi/sqi-worker.yaml`,
	}
	for name, p := range tests {
		t.Run(name, func(t *testing.T) {
			if got, err := canonicalPath(p); err == nil {
				t.Errorf("canonicalPath(%q) = %q, want an error", p, got)
			}
		})
	}
}

func TestConfigInitWindows_RefusesStreamAndUNCTargets(t *testing.T) {
	pd := fakeProgramData(t)
	dir := filepath.Join(pd, "sqi")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	vol := filepath.VolumeName(pd)
	unc := `\\localhost\` + strings.TrimSuffix(vol, ":") + `$` + pd[len(vol):]
	for _, tgt := range []string{
		dir + `::$INDEX_ALLOCATION\sqi-worker.yaml`,
		dir + `:$I30:$INDEX_ALLOCATION\sqi-worker.yaml`,
		unc + `\sqi\sqi-worker.yaml`,
		strings.Replace(unc, "localhost", "127.0.0.1", 1) + `\sqi\sqi-worker.yaml`,
	} {
		for _, args := range [][]string{{"--config", tgt}, {"--user", "render", "--config", tgt}} {
			t.Run(strings.Join(args, " "), func(t *testing.T) {
				f := &fakeWinOps{}
				if _, err := runInitWindows(t, workerSpec, f, args...); err == nil {
					t.Fatal("accepted")
				}
				if len(f.calls) != 0 {
					t.Errorf("calls = %q, want none", f.calls)
				}
			})
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Errorf("the default directory holds %d entries (%v), want none", len(entries), err)
	}
}

// aliases makes, outside pd, a junction to pd\sqi and a junction to pd,
// and returns them.
func aliases(t *testing.T, pd string) (toDefault, toProgramData string) {
	t.Helper()
	elsewhere := t.TempDir()
	toDefault = filepath.Join(elsewhere, "alias-of-sqi")
	toProgramData = filepath.Join(elsewhere, "alias-of-programdata")
	fsutiltest.Junction(t, toDefault, filepath.Join(pd, "sqi"))
	fsutiltest.Junction(t, toProgramData, pd)
	return toDefault, toProgramData
}

func TestConfigInitWindows_UserRefusesDefaultDirByIdentity(t *testing.T) {
	pd := fakeProgramData(t)
	if err := os.Mkdir(filepath.Join(pd, "sqi"), 0o700); err != nil {
		t.Fatal(err)
	}
	toDefault, toProgramData := aliases(t, pd)
	tests := map[string]string{
		"junction to the default directory":   toDefault,
		"through a junction to %ProgramData%": filepath.Join(toProgramData, "sqi"),
	}
	if short := shortPath(t, pd); short != "" {
		tests["8.3 spelling"] = filepath.Join(short, "sqi")
	}
	for name, dir := range tests {
		t.Run(name, func(t *testing.T) {
			f := &fakeWinOps{}
			_, err := runInitWindows(t, workerSpec, f, "--user", "render", "--config", filepath.Join(dir, "sqi-worker.yaml"))
			if err == nil || !strings.HasPrefix(err.Error(), "--user needs its own directory") {
				t.Fatalf("%s: error = %v, want the own-directory refusal", dir, err)
			}
			if want := []string{"requireElevated"}; !slices.Equal(f.calls, want) {
				t.Errorf("calls = %q, want %q", f.calls, want)
			}
		})
	}
}

func TestConfigInitWindows_DefaultDirNotYetCreatedIsRecognized(t *testing.T) {
	pd := fakeProgramData(t)
	_, toProgramData := aliases(t, pd)
	// pd\sqi does not exist; the junction resolves its parent to pd.
	if err := os.Remove(filepath.Join(filepath.Dir(toProgramData), "alias-of-sqi")); err != nil {
		t.Fatal(err)
	}
	f := &fakeWinOps{}
	_, err := runInitWindows(t, workerSpec, f, "--user", "render", "--config", filepath.Join(toProgramData, "SQI", "sqi-worker.yaml"))
	if err == nil || !strings.HasPrefix(err.Error(), "--user needs its own directory") {
		t.Fatalf("error = %v, want the own-directory refusal", err)
	}
}

func TestConfigInitWindows_UnderProgramDataByIdentity(t *testing.T) {
	pd := fakeProgramData(t)
	if err := os.Mkdir(filepath.Join(pd, "sqi"), 0o700); err != nil {
		t.Fatal(err)
	}
	toDefault, toProgramData := aliases(t, pd)
	if err := os.Mkdir(filepath.Join(pd, "other"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Each directory exists, so only "under %ProgramData%" asks for elevation.
	for name, dir := range map[string]string{
		"junction to pd\\sqi":            toDefault,
		"junction to pd":                 toProgramData,
		"directory through the junction": filepath.Join(toProgramData, "other"),
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeWinOps{elevatedErr: winsvc.ErrNotElevated}
			_, err := runInitWindows(t, workerSpec, f, "--config", filepath.Join(dir, "sqi-worker.yaml"))
			if !errors.Is(err, winsvc.ErrNotElevated) {
				t.Fatalf("%s: error = %v, want ErrNotElevated", dir, err)
			}
			if want := []string{"requireElevated"}; !slices.Equal(f.calls, want) {
				t.Errorf("calls = %q, want %q", f.calls, want)
			}
		})
	}
}

func TestIdentity_SameDirectoryAnySpelling(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "a-long-directory-name")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "j")
	fsutiltest.Junction(t, link, dir)
	want, ok, err := identities(dir)
	if err != nil || !ok {
		t.Fatalf("identities(%s) = %v, %v", dir, ok, err)
	}
	spellings := []string{strings.ToUpper(dir), link}
	if short := shortPath(t, dir); short != "" {
		spellings = append(spellings, short)
	}
	for _, s := range spellings {
		got, ok, err := identities(s)
		if err != nil || !ok || !sameFile(got, want) {
			t.Errorf("identities(%s) = %v, %v, %v; want it to match %s", s, got, ok, err, dir)
		}
	}
	other, _, err := identities(root)
	if err != nil || sameFile(other, want) {
		t.Errorf("%s matched %s (%v)", root, dir, err)
	}
	if _, ok, err := identities(filepath.Join(root, "missing")); ok || err != nil {
		t.Errorf("a missing path: ok = %v, err = %v", ok, err)
	}
}
