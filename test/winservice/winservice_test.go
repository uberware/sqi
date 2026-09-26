// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build winservice && windows

package winservice

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWinService_ServerLifecycle(t *testing.T) {
	f := startServer(t)

	// Relative defaults resolve beside the config, not in System32.
	for _, p := range []string{filepath.Join(f.dir, "sqi.db"), filepath.Join(f.dir, "data", "nats")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s not created beside the config: %v", p, err)
		}
	}
	status := mustRun(t, bin("sqi-server"), "service", "status", "--name", f.name)
	for _, want := range []string{
		statusState + "running\n",
		statusStartType + "automatic\n",
		statusAccount + "LocalSystem\n",
		// The config path contains a space; the SCM must hold it quoted.
		`serve --config "` + f.cfg + `"` + "\n",
	} {
		if !strings.Contains(status, want) {
			t.Errorf("status missing %q:\n%s", want, status)
		}
	}

	mustRun(t, bin("sqi-server"), "service", "stop", "--name", f.name)
	status = mustRun(t, bin("sqi-server"), "service", "status", "--name", f.name)
	for _, want := range []string{statusState + "stopped\n", statusExitCodes + "0 (service-specific 0)\n"} {
		if !strings.Contains(status, want) {
			t.Errorf("after stop, status missing %q:\n%s", want, status)
		}
	}
	if log := readLog(t, defaultLogPath(f.name)); !strings.Contains(log, "sqi-server stopped cleanly") {
		t.Errorf("server log lacks the clean-stop line:\n%s", log)
	}

	mustRun(t, bin("sqi-server"), "service", "uninstall", "--name", f.name)
	// The SCM deletes a service once its last handle closes; allow it a moment.
	var out string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		var err error
		if out, err = run(t.Context(), "", bin("sqi-server"), "service", "status", "--name", f.name); err != nil &&
			strings.Contains(out, serviceNotFound) {
			return
		}
	}
	t.Errorf("status after uninstall did not report %q:\n%s", serviceNotFound, out)
}

func TestWinService_InstallRefusesExisting(t *testing.T) {
	f := startServer(t)
	out, err := run(t.Context(), "", bin("sqi-server"), "service", "install", "--name", f.name, "--config", f.cfg)
	if err == nil || !strings.Contains(out, "already exists") ||
		!strings.Contains(out, "sqi-server service uninstall --name "+f.name) {
		t.Fatalf("second install: err=%v\n%s", err, out)
	}
	waitHTTP(t, f.httpURL+"/healthz") // the existing service is untouched
	if status := mustRun(t, bin("sqi-server"), "service", "status", "--name", f.name); !strings.Contains(status, statusState+"running\n") {
		t.Errorf("existing service no longer running:\n%s", status)
	}
}

func TestWinService_WorkerLifecycle(t *testing.T) {
	f := startServer(t)
	dir := testDir(t)
	dataDir := filepath.Join(dir, "data")
	cfg := filepath.Join(dir, "sqi-worker.yaml")
	writeFile(t, cfg, workerYAML(t, f.natsURL, dataDir))
	name := uniqueName(t, "sqi-test-worker")
	cleanupService(t, bin("sqi-worker"), name)
	mustRun(t, bin("sqi-worker"), "service", "install", "--name", name, "--config", cfg, "--start")

	status := mustRun(t, bin("sqi-worker"), "service", "status", "--name", name)
	if !strings.Contains(status, statusStartType+"automatic (delayed)\n") {
		t.Errorf("worker not delayed-start:\n%s", status)
	}
	id := readWorkerID(t, dataDir)
	waitWorkerStatus(t, f.httpURL, id, "online", 60*time.Second)

	mustRun(t, bin("sqi-worker"), "service", "stop", "--name", name)
	// Deregistration, not the heartbeat-timeout sweep (30s by default).
	waitWorkerStatus(t, f.httpURL, id, "offline", 10*time.Second)
	if log := readLog(t, defaultLogPath(name)); !strings.Contains(log, `"trigger":"service stop"`) {
		t.Errorf("worker log lacks the service-stop trigger:\n%s", log)
	}
}

