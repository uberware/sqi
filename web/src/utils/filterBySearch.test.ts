// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect } from 'vitest'
import { filterBySearch } from './filterBySearch'

interface Item {
  name: string
  title: string
  description?: string
}

const items: Item[] = [
  { name: 'blender-render', title: 'Blender Render', description: 'Renders .blend scenes' },
  { name: 'ffmpeg', title: 'FFmpeg Transcode', description: 'Converts media files' },
  { name: 'bare', title: 'Bare' },
]

const pick = (i: Item) => [i.name, i.title, i.description]

describe('filterBySearch', () => {
  it('returns items unchanged for an empty or whitespace-only query', () => {
    expect(filterBySearch(items, '', pick)).toEqual(items)
    expect(filterBySearch(items, '   ', pick)).toEqual(items)
  })

  it('matches case-insensitively as a substring across any picked field', () => {
    expect(filterBySearch(items, 'BLEND', pick)).toEqual([items[0]])
    expect(filterBySearch(items, 'converts', pick)).toEqual([items[1]])
  })

  it('trims the query before matching', () => {
    expect(filterBySearch(items, '  ffmpeg  ', pick)).toEqual([items[1]])
  })

  it('ignores undefined fields and returns [] when nothing matches', () => {
    expect(filterBySearch(items, 'zzz', pick)).toEqual([])
  })
})
