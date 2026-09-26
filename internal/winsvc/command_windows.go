// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package winsvc

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/uberware/sqi/internal/secretin"
)

const (
	// startWait bounds how long start and install --start wait for the
	// service to run (Start then watches it for startSettle more), and is the
	// least that stop and uninstall wait for it to stop.
	startWait = 60 * time.Second
	// tailCount is how many lines of the service log a failed start prints.
	tailCount = 20
)

// CommandSpec describes one binary's `service` command group.
type CommandSpec struct {
	Binary           string // "sqi-server" / "sqi-worker": default service name and config file stem
	RunVerb          string // "serve" / "start"
	DisplayName      string
	Description      string
	DelayedAutoStart bool
	// DrainTimeout returns the binary's graceful-shutdown bound for the given
	// config file; the PreShutdown timeout is this plus ShutdownMargin.
	DrainTimeout func(configPath string) (time.Duration, error)
	// UserWarning, when non-empty, is printed if --user names an account.
	UserWarning string
}

// operations are the side-effecting calls the service commands make. The
// commands reach the SCM, the LSA and the log directory only through them, so
// the order of the calls, their arguments and the error mapping are tested
// without an SCM (command_windows_test.go). systemOperations wires the real
// ones.
type operations struct {
	requireElevated     func() error
	exists              func(name string) (bool, error)
	grantLogonRight     func(account string) error
	validateCredentials func(account, password string) error
	ensureLogDir        func(dir, account string) error
	install             func(ServiceConfig) error
	uninstall           func(name string, wait time.Duration) error
	start               func(name string, wait time.Duration) (StatusInfo, error)
	stop                func(name string, wait time.Duration) (StatusInfo, error)
	status              func(name string) (StatusInfo, error)
	preShutdownTimeout  func(name string) (time.Duration, error)
}

func systemOperations() operations {
	return operations{
		requireElevated:     RequireElevated,
		exists:              Exists,
		grantLogonRight:     GrantServiceLogonRight,
		validateCredentials: ValidateCredentials,
		ensureLogDir:        EnsureLogDir,
		install:             Install,
		uninstall:           Uninstall,
		start:               Start,
		stop:                Stop,
		status:              Status,
		preShutdownTimeout:  PreShutdownTimeout,
	}
}

// commands builds one binary's `service` subcommands.
type commands struct {
	spec CommandSpec
	ops  operations
}

// NewCommand returns the `service` command group for spec.
func NewCommand(spec CommandSpec) *cobra.Command {
	return newCommand(spec, systemOperations())
}

func newCommand(spec CommandSpec, ops operations) *cobra.Command {
	c := commands{spec: spec, ops: ops}
	root := &cobra.Command{
		Use:   "service",
		Short: "Install and control " + spec.Binary + " as a Windows service",
		Long: `Install and control ` + spec.Binary + ` as a Windows service.

Every subcommand needs an elevated (Administrator) shell. --name selects the
service (default "` + spec.Binary + `"), so several instances can coexist.`,
	}
	root.AddCommand(c.installCmd(), c.uninstallCmd(), c.startCmd(), c.stopCmd(), c.statusCmd())
	return root
}

// elevated makes the elevation check the first thing a subcommand does, ahead
// of reading its config, prompting, printing or calling the SCM.
func (c commands) elevated(run func(cmd *cobra.Command) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		if err := c.ops.requireElevated(); err != nil {
			return err
		}
		return run(cmd)
	}
}

func (c commands) nameFlag(cmd *cobra.Command, name *string) {
	cmd.Flags().StringVar(name, "name", c.spec.Binary, "service name")
}

type installFlags struct {
	name, displayName, user string
	start                   bool
}

