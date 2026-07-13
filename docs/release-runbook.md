# Release Runbook

This runbook captures the complete maintainer procedure for cutting a versioned
`sqi` release. Follow the steps in order. Each external action has a **Verify:**
line — do not proceed past a step until verification passes.

This document covers Steps 2–9 of the release plan. Step 1 (writing this file)
is already done. Step 10 (committing the runbook) closes the loop.

---

## Step 2: Apple signing prerequisites

Before the first release that includes macOS notarization, the maintainer must
provision Apple credentials and add them as GitHub secrets.

**Prerequisites:**
- An active Apple Developer Program membership (individual or organization).

**Procedure:**

1. In Xcode / Keychain Access / Apple Developer portal, create a
   **Developer ID Application** certificate. Export it as a `.p12` file and
   choose a strong password; record the password somewhere safe.

2. In [App Store Connect → Users and Access → Keys](https://appstoreconnect.apple.com/access/api),
   create an API key with the **App Manager** role. Download the `.p8` file
   (it can only be downloaded once). Note the **Issuer ID** and **Key ID**
   shown on that page.

3. Add the following GitHub repository secrets
   (`gh secret set` or Settings → Secrets and variables → Actions):

   ```bash
   # Base64-encode the .p12 and .p8 files — no line wrapping
   base64 -i cert.p12 | tr -d '\n' | gh secret set MACOS_CERT_P12_BASE64
   gh secret set MACOS_CERT_PASSWORD   # paste the .p12 export password
   base64 -i AuthKey_KEYID.p8 | tr -d '\n' | gh secret set APPLE_API_KEY_P8_BASE64
   gh secret set APPLE_API_ISSUER_ID   # paste the Issuer ID UUID
   gh secret set APPLE_API_KEY_ID      # paste the Key ID string
   ```

**Verify:** `gh secret list` shows all five secrets:
- `MACOS_CERT_P12_BASE64`
- `MACOS_CERT_PASSWORD`
- `APPLE_API_KEY_P8_BASE64`
- `APPLE_API_ISSUER_ID`
- `APPLE_API_KEY_ID`

---

## Step 3: PyPI trusted publisher

This step is required before `sqi-sdk` can be published to PyPI. It only
needs to be done once.

**Procedure:**

1. Log in to [pypi.org](https://pypi.org) as the `uberware` account.

2. If the `sqi-sdk` project does not yet exist on PyPI, create a
   **pending trusted publisher** (no upload needed) via
   Account → Publishing → "Add a new pending publisher":
   - PyPI project name: `sqi-sdk`
   - Owner: `uberware`
   - Repository name: `sqi`
   - Workflow filename: `release.yml`
   - Environment name: *(leave blank)*

   If the project already exists, navigate to the project's Publishing settings
   and add a trusted publisher there with the same values.

3. Set the repository variable that gates the PyPI publish step in the workflow:

   ```bash
   gh variable set PUBLISH_TO_PYPI --body true
   ```

**Verify:** `gh variable list` shows `PUBLISH_TO_PYPI=true`.

---

## Step 3b: sqi-submitter PyPI trusted publisher

Same procedure as Step 3, for the separate `sqi-submitter` PyPI project (the
DCC submitter framework package in `clients/submitter/`). This is required
before `sqi-submitter` can be published to PyPI, and it only needs to be done
once.

**Procedure:**

1. Log in to [pypi.org](https://pypi.org) as the `uberware` account.

2. If the `sqi-submitter` project does not yet exist on PyPI, claim the name
   with a one-time manual placeholder upload (a pending trusted publisher
   alone does not reserve the name against a `python -m build` from a local
   checkout — verified free 2026-07-05):

   ```bash
   cd clients/submitter
   python -m pip install --upgrade build twine
   python -m build
   twine upload dist/*   # prompts for a PyPI API token on first upload
   ```

3. Once the project exists (either from the placeholder upload above or
   because it already did), navigate to its Publishing settings and add a
   trusted publisher:
   - PyPI project name: `sqi-submitter`
   - Owner: `uberware`
   - Repository name: `sqi`
   - Workflow filename: `release.yml`
   - Environment name: *(leave blank)*

4. The `PUBLISH_TO_PYPI` repository variable set in Step 3 gates both the
   `sqi-sdk` and `sqi-submitter` publish steps in `release.yml` — no separate
   variable is needed.

**Verify:** the `sqi-submitter` project's Publishing page on PyPI lists the
trusted publisher above, and `gh variable list` still shows
`PUBLISH_TO_PYPI=true`.

---

## Step 4: Notarization dry-run via a throwaway tag

Run a full release pipeline using a throwaway `-rc` tag to confirm that
notarization is wired up correctly before tagging the real release.

The `release.yml` workflow skips the `sqi-sdk` version check for tags
containing `-rc` (via `if: ${{ !contains(github.ref_name, '-rc') }}`), so the
dry-run tag `v0.0.1-rc.1` does not require bumping `_version.py`.

**Procedure:**

1. Ensure the five Apple secrets from Step 2 are set.

2. Push the throwaway tag:

   ```bash
   git tag v0.0.1-rc.1 && git push origin v0.0.1-rc.1
   ```

3. Wait for the Release workflow run to complete on GitHub Actions.

**Verify:**
- The Release workflow run is green.
- The resulting GitHub pre-release (`v0.0.1-rc.1`) contains
  `sqi_Darwin_arm64.tar.gz` and `sqi_Darwin_x86_64.tar.gz` as release assets.
- The **Notarize macOS binaries** step logs `The status is Accepted` for each
  binary submitted to Apple Notary.
- Multi-arch image is present:
  ```bash
  docker buildx imagetools inspect ghcr.io/uberware/sqi/sqi-worker:v0.0.1-rc.1
  ```
  Expected: manifest list lists both `linux/amd64` and `linux/arm64`.

**Clean up:**

```bash
gh release delete v0.0.1-rc.1 --yes
git push --delete origin v0.0.1-rc.1
git tag -d v0.0.1-rc.1
```

Optionally, delete the `:v0.0.1-rc.1` and `:v0.0.1-rc.1-*` GHCR image tags
via the GitHub Packages UI or `gh` API.

---

## Step 5: Local pre-tag sanity check

Run this on the machine used to cut the release (no Apple credentials needed).

```bash
goreleaser check
GOVERSION=$(go version | awk '{print $3}') \
  goreleaser release --snapshot --clean --skip=sign,sbom,publish 2>&1 | tail -25
```

**Expected:** the snapshot build completes successfully. Both `sqi-server` and
`sqi-worker` are produced for all target platforms (`linux/amd64`, `linux/arm64`,
`darwin/amd64`, `darwin/arm64`, `windows/amd64`). All four Docker image variants
build (`-amd64`/`-arm64` per image, plus the manifest tags).

---

## Step 6: Cut the real release

Set the target version once and reuse it throughout this section (examples below
use `0.2.0`):

```bash
VERSION=0.2.0
```

**Procedure:**

1. Confirm `clients/python/src/sqi_client/_version.py` reads `$VERSION`:
   ```bash
   grep __version__ clients/python/src/sqi_client/_version.py
   ```

   Also bump and confirm `clients/submitter/src/sqi_submitter/_version.py` —
   it does **not** track the same version as `sqi-sdk` automatically (it ships
   a `.dev0` placeholder until deliberately bumped) and `release.yml`'s
   "Check sqi-submitter version matches the tag" step fails the release
   outright if it does not match the tag:
   ```bash
   grep __version__ clients/submitter/src/sqi_submitter/_version.py
   ```

2. Confirm the `main` branch is green in CI and all release-blocking tasks are
   merged.

3. Regenerate and commit the changelog so the tag captures a dated section.
   `CHANGELOG.md` is generated from the Conventional Commit history by
   git-cliff — do it as the last commit before tagging so the section date
   matches the tag date. (The release workflow also regenerates it for the
   archives, but that copy is not committed back, so the tracked file must be
   refreshed here.)
   ```bash
   make changelog VERSION=$VERSION      # requires git-cliff; rolls pending commits into a dated $VERSION section
   git add CHANGELOG.md
   git commit -m "chore(release): update changelog for $VERSION"
   git push origin main
   ```
   Review the diff first: the new `## [$VERSION]` section should list the
   commits since the previous tag, grouped by type, with no stray groups.

4. Tag and push:
   ```bash
   git tag "v$VERSION" && git push origin "v$VERSION"
   ```

5. Wait for the Release workflow to complete.

**Verify:** The GitHub release `v$VERSION` contains:
- Archives: `sqi_Linux_x86_64.tar.gz`, `sqi_Linux_arm64.tar.gz`,
  `sqi_Darwin_x86_64.tar.gz`, `sqi_Darwin_arm64.tar.gz`,
  `sqi_Windows_x86_64.zip`, `sqi_Windows_arm64.zip`
- `checksums.txt`, SBOMs (`.sbom`), cosign signatures (`.sig` / `.pem`)
- `sqi_sdk-$VERSION-py3-none-any.whl` and `sqi_sdk-$VERSION.tar.gz`
- `sqi_submitter-$VERSION-py3-none-any.whl` and `sqi_submitter-$VERSION.tar.gz`

---

## Step 6b: Publish reference and testing presets to the preset library

The presets authored in this repo — the six `presets/sqi/*.yaml` reference
presets (Maya layer + scene, Nuke write + script, Houdini ROP, Blender batch)
and the four `presets/testing/*.yaml` smoke-test presets (test-render and
test-steps, each in bash + PowerShell) — are not distributed as part of the
GitHub release or PyPI packages; they are published to the separate
`uberware/sqi-presets` repository that backs the default `preset_library.url`
(see `docs/preset-library.md`). This is **automated** by the `publish-presets`
job in `.github/workflows/release.yml`: after a successful release, on a
non-`-rc` tag, it runs `cmd/presetgen` twice against a checkout of the library
repo — once for `presets/sqi` (the `sqi/` namespace) and once for
`presets/testing` under the `testing/` subdir — to regenerate `index.json` +
the definition files, and pushes only if something changed. `presetgen`
merges — each pass owns its own namespace and leaves any other library entries
untouched — and computes each `sha256` over the raw file bytes, so the
published index always matches what `sqi-server` verifies on install.

**One-time setup** (until done, the job is skipped and the release is
unaffected):

1. Create the `uberware/sqi-presets` repository and enable GitHub Pages serving
   from the default branch root, so `https://uberware.github.io/sqi-presets/index.json`
   resolves. It may start empty — the first release populates it.

2. Create a fine-grained personal access token with **Contents: write** scoped
   to `uberware/sqi-presets`, and add it as a secret on this repo:
   ```bash
   gh secret set SQI_PRESETS_TOKEN   # paste the fine-grained PAT
   ```

3. Enable the publish step:
   ```bash
   gh variable set PUBLISH_PRESETS --body true
   ```
   **Verify:** `gh variable list` shows `PUBLISH_PRESETS=true` and
   `gh secret list` shows `SQI_PRESETS_TOKEN`.

**Manual fallback** (out-of-band publish without a release): from a repo
checkout, against a checkout of the library repo, run and push:
```bash
go run ./cmd/presetgen -presets presets/sqi -out /path/to/sqi-presets
go run ./cmd/presetgen -presets presets/testing -subdir testing -out /path/to/sqi-presets
( cd /path/to/sqi-presets && git add -A && git commit -m "publish presets" && git push )
```

**Verify:**
- The published index (`https://uberware.github.io/sqi-presets/index.json`)
  lists all six presets under `sqi/` and the four test presets under `testing/`
  with the correct `sha256`.
- In the sqi web UI's Preset Library page, each preset installs successfully
  and its checksum verifies (no 422).

---

## Step 7: Make GHCR packages public

By default, packages pushed to GHCR inherit the repository's visibility
(private). Make both packages public so users can pull without authenticating.

**Procedure:**

1. Go to https://github.com/orgs/uberware/packages (or the user's Packages page).
2. For `sqi-server`: Package Settings → Change visibility → **Public**.
3. For `sqi-worker`: Package Settings → Change visibility → **Public**.

**Verify (unauthenticated pull):**

```bash
docker logout ghcr.io
docker buildx imagetools inspect ghcr.io/uberware/sqi/sqi-server:v$VERSION
docker buildx imagetools inspect ghcr.io/uberware/sqi/sqi-worker:v$VERSION
```

Expected: each command succeeds and prints a manifest list with both
`linux/amd64` and `linux/arm64` entries.

---

## Step 8: PyPI verification

```bash
python -m pip install --no-cache-dir "sqi-sdk==$VERSION"
python -c "import sqi_client; print(sqi_client.__version__)"
```

Expected output: `$VERSION`.

Also verify `sqi-submitter` (once its version has been bumped past the
`.dev0` placeholder per Step 6):

```bash
python -m pip install --no-cache-dir "sqi-submitter==<version>"
python -c "import sqi_submitter; print(sqi_submitter.__version__)"
```

Expected output: `<version>` matching the tag.

---

## Step 9: End-to-end release verification

### Docker Compose path

```bash
curl -LO https://raw.githubusercontent.com/uberware/sqi/v$VERSION/deploy/docker-compose.yml
docker compose -f docker-compose.yml up -d
curl -sf localhost:8080/readyz && echo server-ready
```

Create a farm and queue, then submit the sample job from the repository:

```bash
curl -LO https://raw.githubusercontent.com/uberware/sqi/v$VERSION/docs/examples/hello.json

FARM=$(curl -s -X POST localhost:8080/api/v1/farms \
  -H 'content-type: application/json' -d '{"name":"demo"}' | jq -r .id)

QUEUE=$(curl -s -X POST localhost:8080/api/v1/queues \
  -H 'content-type: application/json' \
  -d "{\"farm_id\":\"$FARM\",\"name\":\"default\"}" | jq -r .id)

JOB=$(curl -s -X POST \
  "localhost:8080/api/v1/jobs?farm_id=$FARM&queue_id=$QUEUE&owner=runbook" \
  -H 'content-type: application/json' \
  --data-binary @hello.json | jq -r .id)

curl -s localhost:8080/api/v1/jobs/$JOB/tasks
```

Expected: the job's tasks reach `succeeded` status. Logs in the web UI
(http://localhost:8080) show `Hello from sqi v$VERSION`.

Clean up:

```bash
docker compose -f docker-compose.yml down -v
```

### Binary path — macOS

1. Download and extract the Darwin archive matching your machine's arch:
   ```bash
   # arm64 (Apple Silicon)
   curl -LO https://github.com/uberware/sqi/releases/download/v$VERSION/sqi_Darwin_arm64.tar.gz
   tar -xzf sqi_Darwin_arm64.tar.gz
   ```

2. Launch both binaries:
   ```bash
   ./sqi-server serve &
   ./sqi-worker start &
   ```

3. Check Gatekeeper acceptance (no quarantine dialog, no "cannot be opened" error):
   ```bash
   spctl -a -vvv -t exec ./sqi-server
   spctl -a -vvv -t exec ./sqi-worker
   ```
   Expected: `source=Notarized Developer ID` (or `accepted`) for both binaries.

4. Stop both processes when done.

### Binary path — Linux arm64

On an arm64 Linux host:

```bash
curl -LO https://github.com/uberware/sqi/releases/download/v$VERSION/sqi_Linux_arm64.tar.gz
tar -xzf sqi_Linux_arm64.tar.gz
./sqi-server serve &
./sqi-worker start &
```

**Verify:** the worker appears as registered in the web UI or via
`curl -s localhost:8080/api/v1/workers`.
