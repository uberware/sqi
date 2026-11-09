// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/api"
	"github.com/uberware/sqi/internal/capacity/script"
	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/health"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/scheduler"
	"github.com/uberware/sqi/internal/store/fake"
)

// capacityConfigWithSecrets is a capacity block whose providers carry an env
// secret, a command and a timeout that no API response may show.
func capacityConfigWithSecrets() config.CapacityConfig {
	return config.CapacityConfig{
		Providers: map[string]config.CapacityProviderConfig{
			"zeta": {
				Command: []string{"/opt/sqi/zeta-provider.sh", "--region", "eu-west-9"},
				Env:     map[string]string{"ZETA_TOKEN": "zeta-secret-value-8812"},
				Timeout: 7 * time.Minute,
			},
			"aws": {
				Command: []string{"/opt/sqi/aws-provider"},
				Env:     map[string]string{"AWS_SECRET_ACCESS_KEY": "aws-secret-value-4471"},
				Timeout: 3 * time.Minute,
			},
		},
		WakeOnLAN: config.WakeOnLANConfig{Port: 4009, SendTo: []string{"192.168.9.255:9"}},
	}
}

// TestSchedulerConfig_CarriesTheCapacityProviders pins the conversion of the
// capacity block onto the scheduler: script providers keyed by their bare
// name, every field carried, and the wake-on-LAN sender. schedulerConfigFor is
// what both the single-server start and an HA standby's scheduler are built
// from, so this covers both paths.
func TestSchedulerConfig_CarriesTheCapacityProviders(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Capacity = capacityConfigWithSecrets()
	want := map[string]script.Provider{
		"zeta": {
			Name: "zeta", Command: []string{"/opt/sqi/zeta-provider.sh", "--region", "eu-west-9"},
			Env: map[string]string{"ZETA_TOKEN": "zeta-secret-value-8812"}, Timeout: 7 * time.Minute,
		},
		"aws": {
			Name: "aws", Command: []string{"/opt/sqi/aws-provider"},
			Env: map[string]string{"AWS_SECRET_ACCESS_KEY": "aws-secret-value-4471"}, Timeout: 3 * time.Minute,
		},
	}
	s := &Server{cfg: cfg, logger: testLogger()}
	for name, build := range map[string]func() scheduler.Config{
		"schedulerConfig":    func() scheduler.Config { return schedulerConfig(cfg, nil) },
		"schedulerConfigFor": func() scheduler.Config { return s.schedulerConfigFor(nil) },
	} {
		t.Run(name, func(t *testing.T) {
			sc := build()
			if !reflect.DeepEqual(sc.ScriptProviders, want) {
				t.Errorf("ScriptProviders = %+v\nwant %+v", sc.ScriptProviders, want)
			}
			if sc.WakeOnLANPort != 4009 {
				t.Errorf("WakeOnLANPort = %d, want 4009", sc.WakeOnLANPort)
			}
			if !reflect.DeepEqual(sc.WakeOnLANSendTo, []string{"192.168.9.255:9"}) {
				t.Errorf("WakeOnLANSendTo = %v", sc.WakeOnLANSendTo)
			}
		})
	}
}

// TestSchedulerConfig_NoCapacityBlock: with no capacity block the scheduler
// gets no script provider and keeps its own wake-on-LAN defaults.
func TestSchedulerConfig_NoCapacityBlock(t *testing.T) {
	sc := schedulerConfig(DefaultConfig(), nil)
	if len(sc.ScriptProviders) != 0 || sc.WakeOnLANPort != 0 || len(sc.WakeOnLANSendTo) != 0 {
		t.Errorf("scheduler capacity settings = %v %d %v, want none", sc.ScriptProviders, sc.WakeOnLANPort, sc.WakeOnLANSendTo)
	}
}

// TestCapacityProviders_ListsConfiguredNamesOnly drives GET
// /capacity-providers through the server's real wiring: the configured
// providers are listed by name, sorted, and nothing of their configuration
// (env names or values, commands, timeouts) reaches the body.
func TestCapacityProviders_ListsConfiguredNamesOnly(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Capacity = capacityConfigWithSecrets()
	st := fake.New()
	t.Cleanup(func() { st.Close() })
	s := &Server{cfg: cfg, store: st, logger: testLogger()}

	deps := s.apiDeps()
	if want := []string{"aws", "zeta"}; !reflect.DeepEqual(deps.CapacityProviderNames, want) {
		t.Fatalf("CapacityProviderNames = %v, want %v (sorted, bare names)", deps.CapacityProviderNames, want)
	}
	r := api.NewRouter(api.Config{DisableRateLimit: true}, deps, testLogger(), metrics.New(), health.NewRegistry())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/capacity-providers", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	want := `{"providers":[{"name":"external","kinds":["named","sized"]},{"name":"wake_on_lan","kinds":["named"]},` +
		`{"name":"script:aws","kinds":["named","sized"]},{"name":"script:zeta","kinds":["named","sized"]}]}`
	if got := strings.TrimSpace(body); got != want {
		t.Errorf("body = %s\nwant   %s", got, want)
	}
	for _, secret := range []string{
		"zeta-secret-value-8812", "aws-secret-value-4471", "ZETA_TOKEN", "AWS_SECRET_ACCESS_KEY",
		"/opt/sqi", "eu-west-9", "7m0s", "420", "192.168.9.255",
	} {
		if strings.Contains(body, secret) {
			t.Errorf("body contains %q: %s", secret, body)
		}
	}
}