func (c commands) installCmd() *cobra.Command {
	var f installFlags
	sub := &cobra.Command{
		Use:   "install",
		Short: "Register " + c.spec.Binary + " as an automatic-start Windows service",
		Long: `Register ` + c.spec.Binary + ` as an automatic-start Windows service.

The config file (--config, default %ProgramData%\sqi\` + c.spec.Binary + `.yaml) must
exist; its absolute path is baked into the service's command line. The service
runs as LocalSystem unless --user is given. With --user the password is
prompted for (or read from piped stdin), the account is granted "Log on as a
service", and the password is then checked with a service logon. That check
needs the right, so it is granted first, and a wrong password leaves it
granted.`,
		Args: cobra.NoArgs,
		RunE: c.elevated(func(cmd *cobra.Command) error { return c.runInstall(cmd, f) }),
	}
	c.nameFlag(sub, &f.name)
	sub.Flags().StringVar(&f.displayName, "display-name", c.spec.DisplayName,
		`service display name; another --name defaults to "`+c.spec.DisplayName+` (<name>)"`)
	sub.Flags().StringVar(&f.user, "user", "",
		`account to run as: DOMAIN\name or .\name (default LocalSystem); the password is prompted for`)
	sub.Flags().BoolVar(&f.start, "start", false, "start the service after installing it and wait for it to run")
	return sub
}

// configFlag reads the binary's persistent root --config flag (both binaries
// define it); install does not declare its own, which would shadow it.
func configFlag(cmd *cobra.Command) string {
	if fl := cmd.Flag("config"); fl != nil {
		return fl.Value.String()
	}
	return ""
}

// runInstall registers the service. The order is fixed: the name and the
// existence probe (refuseExisting), everything else that can be checked
// without changing the system (installConfig), then the account's logon right
// and password, the log directory, and the service.
func (c commands) runInstall(cmd *cobra.Command, f installFlags) error {
	if err := c.refuseExisting(f.name); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	sc, err := c.installConfig(cmd, f)
	if err != nil {
		return err
	}
	if err := c.grantAndValidate(sc.Account, sc.Password); err != nil {
		return err
	}
	logPath := DefaultLogPath(sc.Name)
	if err := c.ops.ensureLogDir(filepath.Dir(logPath), sc.Account); err != nil {
		return err
	}
	if err := c.ops.install(sc); err != nil {
		if errors.Is(err, ErrServiceExists) { // created since refuseExisting probed
			return c.existsError(err, sc.Name)
		}
		return err
	}
	fmt.Fprintf(out, "installed service %q\n  command: %s\n  account: %s\n  log:     %s (unless log.file is set)\n",
		sc.Name, commandLine(sc), accountLabel(sc.Account), logPath)
	if !f.start {
		fmt.Fprintf(out, "start it with: %s service start --name %s\n", c.spec.Binary, sc.Name)
		return nil
	}
	return c.startAndReport(out, sc.Name, logPath)
}

// refuseExisting rejects an invalid name, or one a service already has (spec
// §2: no overwrite), before install prompts for a password or changes
// anything. Otherwise the logon right, a credential check (a wrong password
// counts toward the account's lockout) and an ACE on the log directory every
// sqi service shares would all come first. Install's own probe still catches
// a service created after this one.
func (c commands) refuseExisting(name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	exists, err := c.ops.exists(name)
	if err != nil {
		return err
	}
	if exists {
		return c.existsError(fmt.Errorf("%w: %s", ErrServiceExists, name), name)
	}
	return nil
}

// existsError points an ErrServiceExists at `service uninstall`.
func (c commands) existsError(err error, name string) error {
	return fmt.Errorf("%w; remove it first with: %s service uninstall --name %s", err, c.spec.Binary, name)
}

// displayName is the display name to register. An explicit --display-name
// wins. Otherwise an instance other than the default gets "<default> (<name>)":
// the SCM refuses a display name another service already has
// (ERROR_DUPLICATE_SERVICE_NAME). Service names are case-insensitive, so
// "SQI-Worker" is the default instance.
func (c commands) displayName(name, flagValue string, explicit bool) string {
	if explicit || strings.EqualFold(name, c.spec.Binary) {
		return flagValue
	}
	return c.spec.DisplayName + " (" + name + ")"
}

// installConfig resolves the config file, the drain timeout, the account and
// its password, and validates the registration they make, without changing
// anything on the system.
func (c commands) installConfig(cmd *cobra.Command, f installFlags) (ServiceConfig, error) {
	cfgPath, err := ResolveConfigPath(configFlag(cmd), c.spec.Binary)
	if err != nil {
		return ServiceConfig{}, err
	}
	exe, err := os.Executable()
	if err != nil {
		return ServiceConfig{}, fmt.Errorf("locate %s: %w", c.spec.Binary, err)
	}
	drain, err := c.spec.DrainTimeout(cfgPath)
	if err != nil {
		return ServiceConfig{}, fmt.Errorf("read shutdown timeout from %s: %w", cfgPath, err)
	}
	// Normalized before anything sees it: a bare name would reach LogonUserW
	// with no domain, which it reads as a UPN.
	account := NormalizeAccount(f.user)
	password, err := c.readPassword(cmd, account)
	if err != nil {
		return ServiceConfig{}, err
	}
	return BuildServiceConfig(InstallSpec{
		Name:             f.name,
		DisplayName:      c.displayName(f.name, f.displayName, cmd.Flags().Changed("display-name")),
		Description:      c.spec.Description,
		ExePath:          exe,
		RunVerb:          c.spec.RunVerb,
		ConfigPath:       cfgPath,
		Account:          account,
		Password:         password,
		DelayedAutoStart: c.spec.DelayedAutoStart,
		DrainTimeout:     drain,
	})
}

// readPassword prompts for account's password, or reads it from piped stdin.
// For LocalSystem ("") it reads nothing and returns "".
func (c commands) readPassword(cmd *cobra.Command, account string) (string, error) {
	if account == "" {
		return "", nil
	}
	out := cmd.OutOrStdout()
	if c.spec.UserWarning != "" {
		fmt.Fprintf(out, "warning: %s\n", c.spec.UserWarning)
	}
	fmt.Fprintf(out, "Password for %s: ", account)
	password, err := secretin.ReadLine(cmd)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	if password == "" {
		return "", errors.New("password must not be empty")
	}
	return password, nil
}

// grantAndValidate grants account "Log on as a service", then proves the
// password with a service logon. The logon needs the right, so the grant comes
// first, and a wrong password leaves the right granted: nothing is rolled
// back. LocalSystem ("") needs neither.
func (c commands) grantAndValidate(account, password string) error {
	if account == "" {
		return nil
	}
	if err := c.ops.grantLogonRight(account); err != nil {
		return fmt.Errorf("grant %s the 'Log on as a service' right: %w", account, err)
	}
	if err := c.ops.validateCredentials(account, password); err != nil {
		return fmt.Errorf("%w (check the password; a domain policy that defines 'Log on as a service' can also remove the right just granted)", err)
	}
	return nil
}

// commandLine renders the service's command line the way mgr.CreateService
// registers it.
func commandLine(sc ServiceConfig) string {
	parts := make([]string, 0, 1+len(sc.Args))
	parts = append(parts, syscall.EscapeArg(sc.ExePath))
	for _, a := range sc.Args {
		parts = append(parts, syscall.EscapeArg(a))
	}
	return strings.Join(parts, " ")
}

func accountLabel(account string) string {
	if account == "" {
		return "LocalSystem"
	}
	return account
}

// startAndReport starts name and waits for it to run or stop. A service that
// does not start has the tail of its log printed, since that is where it
// wrote why.
func (c commands) startAndReport(out io.Writer, name, logPath string) error {
	info, err := c.ops.start(name, startWait)
	if err != nil {
		fmt.Fprintf(out, "service %q did not start: %v\n", name, err)
		if lines := TailLines(logPath, tailCount); len(lines) > 0 {
			fmt.Fprintf(out, "last lines of %s:\n", logPath)
			for _, l := range lines {
				fmt.Fprintf(out, "  %s\n", l)
			}
		}
		return err
	}
	fmt.Fprintf(out, "service %q is %s (pid %d)\n", name, info.State, info.PID)
	return nil
}

// stopWait is how long stop and uninstall wait for name to stop.
func (c commands) stopWait(name string) time.Duration {
	return commandWait(c.ops.preShutdownTimeout(name))
}

// commandWait is how long stop and uninstall wait for a service to stop: its
// PreShutdown timeout (spec §2) plus ShutdownMargin for the process to exit,
// and never less than startWait. When the timeout could not be read it is
// startWait; that is no reason to fail the command.
func commandWait(preShutdown time.Duration, queryErr error) time.Duration {
	if queryErr != nil {
		return startWait
	}
	return max(startWait, preShutdown+ShutdownMargin)
}

func (c commands) uninstallCmd() *cobra.Command {
	var name string
	sub := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the service (config, data and logs are kept)",
		Args:  cobra.NoArgs,
		RunE: c.elevated(func(cmd *cobra.Command) error {
			if err := c.ops.uninstall(name, c.stopWait(name)); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed service %q\n", name)
			return nil
		}),
	}
	c.nameFlag(sub, &name)
	return sub
}

func (c commands) startCmd() *cobra.Command {
	var name string
	sub := &cobra.Command{
		Use:   "start",
		Short: "Start the service and wait for it to run",
		Args:  cobra.NoArgs,
		RunE: c.elevated(func(cmd *cobra.Command) error {
			return c.startAndReport(cmd.OutOrStdout(), name, DefaultLogPath(name))
		}),
	}
	c.nameFlag(sub, &name)
	return sub
}

func (c commands) stopCmd() *cobra.Command {
	var name string
	sub := &cobra.Command{
		Use:   "stop",
		Short: "Stop the service, letting it drain in-flight work",
		Args:  cobra.NoArgs,
		RunE: c.elevated(func(cmd *cobra.Command) error {
			info, err := c.ops.stop(name, c.stopWait(name))
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "service %q is %s\n", name, info.State)
			return nil
		}),
	}
	c.nameFlag(sub, &name)
	return sub
}

func (c commands) statusCmd() *cobra.Command {
	var name string
	sub := &cobra.Command{
		Use:   "status",
		Short: "Show the service's state and registration",
		Args:  cobra.NoArgs,
		RunE: c.elevated(func(cmd *cobra.Command) error {
			info, err := c.ops.status(name)
			if err != nil {
				return err
			}
			printStatus(cmd.OutOrStdout(), info, DefaultLogPath(name))
			return nil
		}),
	}
	c.nameFlag(sub, &name)
	return sub
}

func printStatus(out io.Writer, info StatusInfo, logPath string) {
	fmt.Fprintf(out, "name:       %s\nstate:      %s\n", info.Name, info.State)
	if info.PID != 0 {
		fmt.Fprintf(out, "pid:        %d\n", info.PID)
	}
	fmt.Fprintf(out, "exit codes: %d (service-specific %d)\naccount:    %s\nstart type: %s\ncommand:    %s\nlog:        %s (unless log.file is set)\n",
		info.Win32ExitCode, info.ServiceExitCode, accountLabel(info.Account), info.StartType,
		info.BinaryPath, logPath)
}
