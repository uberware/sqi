// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package winsvc

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/windows"
)

// These tests drive the service command group through cobra. Only
// TestNewCommand_EverySubcommandRequiresElevationFirst uses the real
// operations, and only in an unelevated process, where every subcommand must
// stop at RequireElevated. The rest inject fake operations, so nothing here
// reaches the SCM, the LSA or %ProgramData%.

const (
	testBinary = "sqi-test"
	testDrain  = 30 * time.Second
)

// fakeOps records every operation the commands call, in order.
type fakeOps struct {
	calls          []string
	installed      ServiceConfig
	existing       bool // what exists reports
	existsErr      error
	validateErr    error
	installErr     error
	startErr       error
	preShutdown    time.Duration
	preShutdownErr error
}

func (f *fakeOps) recordf(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeOps) operations() operations {
	return operations{
		requireElevated: func() error { f.recordf("requireElevated"); return nil },
		exists: func(name string) (bool, error) {
			f.recordf("exists %s", name)
			return f.existing, f.existsErr
		},
		grantLogonRight: func(account string) error { f.recordf("grant %s", account); return nil },
		validateCredentials: func(account, password string) error {
			f.recordf("validate %s %s", account, password)
			return f.validateErr
		},
		ensureLogDir: func(dir, account string) error { f.recordf("ensureLogDir %s %s", dir, account); return nil },
		install: func(sc ServiceConfig) error {
			f.recordf("install %s", sc.Name)
			f.installed = sc
			return f.installErr
		},
		uninstall: func(name string, wait time.Duration) error { f.recordf("uninstall %s %v", name, wait); return nil },
		start: func(name string, wait time.Duration) (StatusInfo, error) {
			f.recordf("start %s %v", name, wait)
			return StatusInfo{Name: name, State: "running", PID: 42}, f.startErr
		},
		stop: func(name string, wait time.Duration) (StatusInfo, error) {
			f.recordf("stop %s %v", name, wait)
			return StatusInfo{Name: name, State: "stopped"}, nil
		},
		status: func(name string) (StatusInfo, error) {
			f.recordf("status %s", name)
			return StatusInfo{
				Name: name, State: "running", PID: 42, Account: "LocalSystem",
				StartType: "automatic (delayed)", BinaryPath: `"C:\sqi\sqi-test.exe" start --config "C:\sqi\sqi-test.yaml"`,
			}, nil
		},
		preShutdownTimeout: func(name string) (time.Duration, error) {
			f.recordf("preShutdownTimeout %s", name)
			return f.preShutdown, f.preShutdownErr
		},
	}
}

// testSpec is a CommandSpec whose DrainTimeout counts its calls.
func testSpec(drainCalls *int) CommandSpec {
	return CommandSpec{
		Binary: testBinary, RunVerb: "start", DisplayName: "sqi Test", Description: "sqi test service",
		DelayedAutoStart: true,
		DrainTimeout: func(string) (time.Duration, error) {
			*drainCalls++
			return testDrain, nil
		},
		UserWarning: "the account also needs a privilege",
	}
}

// execute runs `sqi-test service <args>` on a fresh command tree whose root
// declares the persistent --config/-c flag both binaries define.
func execute(t *testing.T, spec CommandSpec, ops operations, stdin io.Reader, args ...string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: spec.Binary, SilenceUsage: true, SilenceErrors: true}
	var configFile string
	root.PersistentFlags().StringVarP(&configFile, "config", "c", "", "path to config file")
	root.AddCommand(newCommand(spec, ops))
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(stdin)
	root.SetArgs(append([]string{"service"}, args...))
	err := root.Execute()
	return out.String(), err
}

func writeConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), testBinary+".yaml")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// readSpy is stdin that records whether anything read from it.
type readSpy struct{ read bool }

func (r *readSpy) Read([]byte) (int, error) {
	r.read = true
	return 0, io.EOF
}

// TestNewCommand_EverySubcommandRequiresElevationFirst runs every subcommand
// with the real operations in this unelevated process: each must fail with
// ErrNotElevated before reading the config, prompting, printing or calling the
// SCM (whose calls would fail here with a different error).
func TestNewCommand_EverySubcommandRequiresElevationFirst(t *testing.T) {
	if windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("the test process is elevated: these subcommands would really install, start and stop " +
			"a service, so this runs only unelevated (the real-SCM suite covers elevated runs)")
	}
	cfg := writeConfig(t)
	const name = "sqi-test-not-installed"
	for _, args := range [][]string{
		{"install", "--name", name, "--user", `.\sqi-test-nobody`, "--start", "--config", cfg},
		{"uninstall", "--name", name},
		{"start", "--name", name},
		{"stop", "--name", name},
		{"status", "--name", name},
	} {
		t.Run(args[0], func(t *testing.T) {
			var drainCalls int
			stdin := &readSpy{}
			out, err := execute(t, testSpec(&drainCalls), systemOperations(), stdin, args...)
			if !errors.Is(err, ErrNotElevated) {
				t.Fatalf("service %s = %v, want ErrNotElevated", args[0], err)
			}
			if drainCalls != 0 || stdin.read || out != "" {
				t.Errorf("work ran before the elevation check: drain calls %d, stdin read %v, output %q",
					drainCalls, stdin.read, out)
			}
		})
	}
}

