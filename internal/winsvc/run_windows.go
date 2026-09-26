// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package winsvc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/windows/svc"
)

// IsService reports whether the SCM started this process. The answer cannot
// change during the process's life, and svc.IsWindowsService snapshots the
// whole process table, so it is computed once.
var IsService = sync.OnceValue(func() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
})

// Run hosts fn as a Windows service when the SCM started the process, and
// otherwise runs it as a console command. See the package doc.
//
// configPath points at the command's --config value, which only matters in
// service mode. There Run first makes it absolute in place, so fn loads the
// same file after the service changes into its directory (or into
// %ProgramData%\sqi when it is empty) — a relative path would otherwise
// resolve against the new directory, and the default, C:\Windows\System32,
// is no place for the server's sqi.db and data/nats. An empty configPath
// never reaches fn: see requireConfigFile.
func Run(name string, configPath *string, fn func(ctx context.Context) error) error {
	if !IsService() {
		return runConsole(fn)
	}
	if *configPath != "" {
		abs, err := filepath.Abs(*configPath)
		if err != nil {
			return fmt.Errorf("winsvc: resolve config path: %w", err)
		}
		*configPath = abs
	}
	h := &handler{
		name:            name,
		configPath:      *configPath,
		workDir:         workDir(*configPath),
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
