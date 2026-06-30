// SPDX-License-Identifier: AGPL-3.0-or-later
import { describe, expect, it } from 'vitest'
import { parsePathDeliveries, serializePathDeliveries } from '@/lib/pathDelivery'

const yamlWith = `specificationVersion: jobtemplate-2023-09
name: T
extensions: [ SQI_PATH_TRANSLATION ]
SQI_PATH_TRANSLATION:
  deliveries:
    - swap_in_place
    - command_flags: { pattern: "--remap {src}={dest}" }
steps:
  - name: S
`

describe('parsePathDeliveries', () => {
  it('parses bare and keyed deliveries', () => {
    const pt = parsePathDeliveries(yamlWith, 'yaml')
    expect(pt).not.toBeNull()
    expect(pt?.deliveries).toHaveLength(2)
    expect(pt?.deliveries[1]).toEqual({ kind: 'command_flags', pattern: '--remap {src}={dest}' })
  })

  it('returns null when the extension is absent', () => {
    const pt = parsePathDeliveries('specificationVersion: x\nname: T\nsteps: []\n', 'yaml')
    expect(pt).toBeNull()
  })
})

describe('serializePathDeliveries', () => {
  it('round-trips through parse', () => {
    const out = serializePathDeliveries('specificationVersion: x\nname: T\nsteps: []\n', 'yaml', {
      deliveries: [{ kind: 'environment', variable: 'PROJECT_ROOT' }],
    })
    const pt = parsePathDeliveries(out, 'yaml')
    expect(pt?.deliveries[0]).toEqual({ kind: 'environment', variable: 'PROJECT_ROOT' })
    expect(out).toContain('SQI_PATH_TRANSLATION')
    expect(out).toContain('extensions')
  })
})
