// SPDX-License-Identifier: AGPL-3.0-or-later

package envutil_test

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/worker/envutil"
)

// ── Build ─────────────────────────────────────────────────────────────────────

// envSliceToMap converts a []string of "KEY=VALUE" pairs into a map for easy
// key lookup in tests.
func envSliceToMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func TestBuild_EmptyOverridesReturnsOsEnviron(t *testing.T) {
	got := envutil.Build(nil)

	// Result must be equivalent to os.Environ(): same length, same entries.
	wantMap := envSliceToMap(os.Environ())
	gotMap := envSliceToMap(got)

	if len(gotMap) != len(wantMap) {
		t.Errorf("Build(nil) len = %d; want %d (os.Environ length)", len(gotMap), len(wantMap))
	}
	for k, wantV := range wantMap {
		if gotV, ok := gotMap[k]; !ok || gotV != wantV {
			t.Errorf("Build(nil)[%q] = (%q, %v); want (%q, true)", k, gotV, ok, wantV)
		}
	}
}

func TestBuild_OverrideReplacesExistingKey(t *testing.T) {
	// Pick a key we know exists in os.Environ() (PATH is universal).
	const key = "PATH"
	if _, exists := os.LookupEnv(key); !exists {
		t.Skipf("os.Environ() does not contain %q — cannot test override", key)
	}

	const overrideVal = "/custom/path"
	got := envutil.Build(map[string]string{key: overrideVal})
	m := envSliceToMap(got)

	if m[key] != overrideVal {
		t.Errorf("Build(%q=%q)[%q] = %q; want %q", key, overrideVal, key, m[key], overrideVal)
	}
}

func TestBuild_OverrideAddsNewKey(t *testing.T) {
	const newKey = "SQI_ENVUTIL_TEST_NEW_KEY_XYZ"
	const newVal = "sentinel"

	// Ensure the key is not already set (guard against test pollution).
	if _, exists := os.LookupEnv(newKey); exists {
		t.Skipf("env key %q already set — cannot test additive override", newKey)
	}

	got := envutil.Build(map[string]string{newKey: newVal})
	m := envSliceToMap(got)

	if m[newKey] != newVal {
		t.Errorf("Build(new key)[%q] = %q; want %q", newKey, m[newKey], newVal)
	}
}

func TestBuild_OutputHasKeyValueFormat(t *testing.T) {
	got := envutil.Build(map[string]string{"TEST_KV": "hello"})

	// Every entry must contain '='.
	for _, entry := range got {
		if !strings.Contains(entry, "=") {
			t.Errorf("entry %q does not contain '='; expected KEY=VALUE format", entry)
		}
	}
}

// ── BuildWithUnset ───────────────────────────────────────────────────────────

func TestBuildWithUnset_RemovesInheritedAndOverrideKeys(t *testing.T) {
	t.Setenv("SQI_BWU_INHERITED", "from-os")
	t.Setenv("SQI_BWU_KEEP", "keep-os")

	overrides := map[string]string{
		"SQI_BWU_OVERRIDE": "override-val",
		"SQI_BWU_DROPME":   "should-be-removed",
	}
	unset := map[string]bool{
		"SQI_BWU_INHERITED": true, // remove an inherited os.Environ var
		"SQI_BWU_DROPME":    true, // remove an override-supplied var
	}

	env := envutil.BuildWithUnset(overrides, unset)

	has := func(key string) (string, bool) {
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			if k == key {
				return v, true
			}
		}
		return "", false
	}

	if _, ok := has("SQI_BWU_INHERITED"); ok {
		t.Error("expected inherited SQI_BWU_INHERITED to be unset")
	}
	if _, ok := has("SQI_BWU_DROPME"); ok {
		t.Error("expected override SQI_BWU_DROPME to be unset")
	}
	if v, ok := has("SQI_BWU_OVERRIDE"); !ok || v != "override-val" {
		t.Errorf("SQI_BWU_OVERRIDE = (%q, %v); want (override-val, true)", v, ok)
	}
	if v, ok := has("SQI_BWU_KEEP"); !ok || v != "keep-os" {
		t.Errorf("SQI_BWU_KEEP = (%q, %v); want (keep-os, true)", v, ok)
	}
}

