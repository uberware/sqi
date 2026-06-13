// SPDX-License-Identifier: AGPL-3.0-or-later

// Primary end-to-end scenario for the embedded production web UI, exercised
// against a real sqi-server + sqi-worker booted by global-setup.ts:
//
//   Dashboard shows the seeded worker online
//     → Submit an OpenJD echo job through the UI
//     → Redirect to the job detail page
//     → Status badge transitions to "Completed" (WebSocket/live driven)
//     → Job list also reflects the completed status
//     → Log viewer shows the echoed output
//
// The embedded server serves the SPA shell only at "/" (and under /ui/*), so
// deep-link page loads to client routes like /submit would 404. We therefore
// drive the app the way a real user does: load "/" once, then navigate purely
// via in-app links (client-side routing) — never a full page load to a sub-route.
//
// The stack's live base URL and seeded farm/queue IDs come from the JSON state
// file written by global setup (read lazily, since Playwright imports spec files
// in the main process during test discovery, before global setup runs).

import { test, expect, type Page } from '@playwright/test'
import { readState, type StackState } from './stack-state'

// The "Load example → Single-step shell command" template echoes this string.
const SENTINEL = 'Hello, world!'

let state: StackState

test.beforeAll(() => {
  state = readState()
})

async function openApp(page: Page): Promise<void> {
  await page.goto(state.baseURL + '/')
  await expect(page.getByRole('navigation', { name: /primary navigation/i })).toBeVisible()
}

test.describe('job lifecycle', () => {
  test('dashboard shows the seeded worker online', async ({ page }) => {
    await openApp(page)
    await expect(page.getByRole('heading', { name: 'Dashboard' })).toBeVisible()
    // The Workers card exposes an "N online workers" listitem; assert N >= 1.
    await expect(page.getByRole('listitem', { name: /[1-9]\d* online workers/ })).toBeVisible({
      timeout: 20_000,
    })
  })

  test('submit an echo job and watch it run to completion', async ({ page }) => {
    await openApp(page)

    // ── Navigate to the Submit page via the sidebar (client-side routing) ─────
    await page.getByRole('link', { name: 'Submit', exact: true }).click()
    await expect(page.getByRole('heading', { name: 'Submit Job' })).toBeVisible()

    const queueSelect = page.getByRole('combobox', { name: /queue/i })
    await expect(queueSelect).toBeEnabled()
    // Target the seeded queue explicitly (defaults to the first queue anyway).
    await queueSelect.selectOption(state.queueId)

    // Populate the OpenJD template via the "Load example" dropdown. This sets the
    // controlled React state used for submission directly, which is far more
    // robust across chromium/firefox/webkit than synthesising keystrokes into
    // CodeMirror's contenteditable surface.
    await page.getByRole('combobox', { name: /load example/i }).selectOption('shell')

    // Confirm the CodeMirror editor reflects the loaded template. The editor is
    // a contenteditable `.cm-content` inside the testid-tagged wrapper.
    const editor = page.getByTestId('template-editor').locator('.cm-content')
    await expect(editor).toContainText('hello-world')

    // ── Submit ───────────────────────────────────────────────────────────────
    const submit = page.getByRole('button', { name: /submit job/i })
    await expect(submit).toBeEnabled()
    await submit.click()

    // Client-side redirect to the job detail page: /jobs/<id>.
    await page.waitForURL(/\/jobs\/[^/]+$/, { timeout: 20_000 })
    const jobId = new URL(page.url()).pathname.split('/').pop() ?? ''
    expect(jobId).not.toEqual('')

    // ── Live status transition on the detail page ─────────────────────────────
    // The job-level status badge (aria-label "Status: <label>") flips to
    // "Completed" once the worker finishes — driven by WebSocket events plus a
    // background refetch. Poll the UI with a web-first assertion, no fixed sleep.
    await expect(page.getByLabel('Status: Completed').first()).toBeVisible({
      timeout: 60_000,
    })

    // ── Job list reflects the completed status (sidebar navigation) ───────────
    await page.getByRole('link', { name: 'Jobs', exact: true }).click()
    await expect(page.getByRole('heading', { name: 'Jobs' })).toBeVisible()
    const jobRow = page.getByRole('row').filter({ hasText: 'hello-world' }).first()
    await expect(jobRow).toBeVisible({ timeout: 20_000 })
    await expect(jobRow.getByLabel('Status: Completed')).toBeVisible({ timeout: 30_000 })

    // ── Log viewer shows the echoed sentinel ──────────────────────────────────
    await jobRow.getByRole('link', { name: 'hello-world' }).click()
    await page.waitForURL(/\/jobs\/[^/]+$/, { timeout: 20_000 })
    await page
      .getByRole('link', { name: /view logs for task/i })
      .first()
      .click()
    await page.waitForURL(/\/tasks\/[^/]+\/logs$/, { timeout: 20_000 })

    const logPanel = page.getByRole('log', { name: /task log output/i })
    await expect(logPanel).toContainText(SENTINEL, { timeout: 30_000 })
  })
})
