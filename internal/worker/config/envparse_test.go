// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// typedEnvVars lists every SQI_* variable the worker parses into a number, a
// duration or a boolean. TestLoad_MalformedTypedEnvFailsLoad walks it, so a
// typed variable added to applyEnv without an entry here goes unchecked.
var typedEnvVars = []struct {
	name, kind string
}{
	{"SQI_WORKER_NATS_MAX_RECONNECT_ATTEMPTS", "integer"},
	{"SQI_WORKER_NATS_RECONNECT_WAIT", "duration"},
	{"SQI_WORKER_NATS_INSECURE_SKIP_VERIFY", "boolean"},
	{"SQI_WORKER_NATS_SERVER_TLS_INSECURE_SKIP_VERIFY", "boolean"},
	{"SQI_WORKER_HEARTBEAT_INTERVAL", "duration"},
	{"SQI_WORKER_SHUTDOWN_GRACE_PERIOD", "duration"},
	{"SQI_WORKER_ALLOW_ROOT", "boolean"},
	{"SQI_WORKER_KEEP_FAILED_SESSIONS", "boolean"},
	{"SQI_WORKER_PULL_IDLE_BACKOFF", "duration"},
	{"SQI_WORKER_PULL_NACK_DELAY", "duration"},
	{"SQI_WORKER_LOG_MAX_SIZE_MB", "integer"},
	{"SQI_WORKER_LOG_MAX_BACKUPS", "integer"},
	{"SQI_WORKER_METRICS_TLS_ENABLED", "boolean"},
	{"SQI_WORKER_METRICS_ENABLE_PPROF", "boolean"},
	{"SQI_WORKER_DISCOVERY_ENABLE_MDNS", "boolean"},
	{"SQI_WORKER_DISCOVERY_MDNS_TIMEOUT", "duration"},
	{"SQI_WORKER_LOG_STREAMER_MAX_LINES_PER_CHUNK", "integer"},
	{"SQI_WORKER_LOG_STREAMER_MAX_BYTES_PER_CHUNK", "integer"},
	{"SQI_WORKER_LOG_STREAMER_FLUSH_INTERVAL", "duration"},
	{"SQI_DIAGNOSTICS_ENABLED", "boolean"},
	{"SQI_STAGING_DEFAULTS", "boolean"},
	{"SQI_WORKER_ISOLATION_REQUIRED", "boolean"},
	{"SQI_WORKER_EXPR_OPERATION_LIMIT", "integer"},
	{"SQI_WORKER_EXPR_MEMORY_LIMIT", "integer"},
	{"SQI_WORKER_EXPR_ASSIGNMENT_POSITIONS", "integer"},
	{"SQI_WORKER_EXPR_ASSIGNMENT_RETAINED_BYTES", "integer"},
	{"SQI_WORKER_EXPR_LET_RETAINED_BYTES", "integer"},
}

// TestTypedEnvVars_CoverEverySetter fails when config.go parses a variable
// with a typed setter that typedEnvVars does not list, or lists one it no
// longer parses, so the table cannot drift from the code it checks.
func TestTypedEnvVars_CoverEverySetter(t *testing.T) {
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{"Int": "integer", "Int64": "integer", "Duration": "duration", "Bool": "boolean"}
	inCode := map[string]string{}
	re := regexp.MustCompile(`set(Int|Int64|Duration|Bool)Env\(&[^,]+, "([A-Z_]+)"\)`)
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		inCode[m[2]] = kinds[m[1]]
	}
	inTable := map[string]string{}
	for _, v := range typedEnvVars {
		inTable[v.name] = v.kind
	}
	for name, kind := range inCode {
		if inTable[name] != kind {
			t.Errorf("config.go parses %s as %s; typedEnvVars has %q", name, kind, inTable[name])
		}
	}
	for name := range inTable {
		if _, ok := inCode[name]; !ok {
			t.Errorf("typedEnvVars lists %s, which config.go does not parse with a typed setter", name)
		}
	}
}

