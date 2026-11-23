// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package main

import "os"

// defaultSessionRoot returns the session working-directory root for a root
// worker, and the mode to create it at.
//
// /var/lib is 0755 on Linux and macOS installations, so every ancestor of
// this path is traversable by any uid and nothing needs to be created or
// widened for run-as-user isolation.
//
// It is a sibling of workerconfig.defaultDataDir's HOME-unset fallback
// (/var/lib/sqi-worker), never a descendant of it: LoadOrCreateWorkerID
// creates that directory at 0700, and isolation.ValidateTraversable walks up
// from sessionRoot through every existing ancestor requiring the "other"
// execute bit. Nested under /var/lib/sqi-worker, a capable,
// isolation.required worker would refuse to start over a directory sqi
// itself created at boot.
func defaultSessionRoot() (path string, mode os.FileMode) {
	return "/var/lib/sqi-worker-sessions", 0o711
}
