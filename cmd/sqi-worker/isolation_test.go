// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"testing"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/isolation"
)

func TestWorkerExitsWhenIsolationRequiredButNotCapable(t *testing.T) {
	cfg := workerconfig.IsolationConfig{Required: true}
	provider := isolation.NewFakeIncapable(nil) // Capable() returns ErrNotCapable

	err := verifyIsolationCapability(cfg, provider)

	if !errors.Is(err, isolation.ErrNotCapable) {
		t.Errorf("err = %v, want ErrNotCapable so the worker refuses to start", err)
	}
}

func TestWorkerStartsWhenIsolationNotRequired(t *testing.T) {
	cfg := workerconfig.IsolationConfig{Required: false}
	provider := isolation.NewFakeIncapable(nil)

	if err := verifyIsolationCapability(cfg, provider); err != nil {
		t.Errorf("err = %v, want nil — an unconfigured worker must still start", err)
	}
}

func TestWorkerStartsWhenIsolationRequiredAndCapable(t *testing.T) {
	cfg := workerconfig.IsolationConfig{Required: true}
	provider := isolation.NewFake(nil) // Capable() returns nil

	if err := verifyIsolationCapability(cfg, provider); err != nil {
		t.Errorf("err = %v, want nil when the provider reports capable", err)
	}
}
