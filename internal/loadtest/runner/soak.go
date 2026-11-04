// SPDX-License-Identifier: AGPL-3.0-or-later

package runner

import (
	"context"

	"github.com/uberware/sqi/internal/loadtest/report"
)

// Soak is Run in soak mode: heap and goroutine profiles every
// DefaultSoakProfileEvery unless Options.ProfileEvery says otherwise, and the
// record marked as a soak so its growth slopes are read as such. Any duration
// is accepted; a soak started from CI is capped by the workflow, not here.
func Soak(ctx context.Context, o Options) (*report.Record, error) {
	o.Mode = report.ModeSoak
	return Run(ctx, o)
}
