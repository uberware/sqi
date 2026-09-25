// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package winsvc

import (
	"context"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows/svc"
)

// IsService reports whether the SCM started this process.
func IsService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// Run hosts fn as a Windows service when the SCM started the process, and
// otherwise runs it as a console command. See the package doc.
func Run(name string, fn func(ctx context.Context) error, opts ...Option) error {
	if !IsService() {
		return runConsole(fn)
	}
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	h := &handler{
		name:            name,
		workDir:         workDir(o.configPath),
		fn:              fn,
		checkpointEvery: 2 * time.Second,
		tracePath:       DefaultLogPath,
		chdir:           chdirCreating,
	}
	if err := svc.Run(name, h); err != nil {
		return fmt.Errorf("winsvc: run service %s: %w", name, err)
	}
	return h.err
}

func chdirCreating(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	return os.Chdir(dir)
}
