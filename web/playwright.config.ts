// SPDX-License-Identifier: AGPL-3.0-or-later

import { defineConfig, devices } from '@playwright/test'

// Cross-browser E2E suite for the embedded production web UI. The stack (server
// + worker) is booted once in e2e/global-setup.ts and torn down in
// e2e/global-teardown.ts; specs read the live base URL + seeded IDs from the
// JSON state file those modules share.
//
// Run a single engine for fast iteration with PWPROJECT=chromium (or firefox /
// webkit); the default runs all three.

const allProjects = [
  { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
  { name: 'firefox', use: { ...devices['Desktop Firefox'] } },
  { name: 'webkit', use: { ...devices['Desktop Safari'] } },
]

const only = process.env.PWPROJECT
const projects = only ? allProjects.filter((p) => p.name === only) : allProjects

export default defineConfig({
  testDir: './e2e',
  // Ignore macOS AppleDouble sidecar files (._*) that appear on this volume.
  testIgnore: ['**/._*'],
  // Single shared server, so run serially to keep worker scheduling predictable.
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  timeout: 60_000,
  expect: { timeout: 15_000 },
  reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : 'list',
  globalSetup: './e2e/global-setup.ts',
  globalTeardown: './e2e/global-teardown.ts',
  use: {
    baseURL: process.env.SQI_BASE_URL,
    trace: 'on-first-retry',
    screenshot: 'only-on-failure',
    actionTimeout: 15_000,
    navigationTimeout: 30_000,
  },
  projects,
})
