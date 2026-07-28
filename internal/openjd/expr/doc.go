// SPDX-License-Identifier: AGPL-3.0-or-later

// Package expr reads and evaluates the OpenJD EXPR extension's expression
// language.
//
// The package is a self-contained leaf: it imports nothing from
// internal/openjd. The dependency runs the other way — internal/openjd will
// import expr at EXPR sub-project E — so expr can be tested with no template
// machinery present.
package expr
