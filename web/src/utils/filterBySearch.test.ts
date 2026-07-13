// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect } from 'vitest'
import { filterBySearch, matchesSearch, searchTerms } from './filterBySearch'

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

  it('ANDs whitespace-separated terms, order-independently, across fields', () => {
    // Each word is an independent substring term; all must match somewhere.
    expect(filterBySearch(items, 'blend render', pick)).toEqual([items[0]])
    expect(filterBySearch(items, 'render blend', pick)).toEqual([items[0]]) // order-independent
    // "transcode" is only in a title, "media" only in a description — both must hit.
    expect(filterBySearch(items, 'transcode media', pick)).toEqual([items[1]])
    // A term with no match anywhere excludes the item.
    expect(filterBySearch(items, 'blend zzz', pick)).toEqual([])
  })
})

describe('matchesSearch / searchTerms', () => {
  it('splits on whitespace into lower-cased terms', () => {
    expect(searchTerms('  Foo   BAR ')).toEqual(['foo', 'bar'])
    expect(searchTerms('   ')).toEqual([])
  })

  it('matches when every term is a case-insensitive substring (the "foo bar" rule)', () => {
    expect(matchesSearch('fool-grabbing-rebar', 'foo bar')).toBe(true)
    expect(matchesSearch('barfoo', 'foo bar')).toBe(true)
    expect(matchesSearch('unrelated', 'foo bar')).toBe(false)
    expect(matchesSearch('anything', '')).toBe(true) // empty query matches
  })
})
