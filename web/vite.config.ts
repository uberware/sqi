import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import { fileURLToPath } from 'url'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: 'dist',
  },
  resolve: {
    alias: {
      // @/ maps to src/ — mirrors the paths setting in tsconfig.app.json.
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  server: {
    proxy: {
      // Forward all /api requests (REST + WebSocket) to a local sqi-server.
      // ws: true enables WebSocket upgrade proxying, covering /api/v1/ws.
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
        ws: true,
      },
    },
  },
  test: {
    // Only match test files under src/; explicitly exclude macOS sidecar (._*) files.
    include: ['src/**/*.{test,spec}.{ts,tsx}'],
    exclude: ['**/._*', 'node_modules', 'dist'],
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
    coverage: {
      provider: 'v8',
      reporter: ['text', 'json', 'html'],
      reportsDirectory: './coverage',
      // All source files must appear in the report, even those with no tests.
      include: ['src/**/*.{ts,tsx}'],
      // Exclude entry point, test files themselves (they inflate coverage),
      // type-declaration files, test infrastructure, and macOS sidecar files.
      exclude: [
        'src/main.tsx',
        'src/api/types.ts',
        'src/ws/types.ts',
        'src/test/**',
        '**/*.{test,spec}.{ts,tsx}',
        '**/*.d.ts',
        '**/._*',
      ],
      thresholds: {
        lines: 60,
        functions: 60,
        branches: 60,
        statements: 60,
      },
    },
  },
})
