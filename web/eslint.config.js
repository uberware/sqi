import js from '@eslint/js'
import globals from 'globals'
import reactHooks from 'eslint-plugin-react-hooks'
import reactRefresh from 'eslint-plugin-react-refresh'
import tseslint from 'typescript-eslint'
import vitest from '@vitest/eslint-plugin'
import prettier from 'eslint-config-prettier'
import { defineConfig, globalIgnores } from 'eslint/config'

export default defineConfig([
  globalIgnores(['dist', 'coverage', 'test-results', 'playwright-report', '**/._*']),

  // Source files: strict TypeScript + React rules, browser globals.
  {
    files: ['src/**/*.{ts,tsx}'],
    extends: [
      js.configs.recommended,
      tseslint.configs.strict,
      reactHooks.configs.flat.recommended,
      reactRefresh.configs.vite,
    ],
    languageOptions: {
      globals: globals.browser,
    },
  },

  // Test files: spread the full Vitest recommended config (plugins + globals +
  // rules) on top of the source config above.  Using just .rules would drop the
  // vitest globals declaration and silently mis-lint tests that use global vi/it.
  {
    files: ['src/**/*.{test,spec}.{ts,tsx}'],
    ...vitest.configs.recommended,
  },

  // Config files (vite.config.ts, etc.): recommended rules only, Node globals.
  {
    files: ['*.config.{ts,js,mts,mjs}'],
    extends: [js.configs.recommended, tseslint.configs.recommended],
    languageOptions: {
      globals: globals.node,
    },
  },

  // Playwright E2E suite (lives outside src/ so Vitest never picks it up):
  // recommended TypeScript rules with Node globals. Runs in Node, not the
  // browser, so console output during stack boot/teardown is expected.
  {
    files: ['e2e/**/*.ts'],
    extends: [js.configs.recommended, tseslint.configs.recommended],
    languageOptions: {
      globals: globals.node,
    },
  },

  // Prettier must come last to disable any ESLint formatting rules that would
  // conflict with Prettier's output.
  prettier,
])
