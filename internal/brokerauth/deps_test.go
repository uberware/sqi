// SPDX-License-Identifier: AGPL-3.0-or-later

package brokerauth_test

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	jwtModule  = "github.com/nats-io/jwt/v2"
	natsModule = "github.com/nats-io/nats-server/v2"
)

// requiredVersion returns the version of module that the go.mod at path
// requires, handling both block and single-line require forms.
func requiredVersion(t *testing.T, path, module string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(sc.Text()), "require"))
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == module {
			return fields[1]
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	t.Fatalf("%s does not require %s", path, module)
	return ""
}

func goEnv(t *testing.T, key string) string {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "go", "env", key).Output()
	if err != nil {
		t.Fatalf("go env %s: %v", key, err)
	}
	return strings.TrimSpace(string(out))
}

// TestJWTVersionMatchesNATSServer pins jwt/v2 to exactly the version the
// pinned nats-server requires. The server validates the account and user
// claims this module issues; a jwt/v2 ahead of the server's can mint claims
// the broker does not understand.
func TestJWTVersionMatchesNATSServer(t *testing.T) {
	ours := requiredVersion(t, filepath.Join("..", "..", "go.mod"), jwtModule)
	serverVersion := requiredVersion(t, filepath.Join("..", "..", "go.mod"), natsModule)

	modCache := goEnv(t, "GOMODCACHE")
	serverGoMod := filepath.Join(modCache, filepath.FromSlash(natsModule)+"@"+serverVersion, "go.mod")
	if _, err := os.Stat(serverGoMod); err != nil {
		t.Fatalf("nats-server %s not in module cache (%v); run go mod download", serverVersion, err)
	}
	theirs := requiredVersion(t, serverGoMod, jwtModule)

	if ours != theirs {
		t.Errorf("go.mod requires %s %s but nats-server %s requires %s; keep them equal",
			jwtModule, ours, serverVersion, theirs)
	}
}
