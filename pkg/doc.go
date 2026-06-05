// SPDX-License-Identifier: AGPL-3.0-only

// Package pkg is the root of the sqi public Go API surface.
//
// # Phase 1 status
//
// This package tree is intentionally empty in Phase 1.  All sqi-server
// internals live under internal/ and are not importable by external code.
// The pkg/ tree will be populated as surfaces stabilize and are ready to
// carry a compatibility commitment.
//
// # Planned packages
//
// The following packages are planned for future phases:
//
//   - pkg/client — Go client for the sqi-server REST API and WebSocket
//     subscriptions.  Exposes a typed [Client] that wraps job submission,
//     status polling, log streaming, and worker administration.
//
//   - pkg/openjd — Re-export of the OpenJD parser and validator so that
//     third-party tools (pipeline bridges, custom submission UIs) can validate
//     templates without importing internal/ packages.
//
//   - pkg/worker — Go SDK for building custom sqi worker plugins.  Provides
//     the NATS-based registration, heartbeat, and task-execution loop so
//     integrators only need to supply a [TaskRunner] implementation.
//
// # Versioning
//
// Anything placed under pkg/ is part of the public API contract and must
// follow [semantic versioning].  Breaking changes require a major version bump.
// Internal-only code belongs in internal/ and carries no compatibility guarantee.
//
// [semantic versioning]: https://semver.org
package pkg