// TestLoad_MalformedTypedEnvFailsLoad pins that a value that does not parse
// fails the load naming the variable, rather than leaving the lower layer's
// value in place. For a boolean that matters most: SQI_WORKER_ISOLATION_REQUIRED=y
// used to read as false, which turned isolation off.
func TestLoad_MalformedTypedEnvFailsLoad(t *testing.T) {
	for _, tc := range typedEnvVars {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SQI_WORKER_NATS_URL", "nats://localhost:4222")
			t.Setenv(tc.name, "bogus")
			_, err := Load("", FlagOverrides{})
			if err == nil {
				t.Fatalf("%s=bogus: Load succeeded, want an error", tc.name)
			}
			want := tc.name + `: invalid ` + tc.kind + ` "bogus"`
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
		})
	}
}

// TestLoad_MalformedTypedEnvReportsEveryVariable pins that every malformed
// variable is reported at once, so fixing one does not reveal the next.
func TestLoad_MalformedTypedEnvReportsEveryVariable(t *testing.T) {
	t.Setenv("SQI_WORKER_NATS_URL", "nats://localhost:4222")
	t.Setenv("SQI_WORKER_HEARTBEAT_INTERVAL", "5 sec")
	t.Setenv("SQI_WORKER_ISOLATION_REQUIRED", "y")
	t.Setenv("SQI_WORKER_EXPR_MEMORY_LIMIT", "1MB")
	_, err := Load("", FlagOverrides{})
	if err == nil {
		t.Fatal("Load succeeded, want an error")
	}
	for _, name := range []string{
		"SQI_WORKER_HEARTBEAT_INTERVAL", "SQI_WORKER_ISOLATION_REQUIRED", "SQI_WORKER_EXPR_MEMORY_LIMIT",
	} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name %s", err, name)
		}
	}
}

// TestLoad_BooleanEnvTokens pins the accepted spellings, which match the
// server's and nats.tls_enabled's: 1/true/yes/on and 0/false/no/off, any case,
// surrounding whitespace ignored.
func TestLoad_BooleanEnvTokens(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"yes", true},
		{"On", true},
		{" true ", true},
		{"0", false},
		{"false", false},
		{"FALSE", false},
		{"no", false},
		{"Off", false},
		{" off ", false},
	}
	for _, tc := range tests {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("SQI_WORKER_NATS_URL", "nats://localhost:4222")
			t.Setenv("SQI_WORKER_KEEP_FAILED_SESSIONS", tc.value)
			cfg, err := Load("", FlagOverrides{})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Worker.KeepFailedSessions != tc.want {
				t.Errorf("KeepFailedSessions = %v, want %v", cfg.Worker.KeepFailedSessions, tc.want)
			}
		})
	}
}

// TestLoad_TypedEnvTrimsWhitespace pins that surrounding whitespace, which a
// unit or Compose file can carry, does not make a number or duration fail.
func TestLoad_TypedEnvTrimsWhitespace(t *testing.T) {
	t.Setenv("SQI_WORKER_NATS_URL", "nats://localhost:4222")
	t.Setenv("SQI_WORKER_HEARTBEAT_INTERVAL", " 7s ")
	t.Setenv("SQI_WORKER_LOG_MAX_BACKUPS", " 4 ")
	t.Setenv("SQI_WORKER_EXPR_OPERATION_LIMIT", " 12345 ")
	cfg, err := Load("", FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Worker.HeartbeatInterval != 7*time.Second {
		t.Errorf("HeartbeatInterval = %v, want 7s", cfg.Worker.HeartbeatInterval)
	}
	if cfg.Log.MaxBackups != 4 {
		t.Errorf("MaxBackups = %d, want 4", cfg.Log.MaxBackups)
	}
	if cfg.Expr.OperationLimit != 12345 {
		t.Errorf("OperationLimit = %d, want 12345", cfg.Expr.OperationLimit)
	}
}
