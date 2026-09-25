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

const pollInterval = 250 * time.Millisecond

// servicePreshutdownInfo is SERVICE_PRESHUTDOWN_INFO, which
// golang.org/x/sys/windows v0.48.0 does not declare (it does declare
// SERVICE_CONFIG_PRESHUTDOWN_INFO).
type servicePreshutdownInfo struct {
	timeoutMillis uint32
}

// StatusInfo is what `service status` prints.
type StatusInfo struct {
	Name, State, Account, StartType, BinaryPath string
	DelayedAutoStart                            bool
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
	if s, openErr := m.OpenService(cfg.Name); openErr == nil {
		s.Close() // only probing for existence
		return fmt.Errorf("%w: %s", ErrServiceExists, cfg.Name)
	}
	s, err := m.CreateService(cfg.Name, cfg.ExePath, mgr.Config{
		DisplayName:      cfg.DisplayName,
		Description:      cfg.Description,
		StartType:        mgr.StartAutomatic,
		DelayedAutoStart: cfg.DelayedAutoStart,
		ServiceStartName: cfg.Account,
		Password:         cfg.Password,
	}, cfg.Args...)
	if err != nil {
		return fmt.Errorf("create service %s: %w", cfg.Name, err)
	}
	defer s.Close()
	defer func() {
		if err != nil {
			s.Delete() //nolint:errcheck // rollback; the configure error is what matters
		}
	}()
	return configure(s, cfg)
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
	if st, qerr := s.Query(); qerr == nil && st.State != svc.Stopped {
		if _, err := stopAndWait(s, wait); err != nil {
			return err
		}
	}
	if err := s.Delete(); err != nil {
		return fmt.Errorf("delete service %s: %w", name, err)
	}
	return nil
}

// Start starts the service and waits up to wait for it to run or stop. A
// service that stops instead of running is an error carrying its exit codes.
func Start(name string, wait time.Duration) (StatusInfo, error) {
	m, err := connect()
	if err != nil {
		return StatusInfo{}, err
	}
	defer m.Disconnect() //nolint:errcheck // handle cleanup
	s, err := open(m, name)
	if err != nil {
		return StatusInfo{}, err
	}
	defer s.Close()
	if err := s.Start(); err != nil {
		return StatusInfo{}, fmt.Errorf("start service %s: %w", name, err)
	}
	st, err := waitFor(s, wait, func(st svc.Status) bool { return st.State == svc.Running || st.State == svc.Stopped })
	info := statusInfo(s, name, st)
	if err != nil {
		return info, err
	}
	if st.State == svc.Stopped {
		return info, fmt.Errorf("service %s stopped during startup (exit code %d, service exit code %d)",
			name, st.Win32ExitCode, st.ServiceSpecificExitCode)
	}
	return info, nil
}

// Stop asks the service to stop and waits up to wait for it.
func Stop(name string, wait time.Duration) (StatusInfo, error) {
	m, err := connect()
	if err != nil {
		return StatusInfo{}, err
	}
	defer m.Disconnect() //nolint:errcheck // handle cleanup
	s, err := open(m, name)
	if err != nil {
		return StatusInfo{}, err
	}
	defer s.Close()
	st, err := stopAndWait(s, wait)
	return statusInfo(s, name, st), err
}

// Status reports the service's state and registration.
func Status(name string) (StatusInfo, error) {
	m, err := connect()
	if err != nil {
		return StatusInfo{}, err
	}
	defer m.Disconnect() //nolint:errcheck // handle cleanup
	s, err := open(m, name)
	if err != nil {
		return StatusInfo{}, err
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return StatusInfo{}, fmt.Errorf("query service %s: %w", name, err)
	}
	return statusInfo(s, name, st), nil
}

func stopAndWait(s *mgr.Service, wait time.Duration) (svc.Status, error) {
	if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
		return svc.Status{}, fmt.Errorf("stop service %s: %w", s.Name, err)
	}
	return waitFor(s, wait, func(st svc.Status) bool { return st.State == svc.Stopped })
}

func waitFor(s *mgr.Service, wait time.Duration, done func(svc.Status) bool) (svc.Status, error) {
	deadline := time.Now().Add(wait)
	for {
		st, err := s.Query()
		if err != nil {
			return svc.Status{}, fmt.Errorf("query service %s: %w", s.Name, err)
		}
		if done(st) {
			return st, nil
		}
		if time.Now().After(deadline) {
			return st, fmt.Errorf("service %s still %s after %v", s.Name, stateString(st.State), wait)
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
		info.DelayedAutoStart = c.DelayedAutoStart
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
