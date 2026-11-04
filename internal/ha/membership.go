// SPDX-License-Identifier: AGPL-3.0-or-later

package ha

import (
	"context"
	"log/slog"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// RunMembership keeps this server's membership row fresh until ctx ends:
// at once, then every third of the lease, whether it leads or not. Each
// upsert reports whether the broker is up, which is what lets a leader that
// lost its broker tell whether a standby could do better. Pruning members
// long gone is the leader's job, not this loop's.
func (e *Elector) RunMembership(ctx context.Context) {
	for ctx.Err() == nil {
		e.upsertSelf(ctx)
		if e.sleep(ctx, e.renewInterval()) != nil {
			return
		}
	}
}

// upsertSelf writes this server's membership row once. A failure is logged
// and left to the next upsert.
func (e *Elector) upsertSelf(ctx context.Context) {
	m := e.cfg.Self
	m.BrokerOK, _ = e.observeBroker(e.clock.Now())
	uctx, cancel := context.WithTimeout(ctx, e.renewInterval())
	defer cancel()
	if err := e.st.UpsertServerMember(uctx, m); err != nil && ctx.Err() == nil {
		e.log.WarnContext(ctx, "ha: server member upsert failed", slog.Any("error", err))
	}
}

// standbyHasBroker reports whether a member other than self reached its
// broker within ttl of nowUS, the database clock the stamps were read
// against. The local clock never enters it.
func standbyHasBroker(members []store.ServerMember, self string, nowUS int64, ttl time.Duration) bool {
	for _, m := range members {
		if m.InstanceID != self && m.BrokerOKUS != nil && nowUS-*m.BrokerOKUS <= ttl.Microseconds() {
			return true
		}
	}
	return false
}
