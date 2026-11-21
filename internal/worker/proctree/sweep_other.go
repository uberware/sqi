// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !linux && !darwin

package proctree

import (
	"context"
	"log/slog"
)

// Sweep does nothing here. On Windows a tree's job object dies with the
// worker that created it, so nothing is left to sweep; other systems have no
// process-table reader.
func (*Ledger) Sweep(context.Context, *slog.Logger) error { return nil }
