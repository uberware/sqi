// SPDX-License-Identifier: AGPL-3.0-or-later

package winsvc

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// runConsole is the non-service path, identical to what serve/start did
// before: the first SIGINT/SIGTERM cancels ctx. stop() runs only via defer,
// when fn returns, exactly as the commands' own `defer stop()` did — it is not
// called earlier, so signal handling stays in place for the whole of fn.
func runConsole(fn func(context.Context) error) error {
	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,    // SIGINT  (Ctrl-C)
		syscall.SIGTERM, // sent by systemd / Docker / Kubernetes
	)
	defer stop()
	return fn(ctx)
}
