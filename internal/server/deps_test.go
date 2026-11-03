// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// workerPkgPrefix is the import path prefix of the worker binary's packages.
const workerPkgPrefix = "github.com/uberware/sqi/internal/worker/"

// serverWorkerDeps are the worker packages sqi-server may link: the wire
// protocol, the leaf both binaries share.
var serverWorkerDeps = map[string]bool{workerPkgPrefix + "protocol": true}

// TestSQIServerLinksNoWorkerInternals keeps the worker's configuration,
// broker client, capability detection and commands out of sqi-server. Values
// both binaries need belong in internal/worker/protocol.
func TestSQIServerLinksNoWorkerInternals(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	out, err := exec.CommandContext(context.Background(), "go", "list", "-deps", "github.com/uberware/sqi/cmd/sqi-server").Output()
	if err != nil {
		t.Fatalf("go list -deps ./cmd/sqi-server: %v", err)
	}
	for pkg := range strings.FieldsSeq(string(out)) {
		if strings.HasPrefix(pkg, workerPkgPrefix) && !serverWorkerDeps[pkg] {
			t.Errorf("sqi-server links %s; move what it needs into internal/worker/protocol", pkg)
		}
	}
}
