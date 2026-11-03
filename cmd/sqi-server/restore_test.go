// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/bus/bustest"
	"github.com/uberware/sqi/internal/config"
)

// writeExternalRestoreConfig writes a configuration in external broker mode
// with the store at db and returns its path. seed is the data user's nkey
// seed file, "" for none.
func writeExternalRestoreConfig(t *testing.T, db string, urls []string, seed string) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "store:\n  sqlite_path: %q\nnats:\n  mode: external\n  external:\n    connect_timeout: 1s\n    urls:\n", db)
	for _, u := range urls {
		fmt.Fprintf(&b, "      - %q\n", u)
	}
	if seed != "" {
		fmt.Fprintf(&b, "    nkey_seed_file: %q\n", seed)
	}
	path := filepath.Join(t.TempDir(), "sqi-server.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// unreachableURL is a nats:// URL nothing listens on.
func unreachableURL(t *testing.T) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := "nats://" + l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return u
}

// requireNotRestored fails t if restore moved the database aside.
func requireNotRestored(t *testing.T, db string) {
	t.Helper()
	moved, err := filepath.Glob(db + ".pre-restore-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(moved) != 0 {
		t.Fatalf("restore moved the database aside although it refused: %v", moved)
	}
}

// executeRestore runs restore with args and returns its stdout and error.
func executeRestore(t *testing.T, args ...string) (string, error) {
	t.Helper()
	prepareRoot(append([]string{"restore"}, args...))
	var runErr error
	var out string
	_ = captureStderr(t, func() { out = captureStdout(t, func() { runErr = Execute() }) })
	return out, runErr
}