// refusingOps is an operations set for a process that is not elevated: the
// elevation check fails, and every other operation fails the test if called.
func refusingOps(t *testing.T) operations {
	t.Helper()
	touched := func(op string) error {
		t.Errorf("%s ran before the elevation check", op)
		return fmt.Errorf("%s must not run", op)
	}
	return operations{
		requireElevated:     func() error { return ErrNotElevated },
		exists:              func(string) (bool, error) { return false, touched("exists") },
		grantLogonRight:     func(string) error { return touched("grantLogonRight") },
		validateCredentials: func(string, string) error { return touched("validateCredentials") },
		ensureLogDir:        func(string, string) error { return touched("ensureLogDir") },
		install:             func(ServiceConfig) error { return touched("install") },
		uninstall:           func(string, time.Duration) error { return touched("uninstall") },
		start:               func(string, time.Duration) (StatusInfo, error) { return StatusInfo{}, touched("start") },
		stop:                func(string, time.Duration) (StatusInfo, error) { return StatusInfo{}, touched("stop") },
		status:              func(string) (StatusInfo, error) { return StatusInfo{}, touched("status") },
		preShutdownTimeout:  func(string) (time.Duration, error) { return 0, touched("preShutdownTimeout") },
	}
}

// TestNewCommand_ElevationCheckedBeforeAnyOperation pins spec §2's "every
// subcommand checks for elevation first" at any elevation, CI's elevated
// runners included: with the check failing, no subcommand may call another
// operation, read its config's drain timeout, read stdin or print.
func TestNewCommand_ElevationCheckedBeforeAnyOperation(t *testing.T) {
	cfg := writeConfig(t)
	for _, args := range [][]string{
		{"install", "--name", "sqi-extra", "--user", "render", "--start", "--config", cfg},
		{"uninstall", "--name", "sqi-extra"},
		{"start", "--name", "sqi-extra"},
		{"stop", "--name", "sqi-extra"},
		{"status", "--name", "sqi-extra"},
	} {
		t.Run(args[0], func(t *testing.T) {
			spec := testSpec(new(int))
			spec.DrainTimeout = func(string) (time.Duration, error) {
				t.Error("DrainTimeout ran before the elevation check")
				return testDrain, nil
			}
			stdin := &readSpy{}
			out, err := execute(t, spec, refusingOps(t), stdin, args...)
			if !errors.Is(err, ErrNotElevated) {
				t.Fatalf("service %s = %v, want ErrNotElevated", args[0], err)
			}
			if stdin.read || out != "" {
				t.Errorf("work ran before the elevation check: stdin read %v, output %q", stdin.read, out)
			}
		})
	}
}

// TestCommandWait pins the stop/uninstall wait: the service's PreShutdown
// timeout plus ShutdownMargin, never below startWait, and startWait when the
// timeout cannot be read.
func TestCommandWait(t *testing.T) {
	for _, tc := range []struct {
		name        string
		preShutdown time.Duration
		err         error
		want        time.Duration
	}{
		{"short timeout keeps the floor", 10 * time.Second, nil, startWait},
		{"default worker timeout reaches the floor", 45 * time.Second, nil, startWait},
		{"long grace period", 10*time.Minute + ShutdownMargin, nil, 10*time.Minute + 2*ShutdownMargin},
		{"unreadable timeout falls back", time.Hour, errors.New("access denied"), startWait},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := commandWait(tc.preShutdown, tc.err); got != tc.want {
				t.Errorf("commandWait(%v, %v) = %v, want %v", tc.preShutdown, tc.err, got, tc.want)
			}
		})
	}
}

