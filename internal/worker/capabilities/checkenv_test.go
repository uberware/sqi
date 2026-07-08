// SPDX-License-Identifier: AGPL-3.0-or-later

package capabilities

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOSCheckEnv_GlobAndEnv(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "hfs20.5", "bin")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "houdini"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := OSCheckEnv()

	got := env.Glob(filepath.Join(dir, "hfs*", "bin", "houdini"))
	if len(got) != 1 {
		t.Fatalf("glob: got %v, want 1 match", got)
	}
	t.Setenv("SQI_TEST_VAR", "value")
	if v, ok := env.Getenv("SQI_TEST_VAR"); !ok || v != "value" {
		t.Errorf("Getenv: got %q,%v", v, ok)
	}
	if _, ok := env.Getenv("SQI_TEST_ABSENT_VAR"); ok {
		t.Errorf("Getenv reported absent var as present")
	}
	if env.GOOS() == "" {
		t.Errorf("GOOS empty")
	}
}
