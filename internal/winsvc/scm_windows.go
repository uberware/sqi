// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package winsvc

import (
	"errors"
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

var (
	// ErrServiceExists is returned by Install when the name is taken.
	ErrServiceExists = errors.New("service already exists")
	// ErrServiceNotFound is returned when no service has the given name.
	ErrServiceNotFound = errors.New("service not found")
)

const (
	pollInterval = 250 * time.Millisecond
	// startSettle is how long Start keeps watching a service after it reports
	// Running (see awaitStart).
	startSettle = 3 * time.Second
)

// servicePreshutdownInfo is SERVICE_PRESHUTDOWN_INFO, which
// golang.org/x/sys/windows v0.48.0 does not declare (it does declare
// SERVICE_CONFIG_PRESHUTDOWN_INFO).
type servicePreshutdownInfo struct {
	timeoutMillis uint32
}

// StatusInfo is what `service status` prints.
type StatusInfo struct {
	Name, State, Account, StartType, BinaryPath string
	Win32ExitCode, ServiceExitCode, PID         uint32
}

func connect() (*mgr.Mgr, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, fmt.Errorf("connect to the service control manager: %w", err)
	}
	return m, nil
}

func open(m *mgr.Mgr, name string) (*mgr.Service, error) {
	s, err := m.OpenService(name)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil, fmt.Errorf("%w: %s", ErrServiceNotFound, name)
	}
	if err != nil {
		return nil, fmt.Errorf("open service %s: %w", name, err)
	}
	return s, nil
}

// withOpenService connects to the SCM, opens name and calls fn with it.
func withOpenService(name string, fn func(*mgr.Service) error) error {
	m, err := connect()
	if err != nil {
		return err
	}
	defer m.Disconnect() //nolint:errcheck // handle cleanup
	s, err := open(m, name)
	if err != nil {
		return err
	}
	defer s.Close()
	return fn(s)
}

// Exists reports whether a service named name is registered. Failing to open
// it for any reason but its absence is an error, not "absent".
func Exists(name string) (bool, error) {
	m, err := connect()
	if err != nil {
		return false, err
	}
	defer m.Disconnect() //nolint:errcheck // handle cleanup
	s, err := open(m, name)
	switch {
	case err == nil:
		s.Close() // only probing for existence
		return true, nil
	case errors.Is(err, ErrServiceNotFound):
		return false, nil
	default:
		return false, err
	}
}

// Install registers cfg: start type Automatic (delayed when
// cfg.DelayedAutoStart), cfg's restart-on-failure actions with recovery on
// non-crash failures enabled too, and cfg.PreShutdownTimeout. mgr.CreateService
// quotes and escapes the executable path and cfg.Args. On any failure after
// creation the half-configured service is deleted again.
func Install(cfg ServiceConfig) (err error) {
	m, err := connect()
	if err != nil {
		return err
	}
	defer m.Disconnect() //nolint:errcheck // handle cleanup
	s, err := m.CreateService(cfg.Name, cfg.ExePath, mgr.Config{
		DisplayName:      cfg.DisplayName,
		Description:      cfg.Description,
		StartType:        mgr.StartAutomatic,
		DelayedAutoStart: cfg.DelayedAutoStart,
		ServiceStartName: cfg.Account,
		Password:         cfg.Password,
	}, cfg.Args...)
	if err != nil {
		return createError(cfg.Name, err)
	}
	defer s.Close()
	defer func() {
		if err != nil {
			s.Delete() //nolint:errcheck // rollback; the configure error is what matters
		}
	}()
	return configure(s, cfg)
}

// createError reports mgr.CreateService's error. ERROR_SERVICE_EXISTS (the
// name is taken, including by a race with another install after runInstall's
// Exists check) is ErrServiceExists, so callers can still point at `service
// uninstall`.
func createError(name string, err error) error {
	if errors.Is(err, windows.ERROR_SERVICE_EXISTS) {
		return fmt.Errorf("%w: %s", ErrServiceExists, name)
	}
	return fmt.Errorf("create service %s: %w", name, err)
}

