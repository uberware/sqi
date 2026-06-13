// SPDX-License-Identifier: AGPL-3.0-or-later

// Shared state passed from the Playwright global setup to the spec files and
// the global teardown. Playwright's globalSetup cannot return values to tests,
// so we persist everything the spec needs (base URL, seeded farm/queue IDs) and
// everything teardown needs (PIDs, temp dir) to a JSON file under test-results/.

import { fileURLToPath } from 'node:url'
import { dirname, resolve } from 'node:path'
import { mkdirSync, readFileSync, writeFileSync, existsSync, rmSync } from 'node:fs'

const here = dirname(fileURLToPath(import.meta.url))

/** Absolute path to the JSON state file shared across setup → tests → teardown. */
export const STATE_FILE = resolve(here, '..', 'test-results', 'e2e-state.json')

export interface StackState {
  /** Base URL of the running sqi-server, e.g. http://127.0.0.1:53124. */
  baseURL: string
  /** Seeded farm ID. */
  farmId: string
  /** Seeded queue ID. */
  queueId: string
  /** Whether this run owns the stack (booted it) or attached to an external one. */
  owned: boolean
  /** Server process PID (only when owned). */
  serverPid?: number
  /** Worker process PID (only when owned). */
  workerPid?: number
  /** Temp working directory to remove on teardown (only when owned). */
  tmpDir?: string
}

export function writeState(state: StackState): void {
  mkdirSync(dirname(STATE_FILE), { recursive: true })
  writeFileSync(STATE_FILE, JSON.stringify(state, null, 2), 'utf-8')
}

export function readState(): StackState {
  const raw = readFileSync(STATE_FILE, 'utf-8')
  return JSON.parse(raw) as StackState
}

export function clearState(): void {
  if (existsSync(STATE_FILE)) {
    rmSync(STATE_FILE, { force: true })
  }
}
