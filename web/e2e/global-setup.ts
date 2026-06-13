// SPDX-License-Identifier: AGPL-3.0-or-later

// Playwright global setup for the sqi web E2E suite.
//
// Boots the REAL embedded production stack the same way scripts/smoke.sh does:
// a temp SQLite DB + temp embedded-NATS JetStream dir, ephemeral loopback ports,
// mDNS discovery disabled. It then seeds a farm + queue over REST, boots a
// worker bound to that farm/queue, waits for the worker to report online, and
// persists the base URL + seeded IDs to a JSON state file the specs read.
//
// If SQI_BASE_URL is already set (an externally-running server, e.g. on a VM),
// it attaches to that server and only seeds a farm/queue if none exist.

import { spawn } from 'node:child_process'
import { createServer } from 'node:net'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { dirname } from 'node:path'
import { writeState, type StackState } from './stack-state'

const here = dirname(fileURLToPath(import.meta.url))
const repoRoot = resolve(here, '..', '..')

// ── Small async helpers ─────────────────────────────────────────────────────

function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms))
}

/** Allocate a free ephemeral TCP port on the loopback interface. */
function freePort(): Promise<number> {
  return new Promise((resolvePort, reject) => {
    const srv = createServer()
    srv.unref()
    srv.on('error', reject)
    srv.listen(0, '127.0.0.1', () => {
      const addr = srv.address()
      if (addr === null || typeof addr === 'string') {
        srv.close()
        reject(new Error('could not determine ephemeral port'))
        return
      }
      const { port } = addr
      srv.close(() => resolvePort(port))
    })
  })
}

async function fetchJson(url: string, init?: RequestInit): Promise<unknown> {
  const res = await fetch(url, init)
  const text = await res.text()
  if (!res.ok) {
    throw new Error(`${init?.method ?? 'GET'} ${url} → ${res.status}: ${text}`)
  }
  return text ? JSON.parse(text) : null
}

// ── Seeding ─────────────────────────────────────────────────────────────────

interface SeedResult {
  farmId: string
  queueId: string
}

/** Ensure a farm and queue exist; reuse existing ones when present. */
async function ensureSeed(baseURL: string): Promise<SeedResult> {
  const farms = (await fetchJson(`${baseURL}/api/v1/farms`)) as Array<{ id: string }> | null
  let farmId = Array.isArray(farms) && farms[0] ? farms[0].id : ''
  if (!farmId) {
    const farm = (await fetchJson(`${baseURL}/api/v1/farms`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: 'e2e farm' }),
    })) as { id: string }
    farmId = farm.id
  }

  const queues = (await fetchJson(
    `${baseURL}/api/v1/queues?farm_id=${encodeURIComponent(farmId)}`,
  )) as { items?: Array<{ id: string }> } | null
  let queueId = queues?.items && queues.items[0] ? queues.items[0].id : ''
  if (!queueId) {
    const queue = (await fetchJson(`${baseURL}/api/v1/queues`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ farm_id: farmId, name: 'e2e queue' }),
    })) as { id: string }
    queueId = queue.id
  }

  return { farmId, queueId }
}

/** Poll GET /readyz until 200 or the deadline elapses. */
async function waitForReady(baseURL: string, timeoutMs: number): Promise<void> {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    try {
      const res = await fetch(`${baseURL}/readyz`)
      if (res.status === 200) return
    } catch {
      // server not up yet
    }
    await sleep(200)
  }
  throw new Error(`server not ready within ${timeoutMs}ms at ${baseURL}`)
}

/** Poll GET /api/v1/workers until at least one worker reports online. */
async function waitForOnlineWorker(baseURL: string, timeoutMs: number): Promise<string> {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    try {
      const data = (await fetchJson(`${baseURL}/api/v1/workers`)) as {
        items?: Array<{ id: string; status: string }>
      } | null
      const online = data?.items?.find((w) => w.status === 'online')
      if (online) return online.id
    } catch {
      // ignore transient errors
    }
    await sleep(300)
  }
  throw new Error(`no worker came online within ${timeoutMs}ms`)
}

