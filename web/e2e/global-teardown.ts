// SPDX-License-Identifier: AGPL-3.0-or-later

// Playwright global teardown: stop the server + worker we booted (if any) and
// remove the temp working directory. No-op when attached to an external server.

import { rmSync } from 'node:fs'
import { readState, clearState } from './stack-state'

/** Kill a detached process group (negative PID), falling back to the bare PID. */
function killProcessTree(pid: number): void {
  try {
    process.kill(-pid, 'SIGTERM')
  } catch {
    try {
      process.kill(pid, 'SIGTERM')
    } catch {
      // already gone
    }
  }
}

export default function globalTeardown(): void {
  let state
  try {
    state = readState()
  } catch {
    return // nothing was persisted; nothing to clean up
  }

  if (state.owned) {
    if (state.workerPid !== undefined) killProcessTree(state.workerPid)
    if (state.serverPid !== undefined) killProcessTree(state.serverPid)
    if (state.tmpDir) {
      try {
        rmSync(state.tmpDir, { recursive: true, force: true })
      } catch {
        // best effort
      }
    }
  }

  clearState()
}