// TestWinService_StartupFailureLeavesTrace installs a server whose config
// fails validation. The service reports Running before serve loads and
// validates its config, so `service start` may see either Running or the stop
// that follows (spec §2: it waits for "Running or a stop"); the test therefore
// accepts either outcome and waits for Stopped instead.
func TestWinService_StartupFailureLeavesTrace(t *testing.T) {
	dir := testDir(t)
	cfg := filepath.Join(dir, "sqi-server.yaml")
	writeFile(t, cfg, "log:\n  level: loud\n")
	name := uniqueName(t, "sqi-test-bad")
	cleanupService(t, bin("sqi-server"), name)
	mustRun(t, bin("sqi-server"), "service", "install", "--name", name, "--config", cfg)
	out, err := run(t.Context(), "", bin("sqi-server"), "service", "start", "--name", name)
	t.Logf("service start (either outcome is expected): err=%v\n%s", err, out)

	// The SCM's restart-on-failure actions may restart it meanwhile; every
	// restart fails the same way, so Stopped always carries the same codes.
	status := waitServiceState(t, bin("sqi-server"), name, "stopped", 30*time.Second)
	if want := statusExitCodes + "1066 (service-specific 1)\n"; !strings.Contains(status, want) {
		t.Errorf("status missing the failure exit code %q:\n%s", want, status)
	}
	log := readLog(t, defaultLogPath(name))
	for _, want := range []string{`"msg":"service exited with error"`, "log.level: unknown level"} {
		if !strings.Contains(log, want) {
			t.Errorf("trace missing %q:\n%s", want, log)
		}
	}
}

// TestWinService_HandRegisteredNewService is the regression test for the
// original report: a worker registered with New-Service exactly as the old
// docs said must now start.
func TestWinService_HandRegisteredNewService(t *testing.T) {
	f := startServer(t)
	dir := testDir(t)
	dataDir := filepath.Join(dir, "data")
	cfg := filepath.Join(dir, "sqi-worker.yaml")
	writeFile(t, cfg, workerYAML(t, f.natsURL, dataDir))
	name := uniqueName(t, "sqi-test-manual")
	cleanupService(t, bin("sqi-worker"), name)
	ps := fmt.Sprintf(`$ErrorActionPreference = 'Stop'; `+
		`New-Service -Name '%s' -BinaryPathName '"%s" start --config "%s"' -StartupType Manual | Out-Null; `+
		`Start-Service -Name '%s'`,
		name, bin("sqi-worker"), cfg, name)
	if out, err := exec.CommandContext(t.Context(), "powershell", "-NoProfile", "-NonInteractive", "-Command", ps).CombinedOutput(); err != nil {
		t.Fatalf("New-Service/Start-Service: %v\n%s", err, out)
	}
	waitWorkerStatus(t, f.httpURL, readWorkerID(t, dataDir), "online", 60*time.Second)
}

func TestWinService_UserAccount(t *testing.T) {
	// First, so that — cleanups running in reverse — the service and the
	// directories below are removed before the account.
	account, password := throwawayAccount(t)
	if _, err := os.Stat(logDir()); err != nil {
		t.Logf("%s does not exist yet, so this run covers install creating it, "+
			"not granting the account access to the existing protected directory; run the whole suite for that", logDir())
	}

	dir := testDir(t)
	// The service account must be able to write sqi.db and data/nats here.
	if out, err := exec.CommandContext(t.Context(), "icacls", dir, "/grant", account+":(OI)(CI)M").CombinedOutput(); err != nil {
		t.Fatalf("icacls: %v\n%s", err, out)
	}
	ports := freePorts(t, 2)
	cfg := filepath.Join(dir, "sqi-server.yaml")
	writeFile(t, cfg, serverYAML(ports[0], ports[1]))
	name := uniqueName(t, "sqi-test-user")
	cleanupService(t, bin("sqi-server"), name)
	// The password is piped on stdin, never passed as an argument or logged.
	out, err := run(t.Context(), password+"\n", bin("sqi-server"), "service", "install", "--name", name,
		"--config", cfg, "--user", account, "--start")
	if err != nil {
		t.Fatalf("install --user: %v\n%s", err, out)
	}
	waitHTTP(t, "http://"+hostPort(ports[0])+"/healthz")
	status := mustRun(t, bin("sqi-server"), "service", "status", "--name", name)
	for _, want := range []string{statusState + "running\n", statusAccount + `.\` + account + "\n"} {
		if !strings.Contains(status, want) {
			t.Errorf("status missing %q:\n%s", want, status)
		}
	}
	// The logs directory normally already exists (protected, created by the
	// earlier LocalSystem installs); the --user service must still have
	// written its log there.
	if _, err := os.Stat(defaultLogPath(name)); err != nil {
		t.Errorf("--user service wrote no log: %v", err)
	}
}
