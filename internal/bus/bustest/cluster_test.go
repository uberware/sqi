// SPDX-License-Identifier: AGPL-3.0-or-later

package bustest_test

import (
	"bufio"
	"context"
	"go/parser"
	"go/token"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/bus/bustest"
)

// infoLine reads the INFO line a node sends on connect.
func infoLine(t *testing.T, rawURL string) string {
	t.Helper()
	addr := strings.TrimPrefix(rawURL, "nats://")
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read INFO: %v", err)
	}
	return line
}

func connectSeed(t *testing.T, urls []string, seedFile string) *nats.Conn {
	t.Helper()
	opt, err := nats.NkeyOptionFromSeed(seedFile)
	if err != nil {
		t.Fatal(err)
	}
	nc, err := nats.Connect(strings.Join(urls, ","), opt)
	if err != nil {
		t.Fatalf("connect with %s: %v", filepath.Base(seedFile), err)
	}
	t.Cleanup(nc.Close)
	return nc
}

func TestStartCluster_ThreeRoutedNodes(t *testing.T) {
	c := bustest.StartCluster(t, 3, bustest.Options{})
	if len(c.URLs) != 3 || c.ClusterName == "" {
		t.Fatalf("cluster = %d URLs, name %q", len(c.URLs), c.ClusterName)
	}
	for _, u := range c.URLs {
		nc := connectSeed(t, []string{u}, c.DataSeed)
		if got := nc.ConnectedClusterName(); got != c.ClusterName {
			t.Errorf("%s reports cluster %q, want %q", u, got, c.ClusterName)
		}
	}
	// The system user is in SYS and can ask every node for its varz.
	sys := connectSeed(t, c.URLs, c.SystemCreds)
	sub, err := sys.SubscribeSync(sys.NewRespInbox())
	if err != nil {
		t.Fatal(err)
	}
	if err := sys.PublishRequest("$SYS.REQ.SERVER.PING.VARZ", sub.Subject, nil); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := sub.NextMsg(5 * time.Second); err != nil {
			t.Fatalf("varz reply %d of 3: %v", i+1, err)
		}
	}
}

func TestStartCluster_PasswordUsersIssueNoNonce(t *testing.T) {
	c := bustest.StartCluster(t, 1, bustest.Options{PasswordUsers: true, AuthEnabled: true})
	if strings.Contains(infoLine(t, c.URLs[0]), `"nonce"`) {
		t.Fatal("a cluster with only password users sent a nonce")
	}
	if c.DataSeed != "" || c.CalloutSeed != "" || c.SystemCreds != "" {
		t.Errorf("password cluster wrote seeds: %+v", c)
	}
	nc, err := nats.Connect(c.URLs[0], nats.UserInfo(c.DataUser, c.DataPassword))
	if err != nil {
		t.Fatalf("data user connect: %v", err)
	}
	nc.Close()

	nkeyCluster := bustest.StartCluster(t, 1, bustest.Options{})
	if !strings.Contains(infoLine(t, nkeyCluster.URLs[0]), `"nonce"`) {
		t.Fatal("a cluster with nkey users sent no nonce")
	}
}

func TestStartCluster_AuthEnabledWritesCalloutKeys(t *testing.T) {
	c := bustest.StartCluster(t, 1, bustest.Options{AuthEnabled: true})
	for name, path := range map[string]string{"callout": c.CalloutSeed, "issuer": c.IssuerSeed} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s seed %q: %v", name, path, err)
		}
	}
	// sqi's data user is exempt from the callout, so it connects with no
	// responder running.
	connectSeed(t, c.URLs, c.DataSeed)
}

func TestStartCluster_FragmentFileUsedVerbatim(t *testing.T) {
	seed, pub, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatal(err)
	}
	frag, err := bus.Layout{Account: "FARM", DataUserPub: pub}.ConfigFragment()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	fragFile := filepath.Join(dir, "sqi-accounts.conf")
	seedFile := filepath.Join(dir, "data.nk")
	if err := os.WriteFile(fragFile, []byte(frag), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(seedFile, seed, 0o600); err != nil {
		t.Fatal(err)
	}

	c := bustest.StartCluster(t, 1, bustest.Options{FragmentFile: fragFile})
	if c.DataSeed != "" {
		t.Errorf("harness generated a data seed for a supplied fragment")
	}
	nc := connectSeed(t, c.URLs, seedFile)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := nc.RequestWithContext(ctx, "$JS.API.INFO", nil); err != nil {
		t.Fatalf("JetStream in the fragment's account: %v", err)
	}
}

func TestCluster_RestartNodeKeepsPorts(t *testing.T) {
	c := bustest.StartCluster(t, 3, bustest.Options{})
	c.StopNode(1)
	if _, err := nats.Connect(c.URLs[1], nats.Timeout(time.Second)); err == nil {
		t.Fatal("a stopped node accepted a connection")
	}
	c.RestartNode(1)
	nc := connectSeed(t, []string{c.URLs[1]}, c.DataSeed)
	if got := nc.ConnectedClusterName(); got != c.ClusterName {
		t.Fatalf("restarted node reports cluster %q, want %q", got, c.ClusterName)
	}
}

// TestNoProductionImports keeps the harness out of the shipped binaries: under
// cmd/ and internal/, only _test.go files may import it.
func TestNoProductionImports(t *testing.T) {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this file")
	}
	root := filepath.Join(filepath.Dir(here), "..", "..", "..")
	const self = "github.com/uberware/sqi/internal/bus/bustest"
	selfDir := filepath.Dir(here)
	walk := func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") ||
			strings.HasPrefix(name, "._") || filepath.Dir(path) == selfDir {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == self { //nolint:errcheck // a parsed import path is always quoted
				t.Errorf("%s imports %s; only tests may", path, self)
			}
		}
		return nil
	}
	// test/ is never part of a shipped binary, and its integration helpers
	// may use the harness.
	for _, dir := range []string{"cmd", "internal"} {
		if err := filepath.WalkDir(filepath.Join(root, dir), walk); err != nil {
			t.Fatal(err)
		}
	}
}
