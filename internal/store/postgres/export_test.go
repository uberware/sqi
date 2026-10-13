// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"database/sql"

	"github.com/uberware/sqi/internal/store/sqlstore"
)

// LockAnchors exposes the dialect's anchor locking to the external tests,
// which need storetest (and storetest imports this package).
func LockAnchors(ctx context.Context, tx *sql.Tx, anchors ...sqlstore.Anchor) error {
	return pgDialect{}.LockAnchors(ctx, tx, anchors...)
}