// TestInstall_UserAccount pins the --user flow: the account is normalized
// before it reaches any operation, and the logon right is granted before the
// credentials are validated (a service logon needs it), then the log
// directory, then the service.
func TestInstall_UserAccount(t *testing.T) {
	logDir := filepath.Dir(DefaultLogPath(testBinary))
	for _, tc := range []struct{ user, account string }{
		{"render", `.\render`},
		{`.\render`, `.\render`},
		{`STUDIO\render`, `STUDIO\render`},
		{"render@studio.lan", "render@studio.lan"},
	} {
		t.Run(tc.user, func(t *testing.T) {
			var drainCalls int
			fake := &fakeOps{}
			cfg := writeConfig(t)
			out, err := execute(t, testSpec(&drainCalls), fake.operations(), strings.NewReader("s3cret\r\n"),
				"install", "--user", tc.user, "-c", cfg)
			if err != nil {
				t.Fatalf("install: %v\n%s", err, out)
			}
			want := []string{
				"requireElevated",
				"exists " + testBinary,
				"grant " + tc.account,
				"validate " + tc.account + " s3cret",
				"ensureLogDir " + logDir + " " + tc.account,
				"install " + testBinary,
			}
			if !slices.Equal(fake.calls, want) {
				t.Errorf("calls:\n got %q\nwant %q", fake.calls, want)
			}
			sc := fake.installed
			if sc.Account != tc.account || sc.Password != "s3cret" {
				t.Errorf("registered account %q password %q, want %q s3cret", sc.Account, sc.Password, tc.account)
			}
			if wantArgs := []string{"start", "--config", cfg}; !slices.Equal(sc.Args, wantArgs) {
				t.Errorf("args = %q, want %q (the root --config flag)", sc.Args, wantArgs)
			}
			if sc.PreShutdownTimeout != testDrain+ShutdownMargin || !sc.DelayedAutoStart || drainCalls != 1 {
				t.Errorf("preshutdown %v delayed %v drain calls %d", sc.PreShutdownTimeout, sc.DelayedAutoStart, drainCalls)
			}
			for _, s := range []string{
				"warning: the account also needs a privilege",
				"Password for " + tc.account + ": ",
				`installed service "sqi-test"`,
				"account: " + tc.account,
				"start it with: sqi-test service start --name sqi-test",
			} {
				if !strings.Contains(out, s) {
					t.Errorf("output lacks %q:\n%s", s, out)
				}
			}
		})
	}
}

// TestInstall_LocalSystem pins that LocalSystem needs no password, logon right
// or warning.
func TestInstall_LocalSystem(t *testing.T) {
	logDir := filepath.Dir(DefaultLogPath(testBinary))
	for _, extra := range [][]string{nil, {"--user", "LocalSystem"}} {
		t.Run(fmt.Sprint(extra), func(t *testing.T) {
			var drainCalls int
			fake := &fakeOps{}
			stdin := &readSpy{}
			args := append([]string{"install", "--config", writeConfig(t)}, extra...)
			out, err := execute(t, testSpec(&drainCalls), fake.operations(), stdin, args...)
			if err != nil {
				t.Fatalf("install: %v\n%s", err, out)
			}
			want := []string{"requireElevated", "exists " + testBinary, "ensureLogDir " + logDir + " ", "install " + testBinary}
			if !slices.Equal(fake.calls, want) {
				t.Errorf("calls:\n got %q\nwant %q", fake.calls, want)
			}
			if stdin.read || strings.Contains(out, "Password") || strings.Contains(out, "warning") {
				t.Errorf("LocalSystem install prompted or warned (stdin read %v):\n%s", stdin.read, out)
			}
			if !strings.Contains(out, "account: LocalSystem") {
				t.Errorf("output lacks the LocalSystem account:\n%s", out)
			}
		})
	}
}

// TestInstall_InvalidInputChangesNothing pins that everything install can
// reject without touching the system is rejected before the logon right is
// granted or the log directory is touched. An invalid name is rejected before
// the existence probe too.
func TestInstall_InvalidInputChangesNothing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	probed := []string{"requireElevated", "exists " + testBinary}
	for _, tc := range []struct {
		name, stdin, want string
		args              []string
		calls             []string
	}{
		{"missing config", "s3cret\n", "not found", []string{"--config", missing}, probed},
		{"empty password", "\n", "password must not be empty", nil, probed},
		{"no password", "", "read password", nil, probed},
		{"invalid service name", "s3cret\n", "is invalid", []string{"--name", `bad\name`}, []string{"requireElevated"}},
		{"empty service name", "s3cret\n", "is invalid", []string{"--name", ""}, []string{"requireElevated"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var drainCalls int
			fake := &fakeOps{}
			args := append([]string{"install", "--user", "render", "--config", writeConfig(t)}, tc.args...)
			_, err := execute(t, testSpec(&drainCalls), fake.operations(), strings.NewReader(tc.stdin), args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("install = %v, want an error containing %q", err, tc.want)
			}
			if !slices.Equal(fake.calls, tc.calls) {
				t.Errorf("calls = %q, want only %q", fake.calls, tc.calls)
			}
		})
	}
}

