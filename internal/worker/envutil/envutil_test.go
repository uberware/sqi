// SPDX-License-Identifier: AGPL-3.0-or-later

package envutil_test

import (
	"os"
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
