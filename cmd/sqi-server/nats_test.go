// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/bus/bustest"
	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/server"
)

// execNATSExternalInit drives "nats external init" with args and returns its
// combined output.
func execNATSExternalInit(t *testing.T, args ...string) (string, error) {
	t.Helper()
	prepareRoot(append([]string{"nats", "external", "init"}, args...))
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	err := rootCmd.Execute()
	return out.String(), err
}

// generatedFiles are the five files the command writes.
var generatedFiles = []string{"issuer.nk", "sqi-data.nk", "sqi-auth.nk", "sqi-sys.nk", "sqi-accounts.conf"}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func mustAbs(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// seedPublicKey reads the seed at path and returns its public key and whether
// it is an account key.
func seedPublicKey(t *testing.T, path string) (pub string, isAccount bool) {
	t.Helper()
	seed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	pub, err = brokerauth.PublicKeyFromSeed(seed)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return pub, nkeys.IsValidPublicAccountKey(pub)
}

func TestNATSExternalInit_WritesFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nats")
	out, err := execNATSExternalInit(t, "--out", dir)
	if err != nil {
		t.Fatalf("nats external init: %v\n%s", err, out)
	}

	for _, name := range generatedFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
	for _, name := range []string{"issuer.nk", "sqi-data.nk", "sqi-auth.nk", "sqi-sys.nk"} {
		assertSecretRestricted(t, filepath.Join(dir, name))
		if _, err := brokerauth.LoadSeed(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	// One account key, three distinct user keys.
	pubs := map[string]string{}
	for _, name := range []string{"issuer.nk", "sqi-data.nk", "sqi-auth.nk", "sqi-sys.nk"} {
		pub, isAccount := seedPublicKey(t, filepath.Join(dir, name))
		if want := name == "issuer.nk"; isAccount != want {
			t.Errorf("%s: account key = %v, want %v", name, isAccount, want)
		}
		if other, dup := pubs[pub]; dup {
			t.Errorf("%s and %s hold the same key", name, other)
		}
		pubs[pub] = name
	}

	frag, err := os.ReadFile(filepath.Join(dir, "sqi-accounts.conf"))
	if err != nil {
		t.Fatal(err)
	}
	for pub, name := range pubs {
		if !strings.Contains(string(frag), pub) {
			t.Errorf("the fragment does not name the key of %s", name)
		}
	}
	for _, seed := range []string{"issuer.nk", "sqi-data.nk"} {
		if bytes.Contains(frag, mustRead(t, filepath.Join(dir, seed))) {
			t.Errorf("the fragment contains the private seed %s", seed)
		}
	}

	abs := mustAbs(t, dir)
	for _, name := range []string{"issuer.nk", "sqi-data.nk", "sqi-auth.nk", "sqi-sys.nk", "sqi-accounts.conf"} {
		if !strings.Contains(out, filepath.Join(abs, name)) {
			t.Errorf("the output does not report the absolute path of %s:\n%s", name, out)
		}
	}
}

func TestNATSExternalInit_AccountFlag(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nats")
	out, err := execNATSExternalInit(t, "--out", dir, "--account", "FARM_1")
	if err != nil {
		t.Fatalf("nats external init: %v\n%s", err, out)
	}
	frag := mustRead(t, filepath.Join(dir, "sqi-accounts.conf"))
	if !strings.Contains(string(frag), `"FARM_1"`) {
		t.Errorf("the fragment does not define account FARM_1:\n%s", frag)
	}
	if !strings.Contains(out, `account: "FARM_1"`) {
		t.Errorf("the printed config does not name account FARM_1:\n%s", out)
	}

	for _, bad := range []string{"AUTH", "SYS", "has space", "$G"} {
		d := filepath.Join(t.TempDir(), "bad")
		if _, err := execNATSExternalInit(t, "--out", d, "--account", bad); err == nil {
			t.Errorf("--account %q was accepted", bad)
		}
		if _, err := os.Stat(d); err == nil {
			t.Errorf("--account %q was refused but wrote %s", bad, d)
		}
	}
}

func TestNATSExternalInit_RefusesOverwrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nats")
	if out, err := execNATSExternalInit(t, "--out", dir); err != nil {
		t.Fatalf("first run: %v\n%s", err, out)
	}
	before := map[string][]byte{}
	for _, name := range generatedFiles {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = b
	}

	out, err := execNATSExternalInit(t, "--out", dir)
	if err == nil {
		t.Fatalf("a second run replaced existing keys without --force:\n%s", out)
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error %q does not mention --force", err)
	}
	for name, want := range before {
		got := mustRead(t, filepath.Join(dir, name))
		if !bytes.Equal(got, want) {
			t.Errorf("%s changed on a refused run", name)
		}
	}

	// One surviving file is enough to refuse, and nothing else is written.
	for _, name := range generatedFiles {
		if name == "sqi-auth.nk" {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := execNATSExternalInit(t, "--out", dir); err == nil {
		t.Fatal("a run beside an existing sqi-auth.nk was accepted")
	}
	for _, name := range generatedFiles {
		if name == "sqi-auth.nk" {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("a refused run still wrote %s", name)
		}
	}
}

func TestNATSExternalInit_ForceOverwrites(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nats")
	if out, err := execNATSExternalInit(t, "--out", dir); err != nil {
		t.Fatalf("first run: %v\n%s", err, out)
	}
	oldPub, _ := seedPublicKey(t, filepath.Join(dir, "sqi-data.nk"))

	if out, err := execNATSExternalInit(t, "--out", dir, "--force"); err != nil {
		t.Fatalf("--force run: %v\n%s", err, out)
	}
	newPub, _ := seedPublicKey(t, filepath.Join(dir, "sqi-data.nk"))
	if newPub == oldPub {
		t.Error("--force kept the old data key")
	}
	for _, name := range []string{"issuer.nk", "sqi-data.nk", "sqi-auth.nk", "sqi-sys.nk"} {
		assertSecretRestricted(t, filepath.Join(dir, name))
	}
	frag := mustRead(t, filepath.Join(dir, "sqi-accounts.conf"))
	if strings.Contains(string(frag), oldPub) || !strings.Contains(string(frag), newPub) {
		t.Error("the fragment does not match the replaced keys")
	}
}

// captureProcessOutput runs fn with the process's stdout and stderr
// redirected and returns what was written to them, so a test also sees
// output that bypasses the command's own writers (a log line, a stray print).
func captureProcessOutput(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	done := make(chan error, 1)
	go func() {
		_, err := buf.ReadFrom(r)
		done <- err
	}()
	stdout, stderr := os.Stdout, os.Stderr
	func() {
		os.Stdout, os.Stderr = w, w
		defer func() { os.Stdout, os.Stderr = stdout, stderr }()
		fn()
	}()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// TestNATSExternalInit_NeverPrintsASeed checks that no generated private key
// reaches the terminal, on a first run or a --force rerun: the seeds go to
// owner-only files, and the output names the files only.
func TestNATSExternalInit_NeverPrintsASeed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nats")
	for _, args := range [][]string{{"--out", dir}, {"--out", dir, "--force"}} {
		var out string
		var runErr error
		process := captureProcessOutput(t, func() { out, runErr = execNATSExternalInit(t, args...) })
		if runErr != nil {
			t.Fatalf("nats external init %v: %v\n%s", args, runErr, out)
		}
		all := out + process
		for _, name := range []string{"issuer.nk", "sqi-data.nk", "sqi-auth.nk", "sqi-sys.nk"} {
			seed := strings.TrimSpace(string(mustRead(t, filepath.Join(dir, name))))
			if len(seed) < 32 || seed[0] != 'S' {
				t.Fatalf("%s does not hold an nkey seed: %d bytes", name, len(seed))
			}
			// The whole seed, and any long piece of its key material.
			for _, piece := range []string{seed, seed[2:22], seed[len(seed)-20:]} {
				if strings.Contains(all, piece) {
					t.Fatalf("nats external init %v printed (part of) the seed in %s", args, name)
				}
			}
		}
	}
}

// printedConfig returns the YAML block at the end of the command's output, with
// the placeholder broker URL replaced by url.
func printedConfig(t *testing.T, out, url string) string {
	t.Helper()
	_, yaml, ok := strings.Cut(out, "\nnats:\n")
	if !ok {
		t.Fatalf("the output has no nats: block:\n%s", out)
	}
	yaml = "nats:\n" + yaml
	if !strings.Contains(yaml, natsExternalPlaceholderURL) {
		t.Fatalf("the printed config does not carry the placeholder URL %q:\n%s", natsExternalPlaceholderURL, yaml)
	}
	return strings.ReplaceAll(yaml, natsExternalPlaceholderURL, url)
}

// credLookup is a [bus.CredentialLookup] over a fixed set of enrolled keys.
type credLookup struct {
	mu    sync.Mutex
	byKey map[string]string
}

func (c *credLookup) lookup(_ context.Context, pub string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id, ok := c.byKey[pub]
	if !ok {
		return "", bus.ErrUnknownCredential
	}
	return id, nil
}

func (c *credLookup) remove(pub string) {
	c.mu.Lock()
	delete(c.byKey, pub)
	c.mu.Unlock()
}

// TestNATSExternalInit_FragmentBootsWithPrintedConfig boots a nats-server from
// the generated fragment alone, loads the printed YAML (with only the URL
// filled in) through config.Load, and runs sqi's broker from it: an enrolled
// worker connects, an unknown key does not, and revoking the worker
// disconnects it, which needs the generated system user.
func TestNATSExternalInit_FragmentBootsWithPrintedConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nats")
	out, err := execNATSExternalInit(t, "--out", dir)
	if err != nil {
		t.Fatalf("nats external init: %v\n%s", err, out)
	}
	cluster := bustest.StartCluster(t, 1, bustest.Options{FragmentFile: filepath.Join(dir, "sqi-accounts.conf")})

	cfgPath := filepath.Join(t.TempDir(), "sqi-server.yaml")
	if err := os.WriteFile(cfgPath, []byte(printedConfig(t, out, cluster.URLs[0])), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath, config.FlagOverrides{})
	if err != nil {
		t.Fatalf("config.Load of the printed config: %v", err)
	}
	if errs := config.Validate(cfg); len(errs) > 0 {
		t.Fatalf("the printed config is invalid: %v", errs)
	}
	if cfg.NATS.Mode != config.NATSModeExternal || !cfg.NATS.Auth.Enabled {
		t.Fatalf("mode = %q, auth = %v; want external with auth on", cfg.NATS.Mode, cfg.NATS.Auth.Enabled)
	}
	if cfg.NATS.External.Callout.SystemCredsFile == "" {
		t.Fatal("the printed config has no callout.system_creds_file")
	}

	workerSeed, workerPub, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatal(err)
	}
	creds := &credLookup{byKey: map[string]string{workerPub: "worker-a"}}
	rcfg := server.RemoteBrokerConfig(cfg.NATS)
	rcfg.Auth.Lookup = creds.lookup

	r := bus.NewRemote(rcfg, slog.New(slog.DiscardHandler))
	if err := r.Start(t.Context()); err != nil {
		t.Fatalf("Start from the printed config: %v", err)
	}
	t.Cleanup(r.Shutdown)
	if err := r.Check(t.Context()); err != nil {
		t.Fatalf("Check: %v", err)
	}

	disconnected := make(chan struct{}, 1)
	nc, err := nats.Connect(cluster.URLs[0], brokerauth.NkeyOption(workerPub, workerSeed), nats.MaxReconnects(-1),
		nats.ReconnectWait(50*time.Millisecond), nats.IgnoreAuthErrorAbort(),
		nats.ErrorHandler(func(*nats.Conn, *nats.Subscription, error) {}),
		nats.DisconnectErrHandler(func(*nats.Conn, error) {
			select {
			case disconnected <- struct{}{}:
			default:
			}
		}))
	if err != nil {
		t.Fatalf("the enrolled worker was refused: %v", err)
	}
	t.Cleanup(nc.Close)

	strangerSeed, _, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatal(err)
	}
	if c, err := nats.Connect(cluster.URLs[0], nats.NoReconnect(), brokerauth.NkeyOption(workerPub, strangerSeed)); err == nil {
		c.Close()
		t.Fatal("a worker presenting an enrolled key signed by another seed was accepted")
	} else if !errors.Is(err, nats.ErrAuthorization) {
		t.Errorf("stranger error = %v, want nats.ErrAuthorization", err)
	}

	creds.remove(workerPub)
	if err := r.Revoke(t.Context(), "worker-a"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	select {
	case <-disconnected:
		nc.Close()
	case <-time.After(5 * time.Second):
		t.Fatalf("the revoked worker is still %s: the generated system user did not kick it", nc.Status())
	}
}

func TestNATSExternalInit_OutputIsACopyPasteBlock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nats")
	out, err := execNATSExternalInit(t, "--out", dir)
	if err != nil {
		t.Fatal(err)
	}
	abs := mustAbs(t, dir)
	if !strings.Contains(out, `include "sqi-accounts.conf"`) {
		t.Errorf("the output does not show how to include the fragment:\n%s", out)
	}
	yaml := out[strings.Index(out, "\nnats:\n")+1:]
	for _, want := range []string{
		"mode: external",
		"enabled: true",
		"nkey_seed_file: " + fmt.Sprintf("%q", filepath.Join(abs, "sqi-data.nk")),
		"issuer_seed_file: " + fmt.Sprintf("%q", filepath.Join(abs, "issuer.nk")),
		"system_creds_file: " + fmt.Sprintf("%q", filepath.Join(abs, "sqi-sys.nk")),
	} {
		if !strings.Contains(yaml, want) {
			t.Errorf("the printed config lacks %q:\n%s", want, yaml)
		}
	}
	if !strings.Contains(yaml, "nkey_seed_file: "+`"`+filepath.Join(abs, "sqi-auth.nk")+`"`) {
		t.Errorf("the printed config does not wire sqi-auth.nk as the callout seed:\n%s", yaml)
	}
}

// TestNATSExternalInit_PrintedIncludeLineParses checks the include line the
// command prints against nats-server's own config reader.
func TestNATSExternalInit_PrintedIncludeLineParses(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nats")
	out, err := execNATSExternalInit(t, "--out", dir)
	if err != nil {
		t.Fatal(err)
	}
	var include string
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "include ") {
			include = strings.TrimSpace(line)
		}
	}
	if include == "" {
		t.Fatalf("no include line in the output:\n%s", out)
	}
	// The fragment sits beside the including config, as the output instructs.
	frag, err := os.ReadFile(filepath.Join(dir, "sqi-accounts.conf"))
	if err != nil {
		t.Fatal(err)
	}
	nodeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(nodeDir, "sqi-accounts.conf"), frag, 0o600); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(nodeDir, "nats-server.conf")
	if err := os.WriteFile(conf, []byte("listen: 127.0.0.1:-1\n"+include+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts, err := natsserver.ProcessConfigFile(conf)
	if err != nil {
		t.Fatalf("nats-server cannot read the printed include line %q: %v", include, err)
	}
	if opts.AuthCallout == nil || len(opts.Accounts) == 0 {
		t.Errorf("the included fragment defined no callout or accounts: %+v", opts.AuthCallout)
	}
}
