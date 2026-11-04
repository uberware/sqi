// SPDX-License-Identifier: AGPL-3.0-or-later

package target

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/simworker"
)

// lookup walks a decoded YAML document along a dotted key path.
func lookup(m map[string]any, path string) (any, bool) {
	var cur any = m
	for part := range strings.SplitSeq(path, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = mm[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func TestManagedConfig_YAML(t *testing.T) {
	runDir := t.TempDir()
	s := serverSettings{
		RunDir:   runDir,
		HTTPAddr: "127.0.0.1:18080",
		NATSAddr: "127.0.0.1:14222",
		Spec: scenario.ServerSpec{
			Store:      "sqlite",
			BrokerAuth: true,
			HTTPAuth:   true,
			Config: map[string]any{
				"scheduler":   map[string]any{"heartbeat_timeout": "45s"},
				"diagnostics": map[string]any{"buffer_size": 50},
			},
		},
		BootstrapUser: "sqi-load",
	}
	data, err := renderServerConfig(s)
	if err != nil {
		t.Fatalf("renderServerConfig: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("generated YAML does not parse: %v\n%s", err, data)
	}

	want := []struct {
		path string
		val  any
	}{
		{"http.addr", "127.0.0.1:18080"},
		{"http.enable_pprof", true},
		{"http.rate_limit.enabled", false},
		{"nats.mode", "embedded"},
		{"nats.embedded.addr", "127.0.0.1:14222"},
		{"nats.embedded.data_dir", filepath.Join(runDir, "data", "nats")},
		{"nats.auth.enabled", true},
		{"nats.auth.join_token_single_use", false},
		{"nats.auth.enrollment_endpoint_enabled", true},
		{"store.driver", "sqlite"},
		{"store.sqlite_path", filepath.Join(runDir, "data", "sqi.db")},
		{"discovery.enabled", false},
		{"auth.enabled", true},
		{"auth.bootstrap.username", "sqi-load"},
		// The scenario's server.config is merged last: it adds keys the
		// harness leaves at the server default and overrides harness defaults.
		{"scheduler.heartbeat_timeout", "45s"},
		{"diagnostics.buffer_size", 50},
	}
	for _, w := range want {
		got, ok := lookup(doc, w.path)
		if !ok {
			t.Errorf("%s missing from server.yaml", w.path)
			continue
		}
		if got != w.val {
			t.Errorf("%s = %#v, want %#v", w.path, got, w.val)
		}
	}
	if _, ok := lookup(doc, "auth.bootstrap.password"); ok {
		t.Error("server.yaml carries the bootstrap password; it must reach the server through the environment only")
	}

	// The real loader must read every key the harness writes: an ignored key
	// would silently leave the server at a default the run did not ask for.
	path := filepath.Join(runDir, "server.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SQI_AUTH_BOOTSTRAP_PASSWORD", "not-a-real-password")
	cfg, err := config.Load(path, config.FlagOverrides{})
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if errs := config.Validate(cfg); len(errs) > 0 {
		t.Fatalf("config.Validate: %v", errs)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"http.rate_limit.enabled", cfg.HTTP.RateLimit.Enabled, false},
		{"http.enable_pprof", cfg.HTTP.EnablePprof, true},
		{"nats.auth.join_token_single_use", cfg.NATS.Auth.JoinTokenSingleUse, false},
		{"nats.auth.enrollment_endpoint_enabled", cfg.NATS.Auth.EnrollmentEndpointEnabled, true},
		{"nats.auth.enabled", cfg.NATS.Auth.Enabled, true},
		{"nats.embedded.addr", cfg.NATS.Embedded.Addr, "127.0.0.1:14222"},
		{"nats.embedded.data_dir", cfg.NATS.Embedded.DataDir, filepath.Join(runDir, "data", "nats")},
		{"store.sqlite_path", cfg.Store.SQLitePath, filepath.Join(runDir, "data", "sqi.db")},
		{"diagnostics.buffer_size", cfg.Diagnostics.BufferSize, 50},
		{"discovery.enabled", cfg.Discovery.Enabled, false},
		{"auth.enabled", cfg.Auth.Enabled, true},
		{"auth.bootstrap.username", cfg.Auth.Bootstrap.Username, "sqi-load"},
		{"scheduler.heartbeat_timeout", cfg.Scheduler.HeartbeatTimeout, 45 * time.Second},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("loaded %s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestManagedConfig_Variants(t *testing.T) {
	tests := []struct {
		name    string
		spec    scenario.ServerSpec
		present map[string]any
		absent  []string
	}{
		{
			name:    "defaults",
			spec:    scenario.ServerSpec{Store: "sqlite"},
			present: map[string]any{"diagnostics.buffer_size": 1000, "http.rate_limit.enabled": false},
			absent:  []string{"nats.auth", "auth", "http.tls", "nats.embedded.tls", "store.postgres"},
		},
		{
			name: "tls",
			spec: scenario.ServerSpec{Store: "sqlite", TLS: true},
			present: map[string]any{
				"http.tls.enabled":          true,
				"http.tls.cert_file":        filepath.Join("RUN", "tls", "server.crt"),
				"http.tls.key_file":         filepath.Join("RUN", "tls", "server.key"),
				"nats.embedded.tls.enabled": true,
			},
		},
		{
			// The DSN can carry a password, so it reaches the server through
			// its environment and never lands in server.yaml.
			name:    "postgres",
			spec:    scenario.ServerSpec{Store: "postgres"},
			present: map[string]any{"store.driver": "postgres"},
			absent:  []string{"store.sqlite_path", "store.postgres"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := renderServerConfig(serverSettings{
				RunDir: "RUN", HTTPAddr: "127.0.0.1:1", NATSAddr: "127.0.0.1:2",
				Spec: tt.spec, BootstrapUser: "sqi-load",
			})
			if err != nil {
				t.Fatalf("renderServerConfig: %v", err)
			}
			var doc map[string]any
			if err := yaml.Unmarshal(data, &doc); err != nil {
				t.Fatal(err)
			}
			for path, want := range tt.present {
				if got, ok := lookup(doc, path); !ok || got != want {
					t.Errorf("%s = %#v (present %v), want %#v", path, got, ok, want)
				}
			}
			for _, path := range tt.absent {
				if _, ok := lookup(doc, path); ok {
					t.Errorf("%s is set, want it left at the server default", path)
				}
			}
		})
	}
}

func TestManagedConfig_RejectsBadOverrides(t *testing.T) {
	tests := []struct {
		name     string
		override map[string]any
		wantErr  string
	}{
		{"unknown top-level key", map[string]any{"shceduler": map[string]any{"tick_interval": "1s"}}, "shceduler"},
		{"unknown nested key", map[string]any{"scheduler": map[string]any{"heartbeat_timout": "1s"}}, "scheduler.heartbeat_timout"},
		{"key under a leaf", map[string]any{"http": map[string]any{"enable_pprof": map[string]any{"x": 1}}}, "http.enable_pprof"},
		{"removed flat nats key", map[string]any{"nats": map[string]any{"data_dir": "x"}}, "nats.data_dir"},
		{"harness-owned address", map[string]any{"http": map[string]any{"addr": ":9999"}}, "http.addr"},
		{"harness-owned store", map[string]any{"store": map[string]any{"sqlite_path": "x.db"}}, "store.sqlite_path"},
		{"harness-owned auth switch", map[string]any{"auth": map[string]any{"enabled": true}}, "server.http_auth"},
		{"harness-owned broker switch", map[string]any{"nats": map[string]any{"auth": map[string]any{"enabled": true}}}, "server.broker_auth"},
		{"harness-owned tls", map[string]any{"http": map[string]any{"tls": map[string]any{"enabled": true}}}, "server.tls"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := renderServerConfig(serverSettings{
				RunDir: "RUN", HTTPAddr: "127.0.0.1:1", NATSAddr: "127.0.0.1:2",
				Spec: scenario.ServerSpec{Store: "sqlite", Config: tt.override},
			})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want one naming %q", err, tt.wantErr)
			}
		})
	}
}

func TestDeepMerge(t *testing.T) {
	dst := map[string]any{
		"a": map[string]any{"x": 1, "y": 2},
		"b": 3,
	}
	src := map[string]any{
		"a": map[string]any{"y": 20, "z": 30},
		"b": map[string]any{"replaced": true},
		"c": "new",
	}
	deepMerge(dst, src)
	want := map[string]any{
		"a": map[string]any{"x": 1, "y": 20, "z": 30},
		"b": map[string]any{"replaced": true},
		"c": "new",
	}
	gotY, err := yaml.Marshal(dst)
	if err != nil {
		t.Fatal(err)
	}
	wantY, err := yaml.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotY) != string(wantY) {
		t.Errorf("deepMerge =\n%s\nwant\n%s", gotY, wantY)
	}
	// The source must not alias into the result: a later merge into dst would
	// otherwise write through into the scenario.
	dstA, ok := dst["a"].(map[string]any)
	if !ok {
		t.Fatalf("dst[a] is %T", dst["a"])
	}
	srcA, ok := src["a"].(map[string]any)
	if !ok {
		t.Fatalf("src[a] is %T", src["a"])
	}
	dstA["x"] = 99
	if _, ok := srcA["x"]; ok {
		t.Error("deepMerge aliased the source map into the destination")
	}
}

func TestIsBindError(t *testing.T) {
	tests := []struct {
		log  string
		want bool
	}{
		{`{"msg":"http server failed","error":"listen tcp 127.0.0.1:8080: bind: address already in use"}`, true},
		{"listen tcp 127.0.0.1:4222: bind: Only one usage of each socket address (protocol/network address/port) is normally permitted.", true},
		{"Error listening on port: 127.0.0.1:4222, \"listen tcp 127.0.0.1:4222: bind: An attempt was made to access a socket in a way forbidden by its access permissions.\"", true},
		{"load config: 1 configuration error(s): log.level must be one of debug, info", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isBindError(tt.log); got != tt.want {
			t.Errorf("isBindError(%q) = %v, want %v", tt.log, got, tt.want)
		}
	}
}

func TestDatabaseName(t *testing.T) {
	tests := []struct {
		runID, want string
	}{
		{"20261103-142501-ab12", "sqi_load_20261103_142501_ab12"},
		{"Smoke.Run", "sqi_load_smoke_run"},
		{"", "sqi_load_run"},
		{strings.Repeat("x", 100), "sqi_load_" + strings.Repeat("x", 54)},
	}
	for _, tt := range tests {
		got := databaseName(tt.runID)
		if got != tt.want {
			t.Errorf("databaseName(%q) = %q, want %q", tt.runID, got, tt.want)
		}
		if len(got) > 63 {
			t.Errorf("databaseName(%q) is %d bytes, over PostgreSQL's 63", tt.runID, len(got))
		}
	}
}

func TestPostgresDSN(t *testing.T) {
	tests := []struct {
		name      string
		dsn       string
		wantAdmin bool
		wantDSN   string // the run's DSN when admin
	}{
		{
			"url admin", "postgres://u:pw@127.0.0.1:5432/postgres?sslmode=disable", true,
			"postgres://u:pw@127.0.0.1:5432/sqi_load_r1?sslmode=disable",
		},
		{"url operator database", "postgres://u@db:5432/loadtest", false, "postgres://u@db:5432/loadtest"},
		{
			"keyword admin", "host=127.0.0.1 user=u dbname=postgres sslmode=disable", true,
			"host=127.0.0.1 user=u dbname=postgres sslmode=disable dbname=sqi_load_r1",
		},
		{"keyword operator database", "host=db user=u dbname=loadtest", false, "host=db user=u dbname=loadtest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			admin, err := isAdminDSN(tt.dsn)
			if err != nil {
				t.Fatalf("isAdminDSN: %v", err)
			}
			if admin != tt.wantAdmin {
				t.Fatalf("isAdminDSN = %v, want %v", admin, tt.wantAdmin)
			}
			got := tt.dsn
			if admin {
				got, err = withDatabase(tt.dsn, "sqi_load_r1")
				if err != nil {
					t.Fatalf("withDatabase: %v", err)
				}
			}
			if got != tt.wantDSN {
				t.Errorf("run DSN = %q, want %q", got, tt.wantDSN)
			}
		})
	}
}

func TestBuildUpToDate(t *testing.T) {
	dir := t.TempDir()
	names := []string{"a", "b"}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeStamp(dir, "abc123"); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		head  string
		dirty bool
		names []string
		want  bool
	}{
		{"same clean revision", "abc123", false, names, true},
		{"new revision", "def456", false, names, false},
		{"dirty tree", "abc123", true, names, false},
		{"no git", "", false, names, false},
		{"binary missing", "abc123", false, []string{"a", "b", "c"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := upToDate(dir, tt.head, tt.dirty, tt.names); got != tt.want {
				t.Errorf("upToDate = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestChildEnv(t *testing.T) {
	base := []string{"PATH=/bin", "SQI_HTTP_ADDR=:1", "sqi_store_driver=postgres", "HOME=/h", "SQIX=keep"}
	got := childEnv(base, "SQI_WORKER_FARM_ID=f")
	want := []string{"PATH=/bin", "HOME=/h", "SQIX=keep", "SQI_WORKER_FARM_ID=f"}
	if !slices.Equal(got, want) {
		t.Errorf("childEnv = %v, want %v", got, want)
	}
}

func TestTailLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	var b strings.Builder
	for i := range 100 {
		b.WriteString("line ")
		b.WriteString(strings.Repeat("x", i%7))
		b.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	got := tailLines(path, 40)
	if n := strings.Count(got, "\n"); n != 40 {
		t.Errorf("tailLines returned %d lines, want 40", n)
	}
	if !strings.HasSuffix(got, "line "+strings.Repeat("x", 99%7)+"\n") {
		t.Errorf("tailLines does not end with the last line: %q", got)
	}
	if got := tailLines(filepath.Join(t.TempDir(), "missing"), 40); !strings.Contains(got, "no log") {
		t.Errorf("tailLines of a missing file = %q", got)
	}
}

func TestAttach_RejectsIncompleteConfig(t *testing.T) {
	sc := &scenario.Scenario{Farm: scenario.FarmSpec{Queues: []scenario.QueueSpec{{Name: "default"}}}}
	good := AttachConfig{HTTPURL: "http://127.0.0.1:1", NATSURL: "nats://127.0.0.1:2", RunID: "r", Scenario: sc, NoAuth: true}
	tests := []struct {
		name    string
		mutate  func(*AttachConfig)
		wantErr string
	}{
		{"no http url", func(c *AttachConfig) { c.HTTPURL = "" }, "HTTP URL"},
		{"no nats url", func(c *AttachConfig) { c.NATSURL = "" }, "NATS URL"},
		{"no run id", func(c *AttachConfig) { c.RunID = "" }, "run ID"},
		{"no scenario", func(c *AttachConfig) { c.Scenario = nil }, "scenario"},
		{"auth without a join token", func(c *AttachConfig) { c.NoAuth = false }, "join token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := good
			tt.mutate(&cfg)
			_, err := Attach(context.Background(), cfg)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Attach err = %v, want one naming %q", err, tt.wantErr)
			}
		})
	}
}

func TestStartManaged_RejectsIncompleteConfig(t *testing.T) {
	sc := &scenario.Scenario{Server: scenario.ServerSpec{Store: "postgres"}}
	tests := []struct {
		name    string
		cfg     ManagedConfig
		wantErr string
	}{
		{"no run dir", ManagedConfig{Scenario: &scenario.Scenario{}}, "RunDir"},
		{"no scenario", ManagedConfig{RunDir: t.TempDir()}, "scenario"},
		{"postgres without a DSN", ManagedConfig{RunDir: t.TempDir(), Scenario: sc}, "PostgresDSN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := StartManaged(context.Background(), tt.cfg)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("StartManaged err = %v, want one naming %q", err, tt.wantErr)
			}
		})
	}
}

// Compile-time checks that both targets satisfy the interface and hand out
// the simworker types the fleet consumes.
var (
	_ Target                = (*managed)(nil)
	_ Target                = (*attached)(nil)
	_ simworker.Credentials = simworker.NoCredentials{}
)

func TestProcessHelpers_NotManaged(t *testing.T) {
	tests := []struct {
		name string
		tgt  Target
	}{
		{"attached", &attached{}},
		{"nil", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PIDs(tt.tgt); got != nil {
				t.Errorf("PIDs = %v, want nil", got)
			}
			if got := PostgresDSN(tt.tgt); got != "" {
				t.Errorf("PostgresDSN = %q, want empty", got)
			}
		})
	}
	if !ProcessAlive(os.Getpid()) {
		t.Error("ProcessAlive(own pid) = false")
	}
}