func TestBuildWithUnset_NilUnset_MatchesBuild(t *testing.T) {
	overrides := map[string]string{"SQI_BWU_X": "1"}
	got := envutil.BuildWithUnset(overrides, nil)

	found := false
	for _, kv := range got {
		if kv == "SQI_BWU_X=1" {
			found = true
		}
	}
	if !found {
		t.Error("expected SQI_BWU_X=1 in built environment")
	}
}

// ── Policy / BaseEnv / BuildFromBase ────────────────────────────────────────

func TestBaseEnvDisabledInheritsEverything(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "secret")

	base := envutil.BaseEnv(envutil.Policy{Enabled: false})

	if base["AWS_ACCESS_KEY_ID"] != "secret" {
		t.Error("with policy disabled the daemon environment must pass through unchanged")
	}
}

func TestBaseEnvFiltersDaemonSecrets(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "secret")
	t.Setenv("PATH", "/usr/bin")

	base := envutil.BaseEnv(envutil.Policy{Enabled: true})

	if _, ok := base["AWS_ACCESS_KEY_ID"]; ok {
		t.Error("AWS_ACCESS_KEY_ID must not survive the allowlist")
	}
	if base["PATH"] != "/usr/bin" {
		t.Error("PATH is in the minimal base and must survive")
	}
}

func TestBaseEnvHonoursPassthroughGlobs(t *testing.T) {
	t.Setenv("foundry_LICENSE", "4101@licsrv")
	t.Setenv("ARNOLD_LICENSE_FILE", "/etc/arnold.lic")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	base := envutil.BaseEnv(envutil.Policy{
		Enabled:   true,
		Allowlist: []string{"foundry_LICENSE", "*_LICENSE_FILE"},
	})

	if base["foundry_LICENSE"] != "4101@licsrv" {
		t.Error("exact-name passthrough must survive")
	}
	if base["ARNOLD_LICENSE_FILE"] != "/etc/arnold.lic" {
		t.Error("glob passthrough must survive")
	}
	if _, ok := base["AWS_SECRET_ACCESS_KEY"]; ok {
		t.Error("non-allowlisted secret must not survive")
	}
}

// POSIX environment names are case-sensitive: a lowercase "home" is a different
// variable from HOME and must not ride through the minimal base.
func TestBaseEnvMinimalBaseIsCaseSensitiveOnPOSIX(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows environment names are case-insensitive by design")
	}
	t.Setenv("home", "/tmp/impostor")

	base := envutil.BaseEnv(envutil.Policy{Enabled: true})

	if _, ok := base["home"]; ok {
		t.Error(`lowercase "home" must not be allowlisted as if it were HOME`)
	}
}

// The critical test: job-supplied variables must NEVER be filtered. The
// allowlist governs only what is inherited from the daemon. A job's own
// openjd_env exports have names no operator allowlist would contain, and eating
// them would silently break jobs.
func TestBuildFromBaseNeverFiltersJobData(t *testing.T) {
	base := map[string]string{"PATH": "/usr/bin"}
	overrides := map[string]string{
		"MY_JOB_EXPORT": "from-openjd-env",
		"RENDER_SCENE":  "/shot/010.ma",
	}

	got := envutil.BuildFromBase(base, overrides, nil)

	want := map[string]string{
		"PATH":          "/usr/bin",
		"MY_JOB_EXPORT": "from-openjd-env",
		"RENDER_SCENE":  "/shot/010.ma",
	}
	gotMap := envSliceToMap(got)
	if len(gotMap) != len(want) {
		t.Errorf("job data must pass through untouched: got %d entries, want %d (got=%v, want=%v)", len(gotMap), len(want), gotMap, want)
	}
	for k, wantV := range want {
		if gotV, ok := gotMap[k]; !ok || gotV != wantV {
			t.Errorf("job data must pass through untouched: [%q] = (%q, %v); want (%q, true)", k, gotV, ok, wantV)
		}
	}
}

func TestBuildFromBaseUnsetRemovesFromBaseAndOverrides(t *testing.T) {
	base := map[string]string{"PATH": "/usr/bin", "DROP_ME": "x"}
	overrides := map[string]string{"ALSO_DROP": "y"}
	unset := map[string]bool{"DROP_ME": true, "ALSO_DROP": true}

	got := envSliceToMap(envutil.BuildFromBase(base, overrides, unset))

	if _, ok := got["DROP_ME"]; ok {
		t.Error("openjd_unset_env must remove a base variable")
	}
	if _, ok := got["ALSO_DROP"]; ok {
		t.Error("openjd_unset_env must remove an override variable")
	}
}
