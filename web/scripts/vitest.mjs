// SPDX-License-Identifier: AGPL-3.0-or-later

// Starts the Vitest CLI from the canonical path of web/. On Windows a working
// directory can carry a lowercase drive letter (c:\...), which Node keeps in
// the path it loads Vitest from. Vitest then starts its test workers from that
// lowercase path, while Vite resolves the `vitest` import in each test file to
// the uppercase form; Node caches modules by URL, so a test file gets a second
// copy of Vitest with no suite running and fails to load ("Vitest failed to
// find the current suite"). Loading Vitest from the canonical path keeps both
// on one URL. Elsewhere the canonical path is the same path and this changes
// nothing. Arguments are passed through to Vitest unchanged.

import { realpathSync } from 'node:fs'
import { createRequire } from 'node:module'
import { dirname, join } from 'node:path'
import { pathToFileURL } from 'node:url'

const root = realpathSync.native(join(import.meta.dirname, '..'))
process.chdir(root)

const require = createRequire(join(root, 'package.json'))
const vitestDir = dirname(require.resolve('vitest/package.json'))
await import(pathToFileURL(join(vitestDir, 'vitest.mjs')).href)