// TestInstall_WrongPasswordStopsBeforeInstall pins that a failed credential
// check registers nothing. The logon right granted just before it stays
// granted: the check is a service logon, which needs the right.
func TestInstall_WrongPasswordStopsBeforeInstall(t *testing.T) {
	var drainCalls int
	fake := &fakeOps{validateErr: windows.ERROR_LOGON_FAILURE}
	_, err := execute(t, testSpec(&drainCalls), fake.operations(), strings.NewReader("wrong\n"),
		"install", "--user", "render", "--config", writeConfig(t))
	if !errors.Is(err, windows.ERROR_LOGON_FAILURE) || !strings.Contains(err.Error(), "check the password") {
		t.Fatalf("install = %v, want the logon failure and a password hint", err)
	}
	want := []string{"requireElevated", "exists " + testBinary, `grant .\render`, `validate .\render wrong`}
	if !slices.Equal(fake.calls, want) {
		t.Errorf("calls:\n got %q\nwant %q", fake.calls, want)
	}
}

// TestInstall_ExistingServiceRefusedBeforeAnySideEffect pins spec §2's
// refusal of an existing service: it comes straight after the elevation
// check, before the password prompt, the logon right, the credential check
// (a wrong password would count toward the account's lockout) and the shared
// log directory's ACL. A probe that fails for another reason stops install
// the same way.
func TestInstall_ExistingServiceRefusedBeforeAnySideEffect(t *testing.T) {
	probeErr := fmt.Errorf("open service sqi-extra: %w", windows.ERROR_ACCESS_DENIED)
	for _, tc := range []struct {
		name     string
		fake     *fakeOps
		want     error
		wantHint bool
	}{
		{"exists", &fakeOps{existing: true}, ErrServiceExists, true},
		{"probe fails", &fakeOps{existsErr: probeErr}, windows.ERROR_ACCESS_DENIED, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdin := &readSpy{}
			out, err := execute(t, testSpec(new(int)), tc.fake.operations(), stdin,
				"install", "--name", "sqi-extra", "--user", "render", "--start", "--config", writeConfig(t))
			if !errors.Is(err, tc.want) {
				t.Fatalf("install = %v, want %v", err, tc.want)
			}
			if hint := strings.Contains(fmt.Sprint(err), "sqi-test service uninstall --name sqi-extra"); hint != tc.wantHint {
				t.Errorf("uninstall hint present = %v, want %v: %v", hint, tc.wantHint, err)
			}
			if want := []string{"requireElevated", "exists sqi-extra"}; !slices.Equal(tc.fake.calls, want) {
				t.Errorf("calls:\n got %q\nwant %q", tc.fake.calls, want)
			}
			if stdin.read || out != "" {
				t.Errorf("install prompted or printed before refusing (stdin read %v): %q", stdin.read, out)
			}
		})
	}
}