func configure(s *mgr.Service, cfg ServiceConfig) error {
	actions := make([]mgr.RecoveryAction, len(cfg.Recovery))
	for i, d := range cfg.Recovery {
		actions[i] = mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: d}
	}
	resetSeconds := uint32(cfg.RecoveryReset / time.Second) //nolint:gosec // G115: 24h in seconds, far inside uint32
	if err := s.SetRecoveryActions(actions, resetSeconds); err != nil {
		return fmt.Errorf("set recovery actions: %w", err)
	}
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("enable recovery on non-crash failures: %w", err)
	}
	// A shutdown grace period is minutes at most; uint32 milliseconds covers ~49 days.
	timeout := uint32(cfg.PreShutdownTimeout / time.Millisecond) //nolint:gosec // G115: see above
	info := servicePreshutdownInfo{timeoutMillis: timeout}
	if err := windows.ChangeServiceConfig2(s.Handle, windows.SERVICE_CONFIG_PRESHUTDOWN_INFO,
		(*byte)(unsafe.Pointer(&info))); err != nil {
		return fmt.Errorf("set preshutdown timeout: %w", err)
	}
	return nil
}

// Uninstall stops the service if it is running (waiting up to wait) and
// deletes it. Config, data and logs are untouched.
func Uninstall(name string, wait time.Duration) error {
	return withOpenService(name, func(s *mgr.Service) error {
		if st, qerr := s.Query(); qerr == nil && st.State != svc.Stopped {
			if _, err := stopAndWait(s, name, wait); err != nil {
				return err
			}
		}
		if err := s.Delete(); err != nil {
			return fmt.Errorf("delete service %s: %w", name, err)
		}
		return nil
	})
}

// Start starts the service and waits up to wait for it to run or stop, then
// watches a running service for startSettle more (see awaitStart). A service
// that stops instead of running, or within that window, is an error carrying
// its exit codes.
func Start(name string, wait time.Duration) (info StatusInfo, err error) {
	err = withOpenService(name, func(s *mgr.Service) error {
		if err := s.Start(); err != nil {
			return fmt.Errorf("start service %s: %w", name, err)
		}
		st, err := awaitStart(s, name, wait, startSettle)
		info = statusInfo(s, name, st)
		return err
	})
	return info, err
}

// awaitStart waits up to wait for a service just asked to start to run or
// stop. The service host reports Running before the binary has loaded its
// configuration, so a service that runs is watched for settle more: a bad
// configuration stops it within moments, and that is a failed start too.
// Stopped, either way, is an error carrying the exit codes. The whole wait is
// bounded by wait + settle plus a poll interval or two.
func awaitStart(s controller, name string, wait, settle time.Duration) (svc.Status, error) {
	st, err := waitFor(s, name, wait, time.Now().Add(wait),
		func(st svc.Status) bool { return st.State == svc.Running || st.State == svc.Stopped })
	if err == nil && st.State == svc.Running {
		st, err = settleRunning(s, name, settle)
	}
	if err != nil {
		return st, err
	}
	if st.State == svc.Stopped {
		return st, fmt.Errorf("service %s stopped during startup (exit code %d, service exit code %d)",
			name, st.Win32ExitCode, st.ServiceSpecificExitCode)
	}
	return st, nil
}

// settleRunning polls a service that has just reported Running until it stops
// or window has passed, and returns the last status it saw.
func settleRunning(s controller, name string, window time.Duration) (svc.Status, error) {
	deadline := time.Now().Add(window)
	for {
		time.Sleep(pollInterval)
		st, err := s.Query()
		if err != nil {
			return svc.Status{}, fmt.Errorf("query service %s: %w", name, err)
		}
		if st.State == svc.Stopped || !time.Now().Before(deadline) {
			return st, nil
		}
	}
}

// Stop asks the service to stop and waits up to wait for it.
func Stop(name string, wait time.Duration) (info StatusInfo, err error) {
	err = withOpenService(name, func(s *mgr.Service) error {
		st, err := stopAndWait(s, name, wait)
		info = statusInfo(s, name, st)
		return err
	})
	return info, err
}

// PreShutdownTimeout reads the PreShutdown timeout the service is registered
// with — for a service `service install` created, its drain timeout plus
// ShutdownMargin.
func PreShutdownTimeout(name string) (time.Duration, error) {
	var info servicePreshutdownInfo
	err := withOpenService(name, func(s *mgr.Service) error {
		var needed uint32
		if err := windows.QueryServiceConfig2(s.Handle, windows.SERVICE_CONFIG_PRESHUTDOWN_INFO,
			(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), &needed); err != nil {
			return fmt.Errorf("query preshutdown timeout of %s: %w", name, err)
		}
		return nil
	})
	return time.Duration(info.timeoutMillis) * time.Millisecond, err
}

