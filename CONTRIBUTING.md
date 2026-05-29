# Contributing to `sqi`

Thank you for your interest in contributing to `sqi`. We welcome contributions across multiple dimensions: code, preset definitions, documentation, bug reports, and design discussion.

---

## How to Contribute

### Code

We maintain a [roadmap and architecture guide](ROADMAP.md) organized by development phases. Code contributions are welcome at all stages.

**Setting up for development:**

```sh
# Clone the repository
git clone https://github.com/Uberware/sqi.git
cd sqi

# Go development (server and workers)
cd cmd/sqi-server
go mod download
go build

# Python development (client API and DCC submitters)
cd python-client
pip install -e .

# Web UI development (TypeScript + React/Svelte)
cd web
npm install
npm run dev
```

**Before starting work:**

- Check the [issues](https://github.com/Uberware/sqi/issues) and [discussions](https://github.com/Uberware/sqi/discussions) to avoid duplicating effort
- For substantial changes, open a discussion first to get feedback on the approach
- Read [ROADMAP.md](ROADMAP.md) to understand the architecture, deployment modes, and job model

**Code style and conventions:**

- **Go**: Follow [Effective Go](https://golang.org/doc/effective_go). Use `gofmt` and `golangci-lint` before submitting
- **Python**: Follow [PEP 8](https://www.python.org/dev/peps/pep-0008/). Use type hints for all function signatures. Format and lint with ruff: `ruff format && ruff check --fix` — this handles style consistency automatically
- **TypeScript/React**: Use ESLint and Prettier with the project configuration. Functional components and hooks preferred
- **Commit messages**: Be clear and specific. Reference issue numbers where relevant (e.g., "Fix scheduler race condition in Phase 1 (#42)")

**Testing:**

- Unit tests are required for code changes. Aim for >80% coverage on new code
- Integration tests are encouraged for complex features
- Run `go test ./...` before submitting PR for Go changes

**Submitting a PR:**

1. Make your changes with clear commits
2. Run tests and linting locally
3. Open a pull request with a clear description of what you're doing and why
4. Respond to review feedback promptly

(If you're new to open source, see [GitHub's guide to forking repositories](https://docs.github.com/en/get-started/quickstart/fork-a-repo).)


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

The [ROADMAP.md](ROADMAP.md) document outlines development phases. Current priorities:

- **Phase 1** (v0.1 — in progress): Core scheduler, pull-based workers, basic web UI, OpenJD execution
- **Phase 2** (planned): Product system, preset library integration, additional DCC support
- **Phase 3** (planned): Auth (LDAP, OAuth2), multi-user role model
- **Phase 4** (planned): Production hardening, PostgreSQL, HA, auto-scaling

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

By contributing code, you agree that your contribution will be made available under both licenses. See [LICENSE](LICENSE) for details.

If you have questions about licensing compatibility, open a discussion or contact [robin@uberware.net](mailto:robin@uberware.net).

---

## Getting Help

- **Questions about contributing?** Open a [discussion](https://github.com/Uberware/sqi/discussions)
- **Questions about the architecture?** Read [ROADMAP.md](ROADMAP.md)
- **Need a development environment tip?** Ask in discussions or open an issue
- **Direct contact**: [robin@uberware.net](mailto:robin@uberware.net)

---

## Thank You

We appreciate all contributions, large and small. Every preset shared, bug report filed, and piece of code submitted makes `sqi` better for everyone.

Happy coding.
