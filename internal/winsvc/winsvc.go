// SPDX-License-Identifier: AGPL-3.0-or-later

// Package winsvc lets sqi-server and sqi-worker run as Windows services and
// installs them as such.
//
// [Run] is the only thing the binaries' run commands call. When the Windows
// Service Control Manager (SCM) started the process, Run hosts the command
// under svc.Run and cancels its context on Stop or PreShutdown. Otherwise — a
// console on Windows, or any other OS — it cancels on SIGINT/SIGTERM as the
// signal.NotifyContext the commands used before this package existed did, so
// non-service behavior does not change. Either way context.Cause names what
// stopped the command: the signal ("interrupt", "terminated"), or
// "service stop" / "service preshutdown".
//
// This package is a leaf: it must not import internal/config, internal/server
// or internal/worker/...; callers pass in whatever configuration it needs.
package winsvc

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	sqilog "github.com/uberware/sqi/internal/log"
)

type ctxKey struct{}

func withService(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, ctxKey{}, name)
}

// ServiceName returns the installed service name when running as a service
// (delivered by the SCM, so it reflects `service install --name`), else "".
func ServiceName(ctx context.Context) string {
	if s, ok := ctx.Value(ctxKey{}).(string); ok {
		return s
	}
	return ""
}

// ProgramDataDir returns %ProgramData%, falling back to C:\ProgramData.
func ProgramDataDir() string {
	if d := os.Getenv("ProgramData"); d != "" {
		return d
	}
	return `C:\ProgramData`
}

// DefaultLogPath is where a service logs when log.file is not configured.
func DefaultLogPath(serviceName string) string {
	return filepath.Join(ProgramDataDir(), "sqi", "logs", serviceName+".log")
}

// requireConfigFile refuses to run a Windows service that was given no
// --config (configPath empty); binary names the command to suggest. Without
// one, both binaries search a default path for their configuration, and on
// Windows two of its entries are open to any local user: the relative
// config\<binary>.yaml resolves under the service's working directory
// (%ProgramData%\sqi, where Users may create folders), and /etc/sqi resolves to
// \etc\sqi on that directory's drive (C:\ grants Authenticated Users
// create-folder). Whoever put a file there would choose what a LocalSystem
// service runs. `service install` always passes --config, so this refuses only
// a service registered by hand without one. Only service mode calls it: in a
// console the search path is the operator's own.
func requireConfigFile(binary, configPath string) error {
	if configPath != "" {
		return nil
	}
	wd := workDir("")
	return fmt.Errorf("running as a Windows service requires --config: without it the configuration "+
		"is searched for in %s and in a config folder under the service's working directory %s, "+
		"which non-administrators can create; install the service with `%s service install` "+
		"(it passes --config) or add --config <path> to the service's command line",
		filepath.VolumeName(wd)+`\etc\sqi`, wd, binary)
}

// workDir is the directory a service runs in: the one containing configPath
// (already absolute, see Run), or %ProgramData%\sqi when it is empty.
func workDir(configPath string) string {
	if configPath == "" {
		return filepath.Join(ProgramDataDir(), "sqi")
	}
	return filepath.Dir(configPath)
}

// ResolveLogFile returns the log file to write: configured when set; in
// service mode with nothing configured, [DefaultLogPath] (a missing directory
// created with a protected ACL — a hand-registered service never ran `service
// install`); otherwise "" for stderr.
func ResolveLogFile(ctx context.Context, configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	name := ServiceName(ctx)
	if name == "" {
		return "", nil
	}
	path := DefaultLogPath(name)
	if err := createLogDir(filepath.Dir(path)); err != nil {
		return "", fmt.Errorf("winsvc: create log directory: %w", err)
	}
	return path, nil
}

// LogOutput opens the log destination for a run command: [ResolveLogFile]
// then [sqilog.Output].
func LogOutput(ctx context.Context, file string, maxSizeMB, maxBackups int) (io.WriteCloser, error) {
	path, err := ResolveLogFile(ctx, file)
	if err != nil {
		return nil, err
	}
	return sqilog.Output(path, maxSizeMB, maxBackups)
}
