// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package main

import (
	"os"
	"path/filepath"
)

// defaultSessionRoot returns the session working-directory root on Windows.
//
// Not under the worker data directory: a worker running as LocalSystem (which
// isolation requires) resolves its data directory to
// C:\Windows\System32\config\systemprofile\.sqi\worker, which is the wrong
// place for gigabytes of render scratch and is undiscoverable to an operator.
//
// The returned mode is inert — Go ignores permission bits in MkdirAll on
// Windows — and is present only so this function matches the POSIX signature.
// The root's real protection is the protected DACL applied to each session
// directory beneath it (isolation.SecureWorkDir); the root itself needs no
// per-account grant, because Windows grants "Bypass traverse checking" to
// Everyone by default, so a task reaches its own session directory without
// any right on the ancestors.
func defaultSessionRoot() (path string, mode os.FileMode) {
	base := os.Getenv("ProgramData")
	if base == "" {
		base = `C:\ProgramData`
	}
	return filepath.Join(base, "sqi", "worker", "sessions"), 0o711
}
