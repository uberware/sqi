# Contributing to `sqi`

Thank you for your interest in contributing to `sqi`. We welcome contributions across multiple dimensions: code, preset definitions, documentation, bug reports, and design discussion.

---

## How to Contribute

### Code

We maintain a [roadmap and architecture guide](roadmap.md) organized by development phases. Code contributions are welcome at all stages.

**Setting up for development:**

```sh
# Clone the repository
git clone https://github.com/Uberware/sqi.git
cd sqi

# Install the git hooks (gofumpt, goimports, go vet, golangci-lint, and the
# Conventional Commits check). Requires gofumpt, goimports and golangci-lint
# on your PATH — see docs/development.md for install commands.
make hooks

# Go development (server and workers) — builds the web UI bundle first and
# writes both binaries into ./bin/
make build

# Python development (client SDK)
make py-install

# Web UI development (TypeScript + React)
cd web
npm ci
npm run dev
```

**Before starting work:**

- Check the [issues](https://github.com/Uberware/sqi/issues) and [discussions](https://github.com/Uberware/sqi/discussions) to avoid duplicating effort
- For substantial changes, open a discussion first to get feedback on the approach
- Read [ROADMAP.md](roadmap.md) to understand the architecture, deployment modes, and job model

**Code style and conventions:**

- **Go**: Follow [Effective Go](https://golang.org/doc/effective_go). Run `make fmt` (gofumpt + goimports) and `make lint` (golangci-lint) before submitting
- **Python**: Follow [PEP 8](https://www.python.org/dev/peps/pep-0008/). Use type hints for all function signatures. Format and lint with ruff: `ruff format && ruff check --fix` — this handles style consistency automatically
- **TypeScript/React**: Use ESLint and Prettier with the project configuration. Functional components and hooks preferred
- **SPDX header**: every source file (Go, TypeScript, YAML, Python, shell) must carry `SPDX-License-Identifier: AGPL-3.0-or-later` before any `package`/`import`/module declaration, in the file's own comment syntax. See [`docs/spdx-header.md`](spdx-header.md) for the exact template.
- **Commit messages**: [Conventional Commits](https://www.conventionalcommits.org) format — `type(scope)?: description` — is **enforced** by the `commit-msg` git hook installed by `make hooks`. Valid types: `feat fix docs style refactor test chore build ci perf revert`. Reference issue numbers in the description where relevant (e.g. `fix(scheduler): correct heartbeat timeout calculation (#42)`). `CHANGELOG.md` is generated from these messages by git-cliff, so a non-conforming commit never reaches the changelog.

**Testing:**

- Unit tests are required for code changes. The enforced coverage floor is 70% (`COVERAGE_MIN` in the Makefile); aim higher on new code
- Integration tests are encouraged for complex features. They live in `test/integration/`; those behind the `integration` build tag need `make test-integration`, because they depend on something the default suite should not require (a built binary, or an external service)
- Run `make test` (or `make ci`) before submitting a PR — do not run bare `go test ./...` from the repo root, since `web/node_modules/` contains third-party Go files the Makefile filters out
- **Changing LDAP code?** Run `make test-ldap`. It drives the login path against a real OpenLDAP server in a throwaway container, which is the only thing that catches a mistake in how sqi talks to a directory *on the wire*
- **Changing OIDC/SSO code?** Run `make test-oidc`. It drives the whole browser flow against a real Keycloak in a throwaway container, which is the only thing that catches what a real provider *omits* — most importantly a missing group claim, which validates fine and silently drops every user to `default_role`
- Both need Docker and **skip** without it — and a skip verifies nothing, so confirm the tests actually ran rather than trusting the exit code. Details in [`docs/development.md`](development.md#testing-against-a-real-directory-or-identity-provider)

**Submitting a PR:**

1. Make your changes with clear commits
2. Run tests and linting locally
3. Open a pull request with a clear description of what you're doing and why
4. Respond to review feedback promptly

(If you're new to open source, see [GitHub's guide to forking repositories](https://docs.github.com/en/get-started/quickstart/fork-a-repo).)


### Web UI contributions

The web UI (`web/`) is React + TypeScript + Vite, built into a bundle that is
embedded in the `sqi-server` binary. Start with [`web/README.md`](https://github.com/uberware/sqi/blob/main/web/README.md)
for the layout and [`docs/web-development.md`](web-development.md) for the
dev-server workflow (run `sqi-server`, optionally a worker, then `npm run dev`
with its API proxy).

Before opening a PR, run the same gate CI does, from `web/`:

```sh
npm run format:check && npm run typecheck && npm run lint && npm run test:coverage
```

**Storybook.** The project does not ship Storybook yet. Components are developed
and reviewed through their tests and the running app rather than an isolated
component explorer. If Storybook is added later, document its `npm run storybook`
entry point here.

**Component testing approach.** Tests use [Vitest](https://vitest.dev/) with
[React Testing Library](https://testing-library.com/) in a `jsdom` environment.
Conventions:

- Co-locate each test next to its subject as `Name.test.tsx` (or `.test.ts`).
- Test behavior through the rendered DOM the way a user experiences it — query
  by role, label, and text; drive interaction with `@testing-library/user-event`;
  assert with `@testing-library/jest-dom` matchers. Avoid asserting on internal
  state or implementation details.
- Mock at the network boundary (the `apiFetch`/query layer), not at the component
  internals, so tests exercise real component wiring.
- New components and hooks ship with tests; coverage is enforced against the
  threshold in `web/vite.config.ts`. See the existing `StatusBadge.test.tsx`,
  `Pagination.test.tsx`, and the `src/api` / `src/hooks` tests for the patterns.

**API client pattern.** All server access goes through `src/api/` — never call
`fetch` directly from a component.

- `src/api/client.ts` exposes `apiFetch<T>`, which sets JSON headers, prefixes
  `/api/v1`, and throws a typed `ApiError` (`{ status, title, detail, … }`) on
  non-2xx responses. Surface `ApiError.detail` to users.
- Read endpoints: add the wire type to `src/api/types.ts`, a raw fetch function
  and query key to `src/api/queries.ts`, then a `useQuery` hook. Write endpoints:
  add a `useMutation` hook in `src/api/mutations.ts` that invalidates the affected
  query keys in `onSuccess`.
- `src/api/types.ts` mirrors the REST wire format; keep it in sync with the
  OpenAPI spec at `GET /api/v1/openapi.yaml` (the spec is authoritative when they
  diverge). Live updates use the WebSocket layer in `src/ws/` via the
  `useWebSocket` hook. Exported functions and types in `src/api/` and `src/ws/`
  carry JSDoc — keep that up as you add to them.

**Styling conventions.**

- Use **CSS Modules** (`Component.module.css`) co-located with each component;
  no global class names or inline style objects for layout.
- Pull every color, spacing, radius, font, and shadow value from the design
  tokens in `src/styles/tokens.css`. Do not hard-code colors — new pairings must
  meet the contrast baseline in
  [`docs/web-accessibility.md`](web-accessibility.md).
- Functional components and hooks only. Import with the `@/` path alias, not
  relative `../../` chains.
- Honor the accessibility baseline ([`docs/web-accessibility.md`](web-accessibility.md)):
  semantic HTML, keyboard-operable controls, and text alternatives for any
  color-only or icon-only indicator.

### Community Preset Library

The community preset library is the easiest way to contribute and benefits the whole community. Presets are YAML or JSON files defining how to run a specific tool — no programming required.

**Preset structure:**

Presets define a product (product name, description, parameters, command mapping) for a DCC or tool. A simple example:

```yaml
name: Arnold CPU Render
description: Render a scene using Arnold (CPU)
version: 1.0
software_required: [ Arnold ]
parameters:
  scene_file:
    type: path
    required: true
    description: Arnold scene file (.ass)
  output_path:
    type: path
    required: true
    description: Output directory for EXR files
  samples:
    type: integer
    default: 100
    description: Camera samples per pixel
command:
  template: |
    kick -r {{ params.output_path }} \
         -samples {{ params.samples }} \
         "{{ params.scene_file }}"
```

**Contributing a preset:**

1. Test your preset locally — submit a job through `sqi` and verify output
2. Document the preset's assumptions (required plugins, environment setup, file paths)
3. Submit a PR to the [sqi-presets](https://github.com/Uberware/sqi-presets) repository with:
   - A descriptive name for the preset file
   - Detailed description in the YAML/JSON
   - An optional README explaining any setup or quirks
4. Include a rendered example (frame, output file) in the PR description if possible

Presets for major DCCs (Arnold, Blender, Houdini, Maya, Nuke, After Effects, Cinema 4D) are especially welcome. Custom tool presets are also valuable — if your studio has a proprietary pipeline tool, sharing the preset helps others.

### Documentation

Documentation contributions help the project significantly:

- **README improvements**: Clearer setup instructions, getting-started guides, use case examples
- **API documentation**: Docstrings in code, generated API reference, Python client examples
- **Architecture guides**: Explaining design decisions, system interaction diagrams, deployment patterns
- **Troubleshooting**: Common issues, diagnostics, logs interpretation

To contribute documentation:

1. Fork the repository
2. Edit `.md` files or code docstrings directly
3. Submit a PR describing what you clarified and why

### Bug Reports

Found an issue? Open a [GitHub issue](https://github.com/Uberware/sqi/issues) with:

- A clear title describing the problem
- Steps to reproduce (if applicable)
- Observed behavior vs. expected behavior
- Relevant logs or error messages
- Your environment (OS, Go version, deployment mode)

### Design Discussion

Have ideas for a feature, architectural improvement, or design change? Open a [GitHub discussion](https://github.com/Uberware/sqi/discussions). These are less formal than issues and are a good place to explore ideas before committing to implementation.

---

## Development Phases and Priorities

The [ROADMAP.md](roadmap.md) document outlines development phases. Current priorities:

- **Phase 1** (v0.1 — released): Core scheduler, pull-based workers, basic web UI, OpenJD execution
- **Phase 2** (v0.2 — released): Product system, preset library, DCC submitters
- **Phase 3** (v0.3 — released): Auth (local accounts, API keys, RBAC, LDAP/AD, OAuth2/OIDC SSO), multi-user role model, and run-as-user task isolation; v0.3 also ships the OpenJD `EXPR` extension and expanded ffmpeg and Mistika reference presets
- **Phase 4** (next): Production hardening, PostgreSQL, HA, auto-scaling

Code contributions aligned with the current phase are most likely to be accepted quickly. Contributions targeting later phases are welcome but may take longer to review if they require design discussion.

---

## Code of Conduct

We are committed to providing a welcoming and inclusive environment for all contributors.

- Be respectful of different perspectives and experience levels
- Provide constructive feedback
- Assume good intent
- Report violations to [robin@uberware.net](mailto:robin@uberware.net)

---

## Licensing

`sqi` is dual-licensed under:

1. **GNU Affero General Public License v3.0 (AGPL-3.0)** — Open source, self-hosted
2. **Commercial license** — For organizations requiring it

By contributing code, you agree that your contribution will be made available under both licenses. See [LICENSE](https://github.com/uberware/sqi/blob/main/LICENSE) for details.

If you have questions about licensing compatibility, open a discussion or contact [robin@uberware.net](mailto:robin@uberware.net).

---

## Getting Help

- **Questions about contributing?** Open a [discussion](https://github.com/Uberware/sqi/discussions)
- **Questions about the architecture?** Read [ROADMAP.md](roadmap.md)
- **Need a development environment tip?** Ask in discussions or open an issue
- **Direct contact**: [robin@uberware.net](mailto:robin@uberware.net)

---

## Thank You

We appreciate all contributions, large and small. Every preset shared, bug report filed, and piece of code submitted makes `sqi` better for everyone.

Happy coding.
