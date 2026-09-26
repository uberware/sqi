// SPDX-License-Identifier: AGPL-3.0-or-later

package winsvc

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ShutdownMargin is added to a binary's own drain timeout to form the
// service's PreShutdown timeout, covering process exit after the drain.
const ShutdownMargin = 15 * time.Second

// recoveryDelays and recoveryReset are the restart-on-failure policy every
// installed service gets (the equivalent of systemd's Restart=on-failure).
var recoveryDelays = []time.Duration{5 * time.Second, 30 * time.Second, 60 * time.Second}

const recoveryReset = 24 * time.Hour

// InstallSpec is what `service install` knows after parsing its flags.
type InstallSpec struct {
	Name, DisplayName, Description string
	ExePath                        string // absolute path of the binary to register
	RunVerb                        string // "serve" or "start"
	ConfigPath                     string // absolute; baked into the command line
	Account                        string // NormalizeAccount'd; "" = LocalSystem
	Password                       string
	DelayedAutoStart               bool
	DrainTimeout                   time.Duration // the binary's own graceful-shutdown bound
}

// ServiceConfig is the exact service registration Install creates.
type ServiceConfig struct {
	Name, DisplayName, Description string
	ExePath                        string
	Args                           []string // mgr.CreateService quotes and escapes these
	Account, Password              string
	DelayedAutoStart               bool
	Recovery                       []time.Duration
	RecoveryReset                  time.Duration
	PreShutdownTimeout             time.Duration
}

// BuildServiceConfig validates spec and derives the registration from it.
func BuildServiceConfig(spec InstallSpec) (ServiceConfig, error) {
	if err := validateSpec(spec); err != nil {
		return ServiceConfig{}, err
	}
	return ServiceConfig{
		Name:               spec.Name,
		DisplayName:        spec.DisplayName,
		Description:        spec.Description,
		ExePath:            spec.ExePath,
		Args:               []string{spec.RunVerb, "--config", spec.ConfigPath},
		Account:            spec.Account,
		Password:           spec.Password,
		DelayedAutoStart:   spec.DelayedAutoStart,
		Recovery:           append([]time.Duration(nil), recoveryDelays...),
		RecoveryReset:      recoveryReset,
		PreShutdownTimeout: spec.DrainTimeout + ShutdownMargin,
	}, nil
}

// validateName checks a service name the way the SCM would, so `service
// install` can reject a bad one before probing for it or changing anything.
func validateName(name string) error {
	if name == "" || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("service name %q is invalid: it must be non-empty and contain no slashes", name)
	}
	return nil
}

func validateSpec(spec InstallSpec) error {
	if err := validateName(spec.Name); err != nil {
		return err
	}
	switch {
	case !filepath.IsAbs(spec.ExePath):
		return fmt.Errorf("executable path %q must be absolute", spec.ExePath)
	case !filepath.IsAbs(spec.ConfigPath):
		return fmt.Errorf("config path %q must be absolute", spec.ConfigPath)
	case spec.RunVerb == "":
		return errors.New("run verb must be set (serve or start)")
	case spec.Account != "" && spec.Password == "":
		return fmt.Errorf("a password is required to run the service as %s", spec.Account)
	case spec.DrainTimeout <= 0:
		return fmt.Errorf("drain timeout must be positive, got %v", spec.DrainTimeout)
	}
	return nil
}

// NormalizeAccount maps --user to the SCM's account syntax: "" and
// "LocalSystem" mean LocalSystem (""); a bare name is a local account (.\name);
// DOMAIN\name and UPNs pass through.
func NormalizeAccount(user string) string {
	switch {
	case user == "" || strings.EqualFold(user, "LocalSystem"):
		return ""
	case strings.ContainsAny(user, `\@`):
		return user
	default:
		return `.\` + user
	}
}

// ResolveConfigPath returns the absolute config path `service install` bakes
// into the service command line: flagValue, or %ProgramData%\sqi\<binary>.yaml
// when empty. The file must exist — a service cannot prompt for one.
func ResolveConfigPath(flagValue, binary string) (string, error) {
	path := flagValue
	if path == "" {
		path = filepath.Join(ProgramDataDir(), "sqi", binary+".yaml")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve config path %q: %w", path, err)
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("config file %s not found (%w); create one first, e.g.:\n  %s config print > \"%s\"",
			abs, err, binary, abs)
	}
	return abs, nil
}

// tailWindow bounds how much of the file TailLines reads: the lines it wants
// are at the end, and a long-running service's log can be max_size_mb long.
const tailWindow = 64 << 10

// TailLines returns up to the last n lines of path, or nil if it cannot be
// read. Used to show why a freshly installed service failed to start. Only the
// last tailWindow bytes are read, less the partial line they start in.
func TailLines(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	skipPartial := false
	if info, err := f.Stat(); err == nil && info.Size() > tailWindow {
		// Start one byte early and drop the first line read: that is the rest
		// of the line the window starts in, or empty when the byte before the
		// window ends a line, so a whole line is never dropped.
		_, err := f.Seek(info.Size()-tailWindow-1, io.SeekStart)
		skipPartial = err == nil
	}
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if skipPartial {
			skipPartial = false
			continue
		}
		lines = append(lines, sc.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	return lines
}
