// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"strings"
	"testing"
)

// TestLoad_MalformedTypedEnvReportsEveryVariable pins that a malformed
// integer, int64, duration or boolean variable fails the load naming it, and
// that every one is reported at once, so fixing one does not reveal the next.
// For a boolean that matters most: SQI_WORKER_ISOLATION_REQUIRED=y used to
// read as false, which turned isolation off. The setters' own parsing is
// tested in internal/envvar.
func TestLoad_MalformedTypedEnvReportsEveryVariable(t *testing.T) {
	t.Setenv("SQI_WORKER_NATS_URL", "nats://localhost:4222")
	t.Setenv("SQI_WORKER_LOG_MAX_BACKUPS", "four")
	t.Setenv("SQI_WORKER_HEARTBEAT_INTERVAL", "5 sec")
	t.Setenv("SQI_WORKER_ISOLATION_REQUIRED", "y")
	t.Setenv("SQI_WORKER_EXPR_MEMORY_LIMIT", "1MB")
	_, err := Load("", FlagOverrides{})
	if err == nil {
		t.Fatal("Load succeeded, want an error")
	}
	for _, want := range []string{
		`SQI_WORKER_LOG_MAX_BACKUPS: invalid integer "four"`,
		`SQI_WORKER_HEARTBEAT_INTERVAL: invalid duration "5 sec"`,
		`SQI_WORKER_ISOLATION_REQUIRED: invalid boolean "y"`,
		`SQI_WORKER_EXPR_MEMORY_LIMIT: invalid integer "1MB"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}