// TestInstall_DisplayNameForAnotherInstance pins that a second instance gets a
// display name of its own: the SCM refuses a duplicate display name
// (ERROR_DUPLICATE_SERVICE_NAME), so the default one cannot be reused. An
// explicit --display-name always wins.
func TestInstall_DisplayNameForAnotherInstance(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"default instance", nil, "sqi Test"},
		{"default name given explicitly", []string{"--name", testBinary}, "sqi Test"},
		{"default name in another case", []string{"--name", "SQI-Test"}, "sqi Test"},
		{"another instance", []string{"--name", "sqi-test-2"}, "sqi Test (sqi-test-2)"},
		{"explicit display name", []string{"--name", "sqi-test-2", "--display-name", "Render node 2"}, "Render node 2"},
		{"explicit default display name", []string{"--name", "sqi-test-2", "--display-name", "sqi Test"}, "sqi Test"},
		{"explicit display name, default instance", []string{"--display-name", "Render node"}, "Render node"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeOps{}
			args := append([]string{"install", "--config", writeConfig(t)}, tc.args...)
			if out, err := execute(t, testSpec(new(int)), fake.operations(), &readSpy{}, args...); err != nil {
				t.Fatalf("install: %v\n%s", err, out)
			}
			if got := fake.installed.DisplayName; got != tc.want {
				t.Errorf("display name = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestInstall_ExistingServicePointsAtUninstall covers the race the probe
// cannot: a service created between the probe and Install, which Install
// reports as ErrServiceExists.
func TestInstall_ExistingServicePointsAtUninstall(t *testing.T) {
	var drainCalls int
	fake := &fakeOps{installErr: fmt.Errorf("%w: sqi-extra", ErrServiceExists)}
	_, err := execute(t, testSpec(&drainCalls), fake.operations(), &readSpy{},
		"install", "--name", "sqi-extra", "--config", writeConfig(t))
	if !errors.Is(err, ErrServiceExists) {
		t.Fatalf("install = %v, want ErrServiceExists", err)
	}
	if !strings.Contains(err.Error(), "sqi-test service uninstall --name sqi-extra") {
		t.Errorf("error does not point at service uninstall: %v", err)
	}
}

func TestInstall_StartWaitsForRunning(t *testing.T) {
	var drainCalls int
	fake := &fakeOps{}
	out, err := execute(t, testSpec(&drainCalls), fake.operations(), &readSpy{},
		"install", "--start", "--config", writeConfig(t))
	if err != nil {
		t.Fatalf("install --start: %v\n%s", err, out)
	}
	if last := fake.calls[len(fake.calls)-1]; last != fmt.Sprintf("start %s %v", testBinary, startWait) {
		t.Errorf("last call = %q, want a start waiting %v", last, startWait)
	}
	if !strings.Contains(out, `service "sqi-test" is running (pid 42)`) {
		t.Errorf("output lacks the running state:\n%s", out)
	}
}

// TestStartAndReport_FailurePrintsLogTail pins spec §2: a service that does
// not start has the tail of its log printed.
func TestStartAndReport_FailurePrintsLogTail(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "sqi-test.log")
	var log strings.Builder
	for i := 1; i <= tailCount+5; i++ {
		fmt.Fprintf(&log, "line %d\n", i)
	}
	if err := os.WriteFile(logPath, []byte(log.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	startErr := errors.New("service sqi-test stopped during startup")
	fake := &fakeOps{startErr: startErr}
	var out bytes.Buffer
	err := commands{ops: fake.operations()}.startAndReport(&out, testBinary, logPath)
	if !errors.Is(err, startErr) {
		t.Fatalf("startAndReport = %v, want the start error", err)
	}
	got := out.String()
	for _, s := range []string{`service "sqi-test" did not start`, "last lines of " + logPath, "  line 6\n", "  line 25\n"} {
		if !strings.Contains(got, s) {
			t.Errorf("output lacks %q:\n%s", s, got)
		}
	}
	if strings.Contains(got, "line 5\n") {
		t.Errorf("output has more than the last %d lines:\n%s", tailCount, got)
	}
}

// TestStopAndUninstall_WaitForPreShutdownTimeout pins spec §2: stop and
// uninstall wait up to the service's own PreShutdown timeout.
func TestStopAndUninstall_WaitForPreShutdownTimeout(t *testing.T) {
	for _, sub := range []string{"stop", "uninstall"} {
		for _, tc := range []struct {
			name        string
			preShutdown time.Duration
			err         error
			want        time.Duration
		}{
			{"configured", 10 * time.Minute, nil, 10*time.Minute + ShutdownMargin},
			{"unreadable", 10 * time.Minute, ErrServiceNotFound, startWait},
		} {
			t.Run(sub+" "+tc.name, func(t *testing.T) {
				var drainCalls int
				fake := &fakeOps{preShutdown: tc.preShutdown, preShutdownErr: tc.err}
				out, err := execute(t, testSpec(&drainCalls), fake.operations(), &readSpy{}, sub, "--name", "sqi-extra")
				if err != nil {
					t.Fatalf("%s: %v", sub, err)
				}
				want := []string{"requireElevated", "preShutdownTimeout sqi-extra", fmt.Sprintf("%s sqi-extra %v", sub, tc.want)}
				if !slices.Equal(fake.calls, want) {
					t.Errorf("calls:\n got %q\nwant %q", fake.calls, want)
				}
				if out == "" {
					t.Error("no outcome reported")
				}
			})
		}
	}
}

func TestStatus_PrintsStateAndRegistration(t *testing.T) {
	var drainCalls int
	fake := &fakeOps{}
	out, err := execute(t, testSpec(&drainCalls), fake.operations(), &readSpy{}, "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, s := range []string{
		"name:       sqi-test\n",
		"state:      running\n",
		"pid:        42\n",
		"exit codes: 0 (service-specific 0)\n",
		"account:    LocalSystem\n",
		"start type: automatic (delayed)\n",
		`command:    "C:\sqi\sqi-test.exe" start --config "C:\sqi\sqi-test.yaml"` + "\n",
		"log:        " + DefaultLogPath(testBinary),
	} {
		if !strings.Contains(out, s) {
			t.Errorf("status output lacks %q:\n%s", s, out)
		}
	}
}
