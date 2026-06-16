// SPDX-License-Identifier: AGPL-3.0-or-later

// Shared test helpers for theme-related mocks.
// Imported only by test files — safe to use vi here.
import { vi } from 'vitest'
import type { Mock } from 'vitest'

/** Shape of the in-memory localStorage mock returned by installLocalStorageMock. */
export type LocalStorageMock = {
  getItem: Mock<(key: string) => string | null>
  setItem: Mock<(key: string, value: string) => void>
  removeItem: Mock<(key: string) => void>
  clear: Mock<() => void>
  readonly length: number
  key: Mock<(index: number) => string | null>
}

/**
 * Create a fresh in-memory localStorage backed by a Map, install it as the
 * global via vi.stubGlobal, and return it so callers can inspect spy calls.
 *
 * Call once per beforeEach so each test starts with an empty store and clean
 * spy histories. vi.unstubAllGlobals() (in afterEach) tears it down.
 */
export function installLocalStorageMock(): LocalStorageMock {
  const store = new Map<string, string>()
  const mock: LocalStorageMock = {
    getItem: vi.fn((key: string): string | null => store.get(key) ?? null),
    setItem: vi.fn((key: string, value: string): void => {
      store.set(key, value)
    }),
    removeItem: vi.fn((key: string): void => {
      store.delete(key)
    }),
    clear: vi.fn((): void => {
      store.clear()
    }),
    get length() {
      return store.size
    },
    key: vi.fn((index: number): string | null => [...store.keys()][index] ?? null),
  }
  vi.stubGlobal('localStorage', mock)
  return mock
}

/**
 * Stub window.matchMedia to report whether the OS prefers dark mode.
 * Must be called before rendering any component that reads matchMedia.
 * vi.unstubAllGlobals() (in afterEach) tears it down.
 */
export function setMatchMedia(prefersDark: boolean): void {
  vi.stubGlobal(
    'matchMedia',
    vi.fn().mockImplementation((query: string) => ({
      matches: prefersDark,
      media: query,
      onchange: null,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      addListener: vi.fn(),
      removeListener: vi.fn(),
      dispatchEvent: vi.fn(),
    })),
  )
}

/**
 * Remove the data-theme attribute from <html> so each test starts with a
 * clean DOM state.
 */
export function resetThemeDom(): void {
  document.documentElement.removeAttribute('data-theme')
}
