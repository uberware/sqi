// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback, useState } from 'react'

/** State and runner for a list page's per-row and bulk actions. */
export interface BulkAction<V extends string> {
  /**
   * Runs `action` for each id in turn. A failure does not stop the rest; once
   * all have settled, the verb's entry in `errors` reports how many failed and
   * the last error.
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

/**
 * Runs one action per id and keeps each verb's failures in its own state.
 *
 * Failures are not read from a TanStack mutation's `isError`, which the next
 * success resets: in a batch where item 2 of 5 fails and item 5 succeeds,
 * `isError` ends false and the failure is lost.
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

  const run = useCallback(
    async (verb: V, ids: readonly string[], action: (id: string) => Promise<unknown>) => {
      setErrors((prev) => {
        if (!prev.has(verb)) return prev
        const next = new Map(prev)
        next.delete(verb)
        return next
      })
      let failed = 0
      let lastMessage = 'Unknown error'
      for (const id of ids) {
        const key = busyKey(verb, id)
        setBusy((prev) => new Set(prev).add(key))
        try {
          await action(id)
        } catch (err) {
          failed++
          lastMessage = err instanceof Error ? err.message : 'Unknown error'
        } finally {
          setBusy((prev) => {
            const next = new Set(prev)
            next.delete(key)
            return next
          })
          onSettled?.(id)
        }
      }
      if (failed > 0) {
        const scope = ids.length === 1 ? '' : ` for ${failed} of ${ids.length} ${noun}`
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
