// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/executor"
	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/staging"
)

// defaultRootSessionDir is the default session working-directory root when
// the worker runs as root. /var/lib is 0755 on every real Linux/macOS
// installation, so every ancestor of this path is already traversable by any
// uid — nothing needs to be created or widened specifically to make
// run-as-user isolation work.
//
// Deliberately a SIBLING of workerconfig.defaultDataDir's own
// HOME-unset fallback (/var/lib/sqi-worker), never a descendant of it:
// LoadOrCreateWorkerID creates that directory at 0700 by design, and this
// package's boot-time isolation.ValidateTraversable walks up from
// sessionRoot through every existing ancestor requiring the "other" execute
// bit. Nesting this path under /var/lib/sqi-worker (an earlier revision used
// /var/lib/sqi-worker/sessions) would have made a capable, isolation.required
// worker refuse to start over a directory sqi itself had just created
// seconds earlier at boot — the split's whole premise is that the two never
// share an ancestor.
const defaultRootSessionDir = "/var/lib/sqi-worker-sessions"

// effectiveSessionRoot resolves the directory under which session working
// directories are created, kept deliberately separate from
// cfg.Worker.DataDir: DataDir holds only the persistent worker-id file and
// must never be widened for run-as-user traversal (see
// workerconfig.LoadOrCreateWorkerID's doc), while session working
// directories are ephemeral scratch that, when isolation is in use, must be
// traversable by whichever run-as-user identity a session resolves to.
//
// Also returns the mode session.Manager must create that root at, chosen by
// WHICH branch resolved the path rather than a single value for all of them:
//   - worker.session_dir, if set, always wins (operator override) — created
//     0711 (traversable), since an explicit override is exactly how an
//     operator opts a non-default location INTO run-as-user isolation.
//   - Running as root defaults to defaultRootSessionDir at 0711: its
//     ancestors are already traversable with no changes to anything (see
//     above), and a root worker is the only kind that can ever actually
//     isolate, so the root it's about to create needs the search bit from
//     birth.
//   - Otherwise (not root) falls back to the pre-split location under
//     worker.data_dir, at 0750 — the exact mode that location was created at
//     before this split existed (a byproduct of the single MkdirAll call
//     session.Manager used to make): real run-as-user isolation cannot
//     function without root regardless of directory permissions (see
//     isolation.unixProvider.Capable), so a fresh non-root deployment gains
//     nothing from the wider 0711 and should not silently get `other +x` it
//     will never use.
func effectiveSessionRoot(cfg workerconfig.WorkerSettings) (sessionRoot string, sessionRootMode os.FileMode) {
	return effectiveSessionRootFor(cfg, executor.IsRunningAsRoot)
}

// effectiveSessionRootFor is effectiveSessionRoot with the root check
// injected, so tests can exercise both branches deterministically regardless
// of the uid the test binary itself happens to run as.
func effectiveSessionRootFor(cfg workerconfig.WorkerSettings, isRoot func() bool) (sessionRoot string, sessionRootMode os.FileMode) {
	if cfg.SessionDir != "" {
		return cfg.SessionDir, 0o711
	}
	if isRoot() {
		return defaultRootSessionDir, 0o711
	}
	return filepath.Join(cfg.DataDir, "sessions"), 0o750
}

// validateIsolationAncestors enforces, at boot, that every ancestor of the
// session root and of staging's effective scratch base is traversable —
// refusing to start with an actionable error rather than silently chmod'ing
// anything (see isolation.ValidateTraversable's doc for why: an
// operator-chosen path may sit under a directory deliberately restricted,
// /root above all, that must never be widened by sqi).
//
// Gated on isolation.required, NOT on provider.Capable(): Capable() reduces
// to "is this worker root" on POSIX, and root and will-actually-isolate are
// not the same predicate — a root worker that happens to never receive an
// isolated assignment (isolation not configured on any queue it serves) has
// no business being refused boot over a directory permission it will never
// need. isolation.required is the one signal that means the operator
// explicitly wants isolation and refusing to start over it is unambiguously
// correct. When required is false, the IDENTICAL check (same actionable
// message) instead runs per-assignment, the moment isolation actually
// arrives — see session.Manager.Create and staging.Stager.StageIn — so that
// case fails only the one task, not the whole worker.
func validateIsolationAncestors(required bool, sessionRoot, stagingScratchDir string) error {
	if !required {
		return nil
	}
	scratchBase := staging.EffectiveScratchBase(stagingScratchDir)
	if err := isolation.ValidateTraversable(sessionRoot, scratchBase); err != nil {
		return fmt.Errorf("worker cannot start: %w", err)
	}
	return nil
}

// resolveIsolation builds the isolation provider, resolves the effective
// session root (and the mode it must be created at), and — only when
// isolation.required is set — validates its (and staging's scratch base's)
// ancestors up front. This is the full boot-time isolation sequence,
// extracted from runStart to keep that function's cyclomatic complexity
// within the project limit. Returns the provider, sessionRoot, and
// sessionRootMode ready to hand to session.NewManager, plus
// isolationCapable — the same executor.IsRunningAsRoot() signal
// effectiveSessionRoot's own root branch uses, threaded through to
// executor.Config.StagingIsolationCapable so staging.Stager decides its
// shared scratch-base ancestor mode once per worker instead of per
// assignment (see staging.WithIsolationCapable's doc).
func resolveIsolation(cfg workerconfig.WorkerConfig, logger *slog.Logger) (provider isolation.Provider, sessionRoot string, sessionRootMode os.FileMode, isolationCapable bool, err error) {
	provider, err = buildIsolationProvider(cfg.Isolation, cfg.Worker.DataDir, logger)
	if err != nil {
		return nil, "", 0, false, err
	}
	sessionRoot, sessionRootMode = effectiveSessionRoot(cfg.Worker)
	if err := validateIsolationAncestors(cfg.Isolation.Required, sessionRoot, cfg.Staging.ScratchDir); err != nil {
		return nil, "", 0, false, err
	}
	return provider, sessionRoot, sessionRootMode, executor.IsRunningAsRoot(), nil
}

// buildIsolationProvider constructs the platform isolation.Provider and
// enforces isolation.required at boot (see verifyIsolationCapability).
// Extracted from runStart to keep that function's cyclomatic complexity
// within the project limit.
func buildIsolationProvider(cfg workerconfig.IsolationConfig, dataDir string, logger *slog.Logger) (isolation.Provider, error) {
	provider, err := isolation.NewProvider(isolation.Config{
		Logger:          logger,
		Provider:        cfg.Provider,
		CredentialStore: isolation.NewFileStore(isolation.CredentialDir(dataDir)),
	})
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
