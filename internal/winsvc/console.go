// SPDX-License-Identifier: AGPL-3.0-or-later

package winsvc

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
)

// runConsole is the non-service path, what serve/start did before: the first
// SIGINT/SIGTERM cancels ctx, and signal handling stays in place until fn
// returns. It is signal.NotifyContext except for the cause: the bare signal
// name ("interrupt", "terminated"), which the worker logs as its shutdown
// trigger exactly as it did before this package existed, where NotifyContext
// would say "terminated signal received".
func runConsole(fn func(context.Context) error) error {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	sigs := make(chan os.Signal, 1)
	signal.Notify(
		sigs,
		os.Interrupt,    // SIGINT  (Ctrl-C)
		syscall.SIGTERM, // sent by systemd / Docker / Kubernetes
	)
	defer signal.Stop(sigs)
	go func() {
		select {
		case s := <-sigs:
			cancel(errors.New(s.String()))
		case <-ctx.Done():
		}
	}()
	return fn(ctx)
}
