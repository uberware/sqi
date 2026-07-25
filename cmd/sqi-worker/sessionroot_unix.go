// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package main

import "os"

// defaultSessionRoot returns the session working-directory root for a root
// worker, and the mode to create it at.
//
// /var/lib is 0755 on every real Linux/macOS installation, so every ancestor
// of this path is already traversable by any uid — nothing needs to be
// created or widened specifically to make run-as-user isolation work.
//
// Deliberately a SIBLING of workerconfig.defaultDataDir's own HOME-unset
// fallback (/var/lib/sqi-worker), never a descendant of it:
// LoadOrCreateWorkerID creates that directory at 0700 by design, and
// isolation.ValidateTraversable walks up from sessionRoot through every
// existing ancestor requiring the "other" execute bit. Nesting this path
// under /var/lib/sqi-worker would make a capable, isolation.required worker
// refuse to start over a directory sqi itself had just created seconds
// earlier at boot.
func defaultSessionRoot() (path string, mode os.FileMode) {
	return "/var/lib/sqi-worker-sessions", 0o711
}
