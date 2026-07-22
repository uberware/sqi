// SPDX-License-Identifier: AGPL-3.0-or-later

// Package isolation runs job-supplied processes under a queue-configured OS
// identity instead of the worker daemon's own account.
//
// Without it, submitting a job is arbitrary code execution as the worker service
// account: every task inherits the daemon's uid and environment. OpenJD's own
// model assumes a per-queue OS user performs this containment.
//
// The platform-specific pieces are confined to this package. Callers deal only
// in Spec (a username) and Credential (an opaque handle).
package isolation

import (
	"context"
	"errors"
	"log/slog"
	"os/exec"
)

// ErrRefusedPrivileged is returned when a Spec names a privileged account.
// Honoring one would make isolation an ESCALATION mechanism rather than a
// containment one, so this refusal is not configurable.
var ErrRefusedPrivileged = errors.New("isolation: refusing to run as a privileged account")

// ErrNotCapable is returned by Capable when this worker cannot drop privileges.
var ErrNotCapable = errors.New("isolation: worker cannot assume another OS identity")

// Spec names the OS identity a process must run as.
type Spec struct {
	// User is the OS username. Required.
	User string
	// Group is the OS group; empty means the user's primary group.
	Group string
}

// Provider turns a Spec into a platform Credential.
type Provider interface {
	// Resolve produces a Credential for spec. It MUST return an error rather
	// than a nil Credential when the identity cannot be assumed: a caller that
	// silently proceeded would run job code as the daemon while appearing
	// isolated.
	Resolve(ctx context.Context, spec Spec) (*Credential, error)

	// Capable reports whether this worker can assume another identity at all —
	// root on POSIX, the relevant privilege on Windows. Checked once at boot.
	Capable() error
}

// CredentialStore supplies the secret for a local account, used by the Windows
// logon_user provider. Declared here rather than in provider_windows.go because
// Config references it and Config must compile on every platform.
//
// Secrets live on the worker — never server-side and never in AssignMsg, because
// the worker↔server broker has no authentication yet.
type CredentialStore interface {
	Secret(user string) (string, error)
}

// Config configures NewProvider.
type Config struct {
	// CredentialStore supplies the secret for the Windows logon_user provider.
	// Ignored on POSIX, where the worker must already run as root.
	CredentialStore CredentialStore

	// Provider selects the Windows credential mechanism: "logon_user" or
	// "s4u" (workerconfig.IsolationConfig.Provider). Ignored on POSIX, where
	// the worker must already run as root and there is only one mechanism.
	Provider string

	// Logger records which identity-resolution path produced a Credential —
	// notably when the POSIX provider falls back from pure-Go os/user to
	// shelling out to NSS-aware tools (id(1)/getent(1)). Nil is replaced with
	// a discard logger, matching internal/worker/lease and .../executor.
	Logger *slog.Logger
}

// NewProvider returns the platform Provider: POSIX credential switching via
// setuid/setgid/supplementary groups, or — until a later task lands the
// LogonUser-based implementation — a Windows provider that refuses every
// request rather than silently running unisolated.
func NewProvider(cfg Config) (Provider, error) {
	return newProvider(cfg)
}

// Apply attaches cred to cmd so the process starts under that identity.
// A nil cred is a no-op, which is the pre-isolation path.
//
// Implementations MUST preserve any SysProcAttr fields the caller already set —
// notably Setpgid, which the executor relies on to reap a whole process tree.
func Apply(cmd *exec.Cmd, cred *Credential) error {
	if cred == nil {
		return nil
	}
	return apply(cmd, cred)
}