// ── Main ────────────────────────────────────────────────────────────────────

export default async function globalSetup(): Promise<void> {
  // ── Attach to an externally-running server when SQI_BASE_URL is provided ──
  const external = process.env.SQI_BASE_URL
  if (external) {
    await waitForReady(external, 30_000)
    const { farmId, queueId } = await ensureSeed(external)
    // The external stack is expected to already have a worker online.
    await waitForOnlineWorker(external, 60_000)
    const state: StackState = { baseURL: external, farmId, queueId, owned: false }
    writeState(state)
    console.log(`[e2e] attached to external server ${external} (farm=${farmId} queue=${queueId})`)
    return
  }

  // ── Otherwise boot our own server + worker ───────────────────────────────
  const serverBin = process.env.SQI_SERVER_BIN ?? join(repoRoot, 'bin', 'sqi-server')
  const workerBin = process.env.SQI_WORKER_BIN ?? join(repoRoot, 'bin', 'sqi-worker')

  const tmpDir = mkdtempSync(join(tmpdir(), 'sqi-e2e-'))
  const httpPort = await freePort()
  const natsPort = await freePort()
  const metricsPort = await freePort()
  const httpAddr = `127.0.0.1:${httpPort}`
  const natsAddr = `127.0.0.1:${natsPort}`
  const baseURL = `http://${httpAddr}`
  console.log(`[e2e] booting sqi-server (http=${httpAddr}, nats=${natsAddr})`)

  const server = spawn(serverBin, ['serve'], {
    env: {
      ...process.env,
      SQI_HTTP_ADDR: httpAddr,
      SQI_NATS_ADDR: natsAddr,
      SQI_NATS_DATA_DIR: join(tmpDir, 'nats'),
      SQI_STORE_SQLITE_PATH: join(tmpDir, 'sqi.db'),
      SQI_DISCOVERY_ENABLED: 'false',
      SQI_SCHEDULER_TICK_INTERVAL: '100ms',
      SQI_LOG_LEVEL: 'warn',
    },
    stdio: 'inherit',
    detached: true,
  })
  server.unref()

  await waitForReady(baseURL, 30_000)
  const { farmId, queueId } = await ensureSeed(baseURL)
  console.log(`[e2e] booting sqi-worker (farm=${farmId} queue=${queueId})`)

  const worker = spawn(workerBin, ['start'], {
    env: {
      ...process.env,
      SQI_WORKER_NATS_URL: `nats://${natsAddr}`,
      SQI_WORKER_DISCOVERY_ENABLE_MDNS: 'false',
      SQI_WORKER_FARM_ID: farmId,
      SQI_WORKER_QUEUE_IDS: queueId,
      SQI_WORKER_DATA_DIR: join(tmpDir, 'worker-data'),
      SQI_WORKER_ALLOW_ROOT: 'true',
      SQI_WORKER_LOG_LEVEL: 'warn',
      SQI_WORKER_LOG_FORMAT: 'text',
      SQI_WORKER_HEARTBEAT_INTERVAL: '1s',
      SQI_WORKER_PULL_IDLE_BACKOFF: '300ms',
      SQI_WORKER_METRICS_ADDR: `127.0.0.1:${metricsPort}`,
    },
    stdio: 'inherit',
    detached: true,
  })
  worker.unref()

  await waitForOnlineWorker(baseURL, 60_000)

  const state: StackState = {
    baseURL,
    farmId,
    queueId,
    owned: true,
    ...(server.pid !== undefined ? { serverPid: server.pid } : {}),
    ...(worker.pid !== undefined ? { workerPid: worker.pid } : {}),
    tmpDir,
  }
  writeState(state)
  console.log(`[e2e] stack ready at ${baseURL}`)
}
