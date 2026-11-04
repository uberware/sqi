// SPDX-License-Identifier: AGPL-3.0-or-later

package target

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/loadtest/scenario"
)

// serverSettings is what a managed server's configuration is made from.
// Secrets are not here: they reach the server through its environment (see
// [managed.serverEnv]), so server.yaml holds none.
type serverSettings struct {
	RunDir             string
	HTTPAddr, NATSAddr string
	Spec               scenario.ServerSpec
	BootstrapUser      string
}

func (s serverSettings) dataDir() string { return filepath.Join(s.RunDir, "data") }
func (s serverSettings) tlsDir() string  { return filepath.Join(s.RunDir, "tls") }

// renderServerConfig returns the server.yaml of a managed server: the
// harness's settings with the scenario's server.config merged over them.
func renderServerConfig(s serverSettings) ([]byte, error) {
	if err := checkOverride(s.Spec.Config); err != nil {
		return nil, err
	}
	doc := harnessConfig(s)
	deepMerge(doc, s.Spec.Config)
	body, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("target: encode server.yaml: %w", err)
	}
	return append([]byte("# sqi-server configuration written by the load harness for one run.\n"), body...), nil
}

// harnessConfig is the harness's own server configuration: loopback ports,
// state under the run directory, profiling on, the rate limiter off (one
// generator address would otherwise be throttled at once), and the security
// features the scenario asks for. Everything else stays at the server's
// default, which is what a run measures.
func harnessConfig(s serverSettings) map[string]any {
	httpSec := map[string]any{
		"addr":         s.HTTPAddr,
		"enable_pprof": true,
		"rate_limit":   map[string]any{"enabled": false},
	}
	embedded := map[string]any{
		"addr":     s.NATSAddr,
		"data_dir": filepath.Join(s.dataDir(), "nats"),
	}
	if s.Spec.TLS {
		tlsSec := func() map[string]any {
			return map[string]any{
				"enabled":   true,
				"cert_file": filepath.Join(s.tlsDir(), "server.crt"),
				"key_file":  filepath.Join(s.tlsDir(), "server.key"),
			}
		}
		httpSec["tls"] = tlsSec()
		embedded["tls"] = tlsSec()
	}
	nats := map[string]any{"mode": "embedded", "embedded": embedded}
	if s.Spec.BrokerAuth {
		nats["auth"] = map[string]any{
			"enabled":                     true,
			"join_token_single_use":       false,
			"enrollment_endpoint_enabled": true,
		}
	}
	store := map[string]any{"driver": "sqlite", "sqlite_path": filepath.Join(s.dataDir(), "sqi.db")}
	if s.Spec.Store == "postgres" {
		store = map[string]any{"driver": "postgres"} // the DSN goes through the environment
	}
	doc := map[string]any{
		"http":        httpSec,
		"nats":        nats,
		"store":       store,
		"discovery":   map[string]any{"enabled": false},
		"diagnostics": map[string]any{"buffer_size": 1000},
	}
	if s.Spec.HTTPAuth {
		doc["auth"] = map[string]any{
			"enabled":   true,
			"bootstrap": map[string]any{"username": s.BootstrapUser},
		}
	}
	return doc
}

// deepMerge merges src into dst: maps merge key by key, anything else in src
// replaces dst's value. src is copied, never aliased into dst.
func deepMerge(dst, src map[string]any) {
	for k, v := range src {
		sv, srcIsMap := v.(map[string]any)
		dv, dstIsMap := dst[k].(map[string]any)
		switch {
		case srcIsMap && dstIsMap:
			deepMerge(dv, sv)
		case srcIsMap:
			cp := map[string]any{}
			deepMerge(cp, sv)
			dst[k] = cp
		default:
			dst[k] = v
		}
	}
}

// ownedKeys are the settings the harness decides. An override of one would
// leave the harness talking to a server it does not know how to reach, or
// measuring a different setup than the scenario says; each names the
// scenario field that controls it instead, if any.
var ownedKeys = []struct{ path, use string }{
	{"http.addr", "the harness picks a free loopback port"},
	{"http.tls", "set server.tls"},
	{"nats.mode", "a managed target runs the embedded broker"},
	{"nats.embedded.addr", "the harness picks a free loopback port"},
	{"nats.embedded.data_dir", "the harness keeps state in the run directory"},
	{"nats.embedded.tls", "set server.tls"},
	{"nats.auth.enabled", "set server.broker_auth"},
	{"store.driver", "set server.store"},
	{"store.sqlite_path", "the harness keeps state in the run directory"},
	{"store.postgres.dsn", "pass the PostgreSQL DSN to the harness"},
	{"auth.enabled", "set server.http_auth"},
	{"auth.bootstrap", "the harness creates its own admin"},
	{"log.file", "the harness captures the server's output in server.log"},
}

// checkOverride rejects keys of the scenario's server.config that the harness
// owns, or that sqi-server would not accept: its loader rejects an unknown
// key, so a misspelled one fails here, before a server is started for it.
func checkOverride(m map[string]any) error {
	if err := checkOwned(m, ""); err != nil {
		return err
	}
	if len(m) == 0 {
		return nil
	}
	body, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("target: encode server.config: %w", err)
	}
	if err := config.CheckFileKeys(body); err != nil {
		return fmt.Errorf("target: server.config is not sqi-server configuration: %w", err)
	}
	return nil
}

// checkOwned rejects any key of m, at any depth, that the harness owns.
func checkOwned(m map[string]any, prefix string) error {
	for _, k := range slices.Sorted(maps.Keys(m)) {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		for _, o := range ownedKeys {
			if path == o.path || strings.HasPrefix(path, o.path+".") {
				return fmt.Errorf("target: server.config.%s is set by the harness: %s", path, o.use)
			}
		}
		if sub, ok := m[k].(map[string]any); ok {
			if err := checkOwned(sub, path); err != nil {
				return err
			}
		}
	}
	return nil
}
