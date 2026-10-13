// SPDX-License-Identifier: AGPL-3.0-or-later

package restore

import (
	"context"
	"errors"
)

// placePostgres loads the backup archive into the configured PostgreSQL
// database. Not implemented yet.
func placePostgres(_ context.Context, _ Options) (int64, error) {
	return 0, errors.New("restore: PostgreSQL restore is not implemented yet")
}
