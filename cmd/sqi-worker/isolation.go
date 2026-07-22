// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"log/slog"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/isolation"
)

// buildIsolationProvider constructs the platform isolation.Provider and
// enforces isolation.required at boot (see verifyIsolationCapability).
// Extracted from runStart to keep that function's cyclomatic complexity
// within the project limit.
func buildIsolationProvider(cfg workerconfig.IsolationConfig, logger *slog.Logger) (isolation.Provider, error) {
	provider, err := isolation.NewProvider(isolation.Config{Logger: logger})
	if err != nil {
		return nil, fmt.Errorf("build isolation provider: %w", err)
	}
	if err := verifyIsolationCapability(cfg, provider); err != nil {
		return nil, err
	}
	return provider, nil
}

// verifyIsolationCapability enforces isolation.required at boot. Returning an
// error here takes the worker down, which is correct: a worker that silently
// ran unisolated would look healthy while providing none of the containment an
// operator configured it for.
//
// This is deliberately separate from a per-assignment credential-resolution
// failure (handled in session.Manager.Create): that is "this queue's account
// is bad", a per-assignment problem. This is "this worker is misconfigured", a
// boot-time problem the worker cannot detect any other way — it learns
// identities only from AssignMsg, so it cannot evaluate queue config at boot.
func verifyIsolationCapability(cfg workerconfig.IsolationConfig, p isolation.Provider) error {
	if !cfg.Required {
		return nil
	}
	if err := p.Capable(); err != nil {
		return fmt.Errorf("isolation.required is set but this worker cannot isolate: %w", err)
	}
	return nil
}
