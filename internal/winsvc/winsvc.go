// SPDX-License-Identifier: AGPL-3.0-or-later

// Package winsvc lets sqi-server and sqi-worker run as Windows services and
// installs them as such.
//
// [Run] is the only thing the binaries' run commands call. When the Windows
// Service Control Manager (SCM) started the process, Run hosts the command
// under svc.Run and cancels its context on Stop or PreShutdown. Otherwise — a
// console on Windows, or any other OS — it is exactly the signal.NotifyContext
// the commands used before this package existed, so non-service behavior does
// not change.
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
	"sync/atomic"

	sqilog "github.com/uberware/sqi/internal/log"
)

// Option configures [Run].
type Option func(*options)

type options struct {
	configPath string
}

// WithWorkDirFromConfig makes a service run in the directory containing
// configPath (or %ProgramData%\sqi when configPath is empty), so relative
// paths in the config — the server's default sqi.db and data/nats — resolve
// beside it instead of in C:\Windows\System32, a service's default working
// directory. It has no effect outside service mode. With configPath empty the
// commands refuse to load any configuration (see [RequireConfigFile]), so the
// service stops with an error straight after changing into that directory.
func WithWorkDirFromConfig(configPath string) Option {
	return func(o *options) { o.configPath = configPath }
}

type ctxKey int

const (
	serviceNameKey ctxKey = iota
	stopReasonKey
)

// stopReason records why the SCM asked the service to stop.
type stopReason struct{ v atomic.Value }

func (r *stopReason) set(s string) { r.v.Store(s) }

func (r *stopReason) get() string {
	if s, ok := r.v.Load().(string); ok {
		return s
	}
	return ""
}

func withService(ctx context.Context, name string, reason *stopReason) context.Context {
	ctx = context.WithValue(ctx, serviceNameKey, name)
	return context.WithValue(ctx, stopReasonKey, reason)
}

// ServiceName returns the installed service name when running as a service
// (delivered by the SCM, so it reflects `service install --name`), else "".
func ServiceName(ctx context.Context) string {
	if s, ok := ctx.Value(serviceNameKey).(string); ok {
		return s
	}
	return ""
}

// StopReason returns "service stop" or "service preshutdown" once the SCM has
// asked the service to stop, else "".
func StopReason(ctx context.Context) string {
	r, ok := ctx.Value(stopReasonKey).(*stopReason)
	if !ok {
		return ""
	}
	return r.get()
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

// RequireConfigFile refuses to run a Windows service that was given no
// --config (configPath empty); binary names the command to suggest. Without
// one, both binaries search a default path for their configuration, and on
// Windows two of its entries are open to any local user: the relative
// config\<binary>.yaml resolves under the service's working directory
// (%ProgramData%\sqi, where Users may create folders), and /etc/sqi resolves to
// \etc\sqi on that directory's drive (C:\ grants Authenticated Users
// create-folder). Whoever put a file there would choose what a LocalSystem
// service runs. `service install` always passes --config, so this refuses only
// a service registered by hand without one. It returns nil in a console, where
// the search path is the operator's own.
func RequireConfigFile(ctx context.Context, binary, configPath string) error {
	if configPath != "" || ServiceName(ctx) == "" {
		return nil
	}
	wd := workDir("")
	return fmt.Errorf("running as a Windows service requires --config: without it the configuration "+
		"is searched for in %s and in a config folder under the service's working directory %s, "+
		"which non-administrators can create; install the service with `%s service install` "+
		"(it passes --config) or add --config <path> to the service's command line",
		filepath.VolumeName(wd)+`\etc\sqi`, wd, binary)
}

func workDir(configPath string) string {
	if configPath == "" {
		return filepath.Join(ProgramDataDir(), "sqi")
	}
	if abs, err := filepath.Abs(configPath); err == nil {
		configPath = abs
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