func TestRestoreCmd_ExternalUnreachableRefusesWithoutSkipFlag(t *testing.T) {
	isolateConfigDiscovery(t)
	unsetStoreSQLitePathEnv(t)
	db, backup, _ := restoreFixture(t)
	withConfigFile(t, writeExternalRestoreConfig(t, db, []string{unreachableURL(t)}, ""))

	_, err := executeRestore(t, backup)
	if err == nil || !strings.Contains(err.Error(), "--skip-nats-purge") {
		t.Fatalf("restore with the broker unreachable = %v, want a refusal naming --skip-nats-purge", err)
	}
	requireNotRestored(t, db)

	out, err := executeRestore(t, backup, "--skip-nats-purge")
	if err != nil {
		t.Fatalf("restore --skip-nats-purge: %v", err)
	}
	streams := []string{bus.StreamTask, bus.StreamLogs, bus.StreamWorker, bus.StreamCancel}
	consumers := []string{"sqi-task-status-srv", "sqi-task-logs-srv", "sqi-worker-srv"}
	for _, want := range append(append([]string{"fenced 1 attempt(s)", "nats stream purge", "nats consumer rm"}, streams...), consumers...) {
		if !strings.Contains(out, want) {
			t.Errorf("restore --skip-nats-purge output missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestRestoreCmd_ExternalPurgesTheCluster(t *testing.T) {
	isolateConfigDiscovery(t)
	unsetStoreSQLitePathEnv(t)
	c := bustest.StartCluster(t, 1, bustest.Options{})
	db, backup, _ := restoreFixture(t)
	withConfigFile(t, writeExternalRestoreConfig(t, db, c.URLs, c.DataSeed))

	// A farm that ran: streams provisioned, one message waiting.
	logger := slog.New(slog.DiscardHandler)
	server := bus.NewRemote(bus.RemoteConfig{
		URLs: c.URLs, Account: bus.DefaultAccount, NkeySeedFile: c.DataSeed, StreamReplicas: 1, ConnectTimeout: 10 * time.Second,
	}, logger)
	if err := server.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	client, err := server.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	if err := client.PublishTaskStatus(t.Context(), "w1", "j1", []byte("stale")); err != nil {
		t.Fatal(err)
	}
	client.Close()
	server.Shutdown()

	out, err := executeRestore(t, backup)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !strings.Contains(out, "purged") || strings.Contains(out, "NATS data directory") {
		t.Errorf("restore summary does not report the purge\ngot:\n%s", out)
	}

	cred, err := nats.NkeyOptionFromSeed(c.DataSeed)
	if err != nil {
		t.Fatal(err)
	}
	nc, err := nats.Connect(strings.Join(c.URLs, ","), cred)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	s, err := js.Stream(t.Context(), bus.StreamTask)
	if err != nil {
		t.Fatal(err)
	}
	if n := s.CachedInfo().State.Msgs; n != 0 {
		t.Errorf("%s holds %d messages after restore, want 0", bus.StreamTask, n)
	}
}

func TestRestoreCmd_SkipNATSPurgeRefusedInEmbeddedMode(t *testing.T) {
	isolateConfigDiscovery(t)
	db, backup, natsDir := restoreFixture(t)
	_, err := executeRestore(t, backup, "--db", db, "--nats-data-dir", natsDir, "--skip-nats-purge")
	if err == nil || !strings.Contains(err.Error(), "external") {
		t.Fatalf("restore --skip-nats-purge in embedded mode = %v, want a refusal", err)
	}
	requireNotRestored(t, db)
	if _, err := os.Stat(natsDir); err != nil {
		t.Fatalf("the NATS directory moved although restore refused: %v", err)
	}
}

func TestRestoreCmd_NATSDataDirRefusedInExternalMode(t *testing.T) {
	isolateConfigDiscovery(t)
	unsetStoreSQLitePathEnv(t)
	db, backup, natsDir := restoreFixture(t)
	withConfigFile(t, writeExternalRestoreConfig(t, db, []string{unreachableURL(t)}, ""))
	_, err := executeRestore(t, backup, "--nats-data-dir", natsDir, "--skip-nats-purge")
	if err == nil || !strings.Contains(err.Error(), "--nats-data-dir") {
		t.Fatalf("restore --nats-data-dir in external mode = %v, want a refusal", err)
	}
	requireNotRestored(t, db)
	if _, err := os.Stat(natsDir); err != nil {
		t.Fatalf("the NATS directory moved although restore refused: %v", err)
	}
}

// A server running on the account refuses restore before the database is
// touched, naming the holder and saying nothing changed.
func TestRestoreCmd_ExternalRefusedWhileServerRunning(t *testing.T) {
	isolateConfigDiscovery(t)
	unsetStoreSQLitePathEnv(t)
	c := bustest.StartCluster(t, 1, bustest.Options{})
	db, backup, _ := restoreFixture(t)
	withConfigFile(t, writeExternalRestoreConfig(t, db, c.URLs, c.DataSeed))

	server := bus.NewRemote(bus.RemoteConfig{
		URLs: c.URLs, Account: bus.DefaultAccount, NkeySeedFile: c.DataSeed, StreamReplicas: 1, ConnectTimeout: 10 * time.Second,
	}, slog.New(slog.DiscardHandler))
	if err := server.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown()

	_, err := executeRestore(t, backup)
	if !errors.Is(err, bus.ErrGuardHeld) || !strings.Contains(err.Error(), "restore changed nothing") ||
		!strings.Contains(err.Error(), "stop that sqi-server") {
		t.Fatalf("restore beside a running server = %v, want a refusal saying nothing changed", err)
	}
	if strings.Contains(err.Error(), "--skip-nats-purge") {
		t.Errorf("err = %v; it must not offer --skip-nats-purge while a server runs", err)
	}
	requireNotRestored(t, db)
}

// A configuration that fails to load leaves an external-mode operator nothing
// to fall back to: the refusal says so, and with SQI_NATS_MODE=external even
// naming the store and a NATS directory does not get past it.
func TestRestoreCmd_UnloadableConfigInExternalMode(t *testing.T) {
	isolateConfigDiscovery(t)
	unsetStoreSQLitePathEnv(t)
	t.Setenv("SQI_SCHEDULER_TICK_INTERVAL", "not-a-duration")
	db, backup, natsDir := restoreFixture(t)

	_, err := executeRestore(t, backup, "--db", db)
	if err == nil || !strings.Contains(err.Error(), "nats.mode: external") {
		t.Fatalf("restore with an unloadable config = %v, want a refusal that covers external mode", err)
	}

	t.Setenv("SQI_NATS_MODE", "external")
	_, err = executeRestore(t, backup, "--db", db, "--nats-data-dir", natsDir)
	if err == nil || !strings.Contains(err.Error(), "SQI_NATS_MODE") {
		t.Fatalf("restore with SQI_NATS_MODE=external and an unloadable config = %v, want a refusal", err)
	}
	requireNotRestored(t, db)
	if _, err := os.Stat(natsDir); err != nil {
		t.Fatalf("the NATS directory moved although restore refused: %v", err)
	}
}

// The manual steps never print a URL's credentials, and quote every value as
// one literal shell word.
func TestPrintManualPurge_RedactsAndQuotes(t *testing.T) {
	odd := filepath.Join(t.TempDir(), "it's $HOME `x`.nk")
	var out strings.Builder
	printManualPurge(&out, config.NATSExternalConfig{
		Account:      "SQI",
		URLs:         []string{"nats://s3cr3t-token@a:4222", "nats://sqi:hunter2@b:4222", "nats://[::1"},
		NkeySeedFile: odd,
	})
	got := out.String()
	for _, secret := range []string{"s3cr3t-token", "hunter2", "sqi:", "[::1"} {
		if strings.Contains(got, secret) {
			t.Errorf("output prints %q:\n%s", secret, got)
		}
	}
	if !strings.Contains(got, "nats://REDACTED@a:4222,nats://REDACTED@b:4222,REDACTED-URL") {
		t.Errorf("output does not list the redacted URLs:\n%s", got)
	}

	for _, s := range []string{odd, "plain/path.nk", "a'b", "'", ""} {
		quoted := shellQuote(s)
		if runtime.GOOS == "windows" {
			continue
		}
		echo, err := exec.CommandContext(t.Context(), "/bin/sh", "-c", "printf %s "+quoted).Output()
		if err != nil {
			t.Fatalf("sh with %s: %v", quoted, err)
		}
		if string(echo) != s {
			t.Errorf("shellQuote(%q) = %s, which the shell reads as %q", s, quoted, echo)
		}
	}
	if !strings.Contains(got, shellQuote(odd)) {
		t.Errorf("output does not quote %q as %s:\n%s", odd, shellQuote(odd), got)
	}
}
