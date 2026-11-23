// SPDX-License-Identifier: AGPL-3.0-or-later

// Package pkg is the root of the sqi public Go API.
//
// It holds no public packages. All sqi code lives under internal/ and is not
// importable by external code.
//
// # Versioning
//
// Anything placed under pkg/ is part of the public API contract and must
// follow [semantic versioning].  Breaking changes require a major version bump.
// Internal-only code belongs in internal/ and carries no compatibility guarantee.
//
// [semantic versioning]: https://semver.org
package pkg
