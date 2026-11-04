// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/ha"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlite"
)

// haStatusFixture creates a migrated database holding a three-server group:
// "sched-a" leads, "sched-b" is a standby and "sched-c" was last seen an hour
// ago. It returns the database path.
func haStatusFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	createTestDB(t, path)
	st, err := sqlite.Open(t.Context(), path, sqlite.Options{AutoMigrate: false})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []store.ServerMember{
		{InstanceID: "ia", Name: "sched-a", Host: "host-a", Version: "v0.4.0", PID: 1, StartedAt: time.Now(), BrokerOK: true},
		{InstanceID: "ib", Name: "sched-b", Host: "host-b", Version: "v0.4.1", PID: 2, StartedAt: time.Now()},
		{InstanceID: "ic", Name: "sched-c", Host: "host-c", Version: "v0.4.0", PID: 3, StartedAt: time.Now()},
	} {
		if err := st.UpsertServerMember(t.Context(), m); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok, err := st.AcquireLeadership(t.Context(), "ia", time.Minute); err != nil || !ok {
		t.Fatalf("acquire = %v, %v", ok, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(t.Context(),
		`UPDATE server_members SET last_seen_us = last_seen_us - 3600000000 WHERE instance_id = 'ic'`); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHAStatusCLI_PrintsTable(t *testing.T) {
	isolateConfigDiscovery(t)
	db := haStatusFixture(t) // no server is running against it

	prepareRoot([]string{"ha", "status", "--db", db})
	out := captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("ha status: %v", err)
		}
	})

	lines := strings.Split(strings.TrimSpace(out), "\n")
	var header string
	for _, l := range lines {
		if strings.HasPrefix(l, "NAME") {
			header = l
		}
	}
	if header == "" {
		t.Fatalf("no table header in:\n%s", out)
	}
	last := -1
	for _, col := range []string{"NAME", "ROLE", "HOST", "VERSION", "LAST SEEN"} {
		i := strings.Index(header, col)
		if i <= last {
			t.Fatalf("header %q: column %s missing or out of order", header, col)
		}
		last = i
	}
	rows := lines[len(lines)-3:]
	for i, want := range [][]string{
		{"sched-a", "leader", "host-a", "v0.4.0"},
		{"sched-b", "standby", "host-b", "v0.4.1"},
		{"sched-c", "missing", "host-c", "v0.4.0"},
	} {
		fields := strings.Fields(rows[i])
		for j, w := range want {
			if fields[j] != w {
				t.Errorf("row %d = %q, field %d want %q", i, rows[i], j, w)
			}
		}
	}
	if !strings.Contains(rows[2], "1h0m") {
		t.Errorf("missing server's row does not show it was last seen an hour ago: %q", rows[2])
	}
	if !strings.Contains(out, "Epoch:     1\n") {
		t.Errorf("output does not show the epoch:\n%s", out)
	}
	if !strings.Contains(out, "HA:") || !strings.Contains(out, "not enabled in the configuration") {
		t.Errorf("output does not say HA is off in the configuration:\n%s", out)
	}
}

func TestHAStatusCLI_ReportsHAEnabledInTheConfig(t *testing.T) {
	isolateConfigDiscovery(t)
	db := haStatusFixture(t)
	cfg := filepath.Join(t.TempDir(), "sqi-server.yaml")
	if err := os.WriteFile(cfg, []byte("store:\n  sqlite_path: "+db+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withConfigFile(t, cfg)
	t.Setenv("SQI_HA_ENABLED", "true")
	t.Setenv("SQI_HA_LEASE_TTL", "6s")

	prepareRoot([]string{"ha", "status", "-c", cfg})
	out := captureStdout(t, func() {
		// HA needs postgres, so the config does not validate; the command
		// reads the database whatever the server would accept.
		if err := Execute(); err != nil {
			t.Fatalf("ha status: %v", err)
		}
	})
	if !strings.Contains(out, "enabled") || strings.Contains(out, "not enabled") {
		t.Errorf("HA should read as enabled:\n%s", out)
	}
}

func TestWriteHAStatus_Formats(t *testing.T) {
	var buf bytes.Buffer
	roster := ha.Roster{Epoch: 2, Members: []ha.RosterMember{
		{Name: "a", Role: ha.RoleLeader, Host: "h", Version: "v", LastSeenAgo: 1500 * time.Millisecond, BrokerOK: true},
		{Name: "b", Role: ha.RoleMissing, LastSeenAgo: 90 * time.Second},
	}}
	if err := writeHAStatus(&buf, true, 15*time.Second, roster); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"enabled", "15s", "Epoch:", "1.5s ago", "1m30s ago"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// A server with no host or version prints a dash, not a gap that shifts the columns.
	if !strings.Contains(out, "-") {
		t.Errorf("empty fields should print -:\n%s", out)
	}
}

func TestHAStatusCLI_NoMembers(t *testing.T) {
	isolateConfigDiscovery(t)
	db := filepath.Join(t.TempDir(), "empty.db")
	createTestDB(t, db)

	prepareRoot([]string{"ha", "status", "--db", db})
	out := captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("ha status: %v", err)
		}
	})
	if !strings.Contains(out, "no servers") {
		t.Errorf("output = %q, want a line saying no servers have registered", out)
	}
}

func TestHAStatusCLI_TakesNoLock(t *testing.T) {
	isolateConfigDiscovery(t)
	db := haStatusFixture(t)
	// A server holds the single-server lock for as long as its store is open.
	srv, err := sqlite.Open(t.Context(), db, sqlite.Options{ServerLock: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	prepareRoot([]string{"ha", "status", "--db", db})
	out := captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("ha status beside a server: %v", err)
		}
	})
	if !strings.Contains(out, "sched-a") {
		t.Errorf("output lacks the members:\n%s", out)
	}

	// And it left the lock with its owner: a second server is still refused.
	if _, err := sqlite.Open(t.Context(), db, sqlite.Options{ServerLock: true}); err == nil {
		t.Error("second server opened while the first holds the lock")
	}
}

func TestHAStatusCLI_NoDatabase(t *testing.T) {
	isolateConfigDiscovery(t)
	prepareRoot([]string{"ha", "status", "--db", filepath.Join(t.TempDir(), "nope.db")})
	err := Execute()
	if err == nil || !strings.Contains(err.Error(), "no database at") {
		t.Fatalf("ha status on a missing database = %v, want a no-database error", err)
	}
}
