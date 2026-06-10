# Web UI Build and Embedding

This document explains how the web UI bundle in `web/dist/` is produced, how
Go's `embed` directive bakes it into the `sqi-server` binary, how cache-busting
works in production, and what to do when the UI looks stale in a built binary.
For the development workflow, see [`web-development.md`](web-development.md).

---

## How `web/dist/` is generated

`web/dist/` is the production output of the Vite build:

```sh
cd web
npm run build      # tsc -b (type-check) then vite build
```

This writes:

```
web/dist/
├── index.html                     # SPA shell, references the hashed assets below
└── assets/
    ├── index-<hash>.js            # application bundle
    └── index-<hash>.css           # styles
```

`build.outDir` is `dist` (the Vite default, set explicitly in `vite.config.ts`).
The `<hash>` segments are content fingerprints — see
[cache-busting](#cache-busting-in-production) below.

You rarely run `npm run build` by hand. The `Makefile` does it for you:

- `make build-web` builds the bundle into `web/dist/`.
- `make build-server` and `make build` depend on `build-web`, so the embedded
  bundle is always rebuilt from current source before the Go build.

`npm ci` (the slow, clean reinstall) is gated on a stamp file
(`web/node_modules/.make-stamp`) keyed to `package.json` and `package-lock.json`,
so it only re-runs when dependencies actually change. The fast `vite build`
(~0.5 s) runs every time, keeping the guarantee that `web/dist/` matches current
source.

---

## How Go's `embed` directive picks it up

Package `web` (`web/embed.go`) embeds the directory at compile time:

```go
//go:embed dist
var distFS embed.FS

func Dist() (fs.FS, error) { /* returns an fs.FS rooted at dist/ */ }
```

Whatever files exist under `web/dist/` when `go build` runs are baked into the
binary. `internal/ui` consumes the `fs.FS` from `web.Dist()` and serves it over
HTTP:

- `GET /` → `index.html` (the SPA shell).
- `GET /assets/index-<hash>.js` (and any other embedded file) → that file, with
  its content type detected automatically.
- `GET /ui` or `GET /ui/...` with no matching embedded file → `index.html`, so
  client-side React Router routes resolve (SPA fallback).
- Any other unknown path → `404`.

Two embed details worth knowing:

- **The directive lives in `web/embed.go`, not `internal/ui`**, because Go's
  `//go:embed` can only reference files at or below the embedding file's own
  directory.
- **The plain (non-`all:`) embed form excludes dot/underscore-prefixed names.**
  This deliberately keeps OS sidecar junk (e.g. macOS `._index.html` resource
  forks) and editor dotfiles out of the binary. The Vite build emits only
  normally-named files, so nothing needed is dropped. If a future bundler emits
  `_`-prefixed paths, switch to `//go:embed all:dist` and strip sidecars first.

`web/dist/index.html` is tracked in git (everything else under `dist/` is
git-ignored) so `//go:embed dist` always has at least one file to embed and a
clean checkout builds without running the web build first. A real `npm run
build` overwrites that placeholder with the full app.

---

## Cache-busting in production

Vite fingerprints asset filenames with a content hash (`index-<hash>.js`). When
the bundle's content changes, the hash changes, so the URL changes — browsers
fetch the new file instead of serving a stale cached copy under the old name.

`internal/ui` sets cache headers to match this scheme:

| Response | `Cache-Control` | Why |
|---|---|---|
| `index.html` (the SPA shell) | `no-cache` | Must be revalidated every load so a freshly deployed binary's shell — which references the new hashed asset names — is picked up immediately. |
| Hashed assets (`/assets/*`) | `public, max-age=3600` | Content-addressed: a given URL's bytes never change, so it is safe to cache. |

The shell is always re-fetched and always points at the current asset hashes; the
heavy JS/CSS are cached by URL. A new build changes the hashes, the freshly
fetched `index.html` references them, and clients pull the new assets — no manual
cache invalidation required.

---

## Troubleshooting: the UI looks stale in a built binary

If a running `sqi-server` serves an old UI after you changed source:

1. **Confirm the bundle was rebuilt.** The embed is a compile-time snapshot of
   `web/dist/`. Rebuild it and the binary together:

   ```sh
   make build          # runs build-web (npm build) then the Go build
   ```

   A bare `go build ./cmd/sqi-server` embeds whatever is *already* in
   `web/dist/` — stale output if you skipped the web build.

2. **Force a clean web build** if `dist/` might be out of date:

   ```sh
   cd web && npm run build
   ```

   Note `make clean` intentionally does **not** wipe `web/dist/` (the tracked
   `index.html` must remain for the embed). Rebuild via `npm run build` rather
   than deleting the directory.

3. **Rule out a dependency-stamp miss.** If you changed `package.json` /
   `package-lock.json` but the stamp didn't trigger a reinstall, remove it:

   ```sh
   rm -f web/node_modules/.make-stamp && make build-web
   ```

4. **Rule out browser caching.** The shell is `no-cache`, but a proxy or an
   aggressive browser extension can still hold an old `index.html`. Hard-reload
   (DevTools open, "Disable cache") to confirm whether the stale UI is in the
   binary or the browser.

5. **Check for embed exclusions.** If an expected asset 404s, verify its name is
   not dot/underscore-prefixed (those are excluded by the embed form, see above).

A quick way to confirm what a binary actually embeds:

```sh
./bin/sqi-server serve &
curl -s http://localhost:8080/ | grep -o 'assets/index-[^"]*'   # the hashed names it references
```
