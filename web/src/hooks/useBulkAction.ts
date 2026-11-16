// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback, useRef, useState } from 'react'

/** State and runner for a list page's per-row and bulk actions. */
export interface BulkAction<V extends string> {
  /**
   * Runs `action` for each id in turn. A failure does not stop the rest; once
   * all have settled, the verb's entry in `errors` reports how many failed and
   * the last error. Runs of one verb that overlap (a per-row click during a
   * bulk run) are counted together, so neither run's failures are lost.
   */
  run: (verb: V, ids: readonly string[], action: (id: string) => Promise<unknown>) => Promise<void>
  /** Failure message per verb, in the order the failures were recorded. */
  errors: ReadonlyMap<V, string>
  /** True while `verb`'s action is in flight for `id`. */
  isBusy: (verb: V, id: string) => boolean
  /** True while `verb`'s action is in flight for any id. */
  isRunning: (verb: V) => boolean
}

const busyKey = (verb: string, id: string) => `${verb}\u0000${id}`

/** Running count for one verb, kept while any run of that verb is in flight. */
interface Tally {
  runs: number
  total: number
  failed: number
  lastMessage: string
}

/**
 * Runs one action per id and keeps each verb's failures in its own state.
 *
 * Failures are counted here rather than read from a TanStack mutation's
 * `isError`, which the next success resets, so a failure early in a batch
 * would not be reported.
 *
 * @param noun plural name of the items, for messages ("jobs", "workers").
 * @param onSettled called for each id once its action finishes, either way.
 */
export function useBulkAction<V extends string>(
  noun: string,
  onSettled?: (id: string) => void,
): BulkAction<V> {
  const [errors, setErrors] = useState<ReadonlyMap<V, string>>(new Map())
  const [busy, setBusy] = useState<ReadonlySet<string>>(new Set())
  const tallies = useRef(new Map<V, Tally>())

  const run = useCallback(
    async (verb: V, ids: readonly string[], action: (id: string) => Promise<unknown>) => {
      let tally = tallies.current.get(verb)
      if (tally === undefined) {
        // No run of this verb is in flight: start a fresh count.
        tally = { runs: 0, total: 0, failed: 0, lastMessage: 'Unknown error' }
        tallies.current.set(verb, tally)
        setErrors((prev) => {
          if (!prev.has(verb)) return prev
          const next = new Map(prev)
          next.delete(verb)
          return next
        })
      }
      tally.runs++
      tally.total += ids.length
      for (const id of ids) {
        const key = busyKey(verb, id)
        setBusy((prev) => new Set(prev).add(key))
        try {
          await action(id)
        } catch (err) {
          tally.failed++
          tally.lastMessage = err instanceof Error ? err.message : 'Unknown error'
        } finally {
          setBusy((prev) => {
            const next = new Set(prev)
            next.delete(key)
            return next
          })
          onSettled?.(id)
        }
      }
      tally.runs--
      if (tally.runs === 0) tallies.current.delete(verb)
      if (tally.failed > 0) {
        const { total, failed, lastMessage } = tally
        const scope = total === 1 ? '' : ` for ${failed} of ${total} ${noun}`
        setErrors((prev) => new Map(prev).set(verb, `${verb} failed${scope}: ${lastMessage}`))
      }
    },
    [noun, onSettled],
  )

  const isBusy = (verb: V, id: string) => busy.has(busyKey(verb, id))
  const isRunning = (verb: V) => {
    const prefix = busyKey(verb, '')
    for (const key of busy) if (key.startsWith(prefix)) return true
    return false
  }

  return { run, errors, isBusy, isRunning }
}
