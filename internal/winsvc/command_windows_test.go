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
	sqiDirErr      error     // what ensureSqiDir returns
	checkErr       error     // what checkPaths returns
	checked        []PathRef // the refs checkPaths was given
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
		ensureSqiDir: func() error { f.recordf("ensureSqiDir"); return f.sqiDirErr },
		checkPaths: func(refs []PathRef, account string) error {
			f.recordf("checkPaths %s", account)
			f.checked = refs
			return f.checkErr
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

// testSpec is a CommandSpec whose DrainTimeout counts its calls and whose
// configuration names no paths.
func testSpec(drainCalls *int) CommandSpec {
	return CommandSpec{
		Binary: testBinary, RunVerb: "start", DisplayName: "sqi Test", Description: "sqi test service",
		DelayedAutoStart: true,
		DrainTimeout: func(string) (time.Duration, error) {
			*drainCalls++
			return testDrain, nil
		},
		ConfigPaths: func(string, string) (ConfigPaths, error) { return ConfigPaths{}, nil },
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
		ensureSqiDir:        func() error { return touched("ensureSqiDir") },
		checkPaths:          func([]PathRef, string) error { return touched("checkPaths") },
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

// TestNewCommand_ElevationCheckedBeforeAnyOperation pins "every subcommand
// checks for elevation first" at any elevation, CI's elevated
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
				"ensureSqiDir",
				"checkPaths " + tc.account,
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
			want := []string{
				"requireElevated", "exists " + testBinary, "ensureSqiDir", "checkPaths ",
				"ensureLogDir " + logDir + " ", "install " + testBinary,
			}
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
	checked := append(slices.Clone(probed), "ensureSqiDir", `checkPaths .\render`)
	for _, tc := range []struct {
		name, stdin, want string
		args              []string
		calls             []string
	}{
		{"missing config", "s3cret\n", "not found", []string{"--config", missing}, probed},
		{"empty password", "\n", "password must not be empty", nil, checked},
		{"no password", "", "read password", nil, checked},
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
	want := []string{
		"requireElevated", "exists " + testBinary, "ensureSqiDir", `checkPaths .\render`,
		`grant .\render`, `validate .\render wrong`,
	}
	if !slices.Equal(fake.calls, want) {
		t.Errorf("calls:\n got %q\nwant %q", fake.calls, want)
	}
}

// TestInstall_ExistingServiceRefusedBeforeAnySideEffect pins the refusal of
// an existing service: it comes straight after the elevation
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

// TestStartAndReport_FailurePrintsLogTail pins that a service that does
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

// TestStopAndUninstall_WaitForPreShutdownTimeout pins that stop and
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

// recordingStdin records "readPassword" in f's calls when it is first read.
type recordingStdin struct {
	f    *fakeOps
	r    io.Reader
	read bool
}

func (s *recordingStdin) Read(p []byte) (int, error) {
	if !s.read {
		s.read = true
		s.f.recordf("readPassword")
	}
	return s.r.Read(p)
}

// TestInstall_ChecksPathsBeforePasswordAndChanges pins where the path check
// sits: after the existence probe and the creation of %ProgramData%\sqi
// (without which a fresh host's default log directory would be judged by
// %ProgramData% itself, which every user can write), and before the password
// is read, the logon right granted, the log directory touched or the service
// registered. A refusal, a configuration whose paths cannot be read, or a
// %ProgramData%\sqi that cannot be created stops install there.
func TestInstall_ChecksPathsBeforePasswordAndChanges(t *testing.T) {
	refused := &UntrustedPathsError{Failures: []PathFailure{
		{Key: "nats.tls_ca_file", Path: `C:\ca.pem`, JudgedPath: `C:\ca.pem`, Reason: `BUILTIN\Users can write (WD)`},
	}}
	ioErr := fmt.Errorf("read security: %w", windows.ERROR_ACCESS_DENIED)
	prefix := []string{"requireElevated", "exists " + testBinary, "ensureSqiDir", `checkPaths .\render`}
	sqiDirErr := fmt.Errorf("create C:\\ProgramData\\sqi: %w", windows.ERROR_ACCESS_DENIED)
	for _, tc := range []struct {
		name      string
		sqiDirErr error
		checkErr  error
		pathsErr  error
		want      []string
	}{
		{"passes", nil, nil, nil, append(slices.Clone(prefix), "readPassword", `grant .\render`, `validate .\render s3cret`,
			`ensureLogDir `+filepath.Dir(DefaultLogPath(testBinary))+` .\render`, "install "+testBinary)},
		{"refused", nil, refused, nil, prefix},
		{"unreadable path", nil, ioErr, nil, prefix},
		{"sqi directory not created", sqiDirErr, nil, nil, prefix[:3]},
		{"unreadable configuration", nil, nil, errors.New("line 2: unknown key log.fil"), prefix[:2]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeOps{checkErr: tc.checkErr, sqiDirErr: tc.sqiDirErr}
			spec := testSpec(new(int))
			spec.ConfigPaths = func(string, string) (ConfigPaths, error) { return ConfigPaths{}, tc.pathsErr }
			stdin := &recordingStdin{f: fake, r: strings.NewReader("s3cret\n")}
			_, err := execute(t, spec, fake.operations(), stdin, "install", "--user", "render", "--config", writeConfig(t))
			if !slices.Equal(fake.calls, tc.want) {
				t.Errorf("calls:\n got %q\nwant %q", fake.calls, tc.want)
			}
			switch {
			case tc.sqiDirErr != nil:
				if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
					t.Errorf("install = %v, want the ensureSqiDir error", err)
				}
			case tc.checkErr == nil && tc.pathsErr == nil:
				if err != nil {
					t.Fatalf("install: %v", err)
				}
			case tc.pathsErr != nil:
				if err == nil || !strings.Contains(err.Error(), tc.pathsErr.Error()) {
					t.Errorf("install = %v, want the configuration error", err)
				}
			default:
				if !errors.Is(err, tc.checkErr) {
					t.Errorf("install = %v, want %v", err, tc.checkErr)
				}
			}
		})
	}
}

// TestInstall_CheckedRefs pins the paths install checks, in order: the
// configuration and its directory, the binary and its directory, log.file's
// directory and its parent when it is set, the service's own default log
// directory and its parent, the shared logs directory (always: install
// prepares it and the service's trace log goes there), then every path the
// configuration names. A default log directory already checked under another
// key is not checked again.
func TestInstall_CheckedRefs(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pd := t.TempDir()
	t.Setenv("ProgramData", pd)
	logs := filepath.Join(pd, "sqi", "logs")
	defaultLogDir := filepath.Join(logs, testBinary)
	defaults := []PathRef{{"default log directory", defaultLogDir}, {"default log directory parent", logs}}
	for _, tc := range []struct {
		name          string
		cfgDir        string // "" for a directory of its own
		paths         ConfigPaths
		logDir, named []PathRef
	}{
		{
			name:   "log.file unset",
			paths:  ConfigPaths{Paths: []PathRef{{"nats.tls_ca_file", `E:\pki\ca.pem`}}},
			logDir: defaults,
			named:  []PathRef{{"nats.tls_ca_file", `E:\pki\ca.pem`}},
		},
		{
			name: "log.file set",
			paths: ConfigPaths{LogFile: `D:\logs\sqi\w.log`, Paths: []PathRef{
				{"worker.data_dir", `E:\sqi\data`}, {"log.file", `D:\logs\sqi\w.log`},
			}},
			logDir: append([]PathRef{{"log directory", `D:\logs\sqi`}, {"log directory parent", `D:\logs`}}, defaults...),
			named:  []PathRef{{"worker.data_dir", `E:\sqi\data`}, {"log.file", `D:\logs\sqi\w.log`}},
		},
		{
			name:   "log.file in the default log directory",
			paths:  ConfigPaths{LogFile: filepath.Join(defaultLogDir, "w.log")},
			logDir: []PathRef{{"log directory", defaultLogDir}, {"log directory parent", logs}},
		},
		{
			name:   "configuration in the default log directory's parent",
			cfgDir: logs,
			logDir: defaults[:1],
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := writeConfig(t)
			if tc.cfgDir != "" {
				if err := os.MkdirAll(tc.cfgDir, 0o700); err != nil {
					t.Fatal(err)
				}
				cfg = filepath.Join(tc.cfgDir, testBinary+".yaml")
				if err := os.WriteFile(cfg, []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			fake := &fakeOps{}
			spec := testSpec(new(int))
			var loaded string
			spec.ConfigPaths = func(p, _ string) (ConfigPaths, error) { loaded = p; return tc.paths, nil }
			if out, err := execute(t, spec, fake.operations(), &readSpy{}, "install", "--config", cfg); err != nil {
				t.Fatalf("install: %v\n%s", err, out)
			}
			if loaded != cfg {
				t.Errorf("ConfigPaths loaded %q, want %q", loaded, cfg)
			}
			want := []PathRef{
				{"config", cfg},
				{"config directory", filepath.Dir(cfg)},
				{"binary", exe},
				{"binary directory", filepath.Dir(exe)},
			}
			want = append(append(want, tc.logDir...), tc.named...)
			if !slices.Equal(fake.checked, want) {
				t.Errorf("checked refs:\n got %q\nwant %q", fake.checked, want)
			}
		})
	}
}

// TestServiceInstall_ResolvesRelativePathsAgainstConfigDir pins that a
// relative configured path is checked where the service will find it: under
// the configuration's directory, which the service runs in, not the
// installing shell's working directory.
func TestServiceInstall_ResolvesRelativePathsAgainstConfigDir(t *testing.T) {
	cfg := writeConfig(t)
	dir := filepath.Dir(cfg)
	t.Chdir(t.TempDir())
	fake := &fakeOps{}
	spec := testSpec(new(int))
	spec.ConfigPaths = func(string, string) (ConfigPaths, error) {
		return ConfigPaths{LogFile: `logs\w.log`, Paths: []PathRef{
			{"log.file", `logs\w.log`}, {"worker.data_dir", "data"},
		}}, nil
	}
	if out, err := execute(t, spec, fake.operations(), &readSpy{}, "install", "--config", cfg); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	defaultLogDir := filepath.Dir(DefaultLogPath(testBinary))
	want := []PathRef{
		{"log directory", filepath.Join(dir, "logs")},
		{"log directory parent", dir},
		{"default log directory", defaultLogDir},
		{"default log directory parent", filepath.Dir(defaultLogDir)},
		{"log.file", filepath.Join(dir, "logs", "w.log")},
		{"worker.data_dir", filepath.Join(dir, "data")},
	}
	if len(fake.checked) < 4 {
		t.Fatalf("checked refs = %q", fake.checked)
	}
	if got := fake.checked[4:]; !slices.Equal(got, want) {
		t.Errorf("checked refs after the binary's:\n got %q\nwant %q", got, want)
	}
}

// TestInstall_RefusesUnsetWorkerDataDir pins that an error from ConfigPaths
// (the worker's for a configuration that leaves worker.data_dir unset) is
// returned as it is, before the password prompt and before any operation
// after the existence probe.
func TestInstall_RefusesUnsetWorkerDataDir(t *testing.T) {
	unset := errors.New("worker.data_dir is not set in the configuration; set it explicitly")
	fake := &fakeOps{}
	spec := testSpec(new(int))
	var account string
	spec.ConfigPaths = func(_, a string) (ConfigPaths, error) { account = a; return ConfigPaths{}, unset }
	stdin := &readSpy{}
	out, err := execute(t, spec, fake.operations(), stdin, "install", "--user", "render", "--config", writeConfig(t))
	if err != unset { //nolint:errorlint // returned as it is, not wrapped
		t.Fatalf("install = %v, want the ConfigPaths error unchanged", err)
	}
	if account != `.\render` {
		t.Errorf("ConfigPaths was given account %q, want the normalized .\\render", account)
	}
	if want := []string{"requireElevated", "exists " + testBinary}; !slices.Equal(fake.calls, want) {
		t.Errorf("calls = %q, want only %q", fake.calls, want)
	}
	if stdin.read || out != "" {
		t.Errorf("install prompted or printed before refusing (stdin read %v): %q", stdin.read, out)
	}
}

// TestResolveAgainst pins how a configured path resolves for a service whose
// working directory is base: a rooted path takes base's drive, and a path
// relative to another drive's current directory resolves from that drive's
// root, since a service has no current directory there.
func TestResolveAgainst(t *testing.T) {
	const base = `C:\ProgramData\sqi`
	for _, tc := range []struct{ path, want string }{
		{`E:\sqi\data`, `E:\sqi\data`},
		{`data`, `C:\ProgramData\sqi\data`},
		{`..\x\y`, `C:\ProgramData\x\y`},
		{`logs/w.log`, `C:\ProgramData\sqi\logs\w.log`},
		{`\sqi\data`, `C:\sqi\data`},
		{`/sqi/data`, `C:\sqi\data`},
		{`C:data`, `C:\ProgramData\sqi\data`},
		{`D:data`, `D:\data`},
		{`\\server\share\sqi`, `\\server\share\sqi`},
	} {
		if got := resolveAgainst(base, tc.path); got != tc.want {
			t.Errorf("resolveAgainst(%q, %q) = %q, want %q", base, tc.path, got, tc.want)
		}
	}
}

// TestInstall_RefusalNamesRemedy pins the way out a refusal offers: a binary
// an ordinary user can change is copied into an administrators-only
// directory from an elevated PowerShell; a path under %ProgramData% is moved
// aside, and a configuration recreated with config init; anything else is
// for the operator to fix. The *UntrustedPathsError stays reachable.
func TestInstall_RefusalNamesRemedy(t *testing.T) {
	pd := t.TempDir()
	t.Setenv("ProgramData", pd)
	sqiDir := filepath.Join(pd, "sqi")
	if err := os.Mkdir(sqiDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(sqiDir, testBinary+".yaml")
	if err := os.WriteFile(cfg, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(sqiDir, "ca.pem")
	if err := os.WriteFile(caFile, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	programFiles := os.Getenv("ProgramFiles")
	if programFiles == "" {
		programFiles = `C:\Program Files`
	}
	binaryRemedy := []string{"elevated PowerShell", "Git Bash", "'" + filepath.Join(programFiles, "sqi") + "'", "Copy-Item -LiteralPath '" + exe + "'"}
	moveRemedy := []string{
		"Rename-Item -LiteralPath '" + sqiDir + "' -NewName ('sqi.suspect-' + (Get-Date -Format yyyyMMddHHmmss))",
		testBinary + " config init",
	}
	otherRemedy := []string{"administrators-only"}
	for _, tc := range []struct {
		name     string
		failures []PathFailure
		want     []string
		wantNot  []string
	}{
		{
			name:     "binary",
			failures: []PathFailure{{Key: "binary", Path: exe, JudgedPath: exe, Reason: `owner DESKTOP\bob is not trusted`}},
			want:     binaryRemedy,
			wantNot:  []string{"Rename-Item", "config init"},
		},
		{
			name: "configuration under ProgramData",
			failures: []PathFailure{
				{Key: "config directory", Path: sqiDir, JudgedPath: sqiDir, Reason: `BUILTIN\Users can write (AD)`},
			},
			want:    moveRemedy,
			wantNot: []string{"Copy-Item"},
		},
		{
			name: "both",
			failures: []PathFailure{
				{Key: "binary directory", Path: filepath.Dir(exe), JudgedPath: filepath.Dir(exe), Reason: "x"},
				{Key: "config", Path: cfg, JudgedPath: sqiDir, Reason: "y"},
			},
			want: append(slices.Clone(binaryRemedy), moveRemedy...),
		},
		{
			name:     "elsewhere",
			failures: []PathFailure{{Key: "nats.tls_ca_file", Path: `E:\ca.pem`, JudgedPath: `E:\`, Reason: "z"}},
			want:     otherRemedy,
			wantNot:  []string{"Copy-Item", "Rename-Item", "config init"},
		},
		{
			// Nothing creates a CA again: moving it aside is no way out.
			name:     "file a setting names, under ProgramData",
			failures: []PathFailure{{Key: "nats.tls_ca_file", Path: caFile, JudgedPath: caFile, Reason: "w"}},
			want:     []string{"permissions", "administrators-only folder"},
			wantNot:  []string{"Rename-Item", "Copy-Item"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refused := &UntrustedPathsError{Failures: tc.failures}
			fake := &fakeOps{checkErr: refused}
			_, err := execute(t, testSpec(new(int)), fake.operations(), &readSpy{}, "install", "--config", cfg)
			if got, ok := errors.AsType[*UntrustedPathsError](err); !ok || got != refused {
				t.Fatalf("install = %v, want the *UntrustedPathsError", err)
			}
			msg := err.Error()
			for _, s := range append([]string{refused.Error()}, tc.want...) {
				if !strings.Contains(msg, s) {
					t.Errorf("refusal lacks %q:\n%s", s, msg)
				}
			}
			for _, s := range tc.wantNot {
				if strings.Contains(msg, s) {
					t.Errorf("refusal has %q:\n%s", s, msg)
				}
			}
		})
	}
}

func TestPathWithin(t *testing.T) {
	tests := []struct {
		dir, root string
		want      bool
	}{
		{`C:\ProgramData`, `C:\ProgramData`, true},
		{`C:\ProgramData\sqi`, `C:\ProgramData`, true},
		{`c:\programdata\SQI\x`, `C:\ProgramData`, true},
		{`C:\ProgramData\sqi\..\sqi`, `C:\ProgramData\`, true},
		{`C:\ProgramDataX`, `C:\ProgramData`, false},
		{`C:\ProgramDataX\sqi`, `C:\ProgramData`, false},
		{`C:\Program`, `C:\ProgramData`, false},
		{`D:\ProgramData`, `C:\ProgramData`, false},
		{`C:\x`, `C:\`, true},
	}
	for _, tt := range tests {
		if got := PathWithin(tt.dir, tt.root); got != tt.want {
			t.Errorf("PathWithin(%q, %q) = %v, want %v", tt.dir, tt.root, got, tt.want)
		}
	}
}

// TestInstall_UserRefusesConfigInDefaultDir pins that --user with a
// configuration in the default %ProgramData%\sqi, which only SYSTEM and
// Administrators can read, is refused before the configuration is loaded or
// anything is changed, pointing at config init --user with a directory of the
// account's own; and that the same configuration installs as LocalSystem.
func TestInstall_UserRefusesConfigInDefaultDir(t *testing.T) {
	pd := t.TempDir()
	t.Setenv("ProgramData", pd)
	for _, dir := range []string{filepath.Join(pd, "sqi"), filepath.Join(pd, "SQI", "nested")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		cfg := filepath.Join(dir, testBinary+".yaml")
		if err := os.WriteFile(cfg, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		fake := &fakeOps{}
		stdin := &readSpy{}
		_, err := execute(t, testSpec(new(int)), fake.operations(), stdin, "install", "--user", "render", "--config", cfg)
		if err == nil {
			t.Fatalf("install --user with %s succeeded", cfg)
		}
		for _, s := range []string{cfg, `.\render`, testBinary + ` config init --user .\render --config`, filepath.Join(pd, "sqi-")} {
			if !strings.Contains(err.Error(), s) {
				t.Errorf("refusal lacks %q:\n%v", s, err)
			}
		}
		if want := []string{"requireElevated", "exists " + testBinary}; !slices.Equal(fake.calls, want) || stdin.read {
			t.Errorf("calls = %q (stdin read %v), want only %q", fake.calls, stdin.read, want)
		}
		fake = &fakeOps{}
		if out, err := execute(t, testSpec(new(int)), fake.operations(), &readSpy{}, "install", "--config", cfg); err != nil {
			t.Errorf("LocalSystem install with %s: %v\n%s", cfg, err, out)
		}
	}
}

// TestInstall_HintsAtUserForAccountDataDir pins the hint a LocalSystem
// refusal adds when a data path it judged grants a user account Modify, as
// config init --user lays it out: install with --user for that account. A
// --user install, and a failure on the configuration itself, get no hint.
func TestInstall_HintsAtUserForAccountDataDir(t *testing.T) {
	data := t.TempDir() // the test user has full control here, inherited
	_, me := currentAccount(t)
	dataFailure := PathFailure{Key: "worker.data_dir", Path: data, JudgedPath: data, Reason: me + " can write (WD)"}
	cfgFailure := PathFailure{Key: "config directory", Path: data, JudgedPath: data, Reason: me + " can write (WD)"}
	hint := "--user " + me
	for _, tc := range []struct {
		name     string
		failures []PathFailure
		user     bool
		want     bool
	}{
		{"data directory, LocalSystem", []PathFailure{dataFailure}, false, true},
		{"data directory, --user", []PathFailure{dataFailure}, true, false},
		{"configuration directory", []PathFailure{cfgFailure}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"install", "--config", writeConfig(t)}
			if tc.user {
				args = append(args, "--user", "render")
			}
			fake := &fakeOps{checkErr: &UntrustedPathsError{Failures: tc.failures}}
			_, err := execute(t, testSpec(new(int)), fake.operations(), &readSpy{}, args...)
			if err == nil {
				t.Fatal("install succeeded")
			}
			msg := strings.ToLower(err.Error())
			if got := strings.Contains(msg, strings.ToLower("written for "+hint)); got != tc.want {
				t.Errorf("hint for %s present = %v, want %v:\n%v", me, got, tc.want, err)
			}
		})
	}
}