// Status reports the service's state and registration.
func Status(name string) (info StatusInfo, err error) {
	err = withOpenService(name, func(s *mgr.Service) error {
		st, err := s.Query()
		if err != nil {
			return fmt.Errorf("query service %s: %w", name, err)
		}
		info = statusInfo(s, name, st)
		return nil
	})
	return info, err
}

// controller is the part of *mgr.Service that stopping and waiting use, so
// they can be tested without an SCM.
type controller interface {
	Control(c svc.Cmd) (svc.Status, error)
	Query() (svc.Status, error)
}

// stopAndWait stops s and waits for Stopped, all within one wait budget
// (overrun: at most one poll interval plus the SCM calls in flight). A
// service already stopping is just waited for; one still starting is waited
// out of StartPending first, then stopped once if it came up.
func stopAndWait(s controller, name string, wait time.Duration) (svc.Status, error) {
	deadline := time.Now().Add(wait)
	starting, err := requestStop(s, name)
	if err != nil {
		return svc.Status{}, err
	}
	if starting {
		st, err := waitFor(s, name, wait, deadline, func(st svc.Status) bool { return st.State != svc.StartPending })
		if err != nil {
			return st, err
		}
		if st.State != svc.Stopped {
			if _, err := requestStop(s, name); err != nil {
				return st, err
			}
		}
	}
	return waitFor(s, name, wait, deadline, func(st svc.Status) bool { return st.State == svc.Stopped })
}

// requestStop sends Stop. The SCM refuses it with CANNOT_ACCEPT_CTRL (and the
// current status) while a service is start- or stop-pending: stop-pending
// needs nothing more, and starting reports start-pending, which must be
// waited out before Stop can be sent. CANNOT_ACCEPT_CTRL with Stopped means
// the service stopped between the caller's Query and this Control.
func requestStop(s controller, name string) (starting bool, err error) {
	st, err := s.Control(svc.Stop)
	switch {
	case err == nil || errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE):
		return false, nil
	case errors.Is(err, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL) &&
		(st.State == svc.StopPending || st.State == svc.Stopped):
		return false, nil
	case errors.Is(err, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL) && st.State == svc.StartPending:
		return true, nil
	default:
		return false, fmt.Errorf("stop service %s: %w", name, err)
	}
}

// waitFor polls s until done, giving up once deadline has passed; wait is
// only reported.
func waitFor(s controller, name string, wait time.Duration, deadline time.Time, done func(svc.Status) bool) (svc.Status, error) {
	for {
		st, err := s.Query()
		if err != nil {
			return svc.Status{}, fmt.Errorf("query service %s: %w", name, err)
		}
		if done(st) {
			return st, nil
		}
		if time.Now().After(deadline) {
			return st, fmt.Errorf("service %s still %s after %v", name, stateString(st.State), wait)
		}
		time.Sleep(pollInterval)
	}
}

func statusInfo(s *mgr.Service, name string, st svc.Status) StatusInfo {
	info := StatusInfo{
		Name: name, State: stateString(st.State), PID: st.ProcessId,
		Win32ExitCode: st.Win32ExitCode, ServiceExitCode: st.ServiceSpecificExitCode,
	}
	if c, err := s.Config(); err == nil {
		info.Account = c.ServiceStartName
		info.BinaryPath = c.BinaryPathName
		info.StartType = startTypeString(c.StartType, c.DelayedAutoStart)
	}
	return info
}

func stateString(st svc.State) string {
	switch st {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "start pending"
	case svc.StopPending:
		return "stop pending"
	case svc.Running:
		return "running"
	case svc.ContinuePending:
		return "continue pending"
	case svc.PausePending:
		return "pause pending"
	case svc.Paused:
		return "paused"
	default:
		return fmt.Sprintf("unknown (%d)", st)
	}
}

func startTypeString(t uint32, delayed bool) string {
	switch t {
	case mgr.StartAutomatic:
		if delayed {
			return "automatic (delayed)"
		}
		return "automatic"
	case mgr.StartManual:
		return "manual"
	case mgr.StartDisabled:
		return "disabled"
	default:
		return fmt.Sprintf("unknown (%d)", t)
	}
}
