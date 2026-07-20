// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

// ldap_test.go — the real-directory regression guard for LDAP authentication
// (Phase 3, component C1).
//
// # Why this file exists
//
// Every other test of the LDAP feature drives a fake connection
// (internal/auth/ldap/fakeconn_test.go). Those cover sqi's own logic
// thoroughly — routing, provisioning, role mapping, collisions, the equalized
// 401 — but they cannot catch a mistake in how sqi uses go-ldap on the wire:
// a wrong search scope, a misnamed attribute, a filter a real server rejects,
// or a server that answers an unsupported request in a way the fake never
// would. Such a bug passes CI and fails against a live directory.
//
// That is not hypothetical. Manual verification against a real OpenLDAP found
// that it does not *reject* the Active-Directory-only nested-group matching
// rule — it rewrites the filter to "(?=undefined)" and answers SUCCESS with
// zero entries. Under an earlier revision, that empty result replaced the
// user's real groups and silently demoted every nested-group admin to
// default_role. No fake reproduced it, because no fake would have thought to.
// TestLDAP_NestedGroupExpansionFallsBackOnNonAD is that bug's guard.
//
// # What it runs against
//
// A throwaway OpenLDAP container, started and torn down per test run. Set
// SQI_TEST_LDAP_URL to point at a directory you already have (including a real
// Active Directory) and the container is skipped — see startDirectory.
//
// Skips cleanly when Docker is unavailable, so it never blocks a developer who
// has not installed it.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/scheduler"
	"github.com/uberware/sqi/internal/server"
)

// ── Directory fixture ─────────────────────────────────────────────────────────

const (
	// ldapImage is pinned rather than floating on :latest so a silent upstream
	// change cannot turn this guard red for reasons unrelated to sqi. It is
	// multi-arch (amd64 + arm64), so it runs on both CI and Apple Silicon.
	ldapImage = "osixia/openldap:1.5.0"

	ldapBaseDN  = "dc=example,dc=com"
	ldapAdminDN = "cn=admin,dc=example,dc=com"
	ldapAdminPW = "adminpass"

	// Separate credential for the cn=config tree, needed to widen the default
	// ACL — see aclLDIF.
	ldapConfigDN = "cn=admin,cn=config"
	ldapConfigPW = "configpass"

	// Service account for search-then-bind. A plain inetOrgPerson: it needs
	// only to bind and read, which the container's default ACL already allows.
	ldapSvcDN = "cn=sqi-svc,dc=example,dc=com"
	ldapSvcPW = "svcpass"

	ldapGroupAdmins  = "cn=Farm Admins,ou=groups,dc=example,dc=com"
	ldapGroupArtists = "cn=Artists,ou=groups,dc=example,dc=com"

	// Container start can be slow on a cold image pull or a busy CI runner.
	ldapReadyTimeout = 90 * time.Second
)

// directory is a running LDAP server under test.
type directory struct {
	// URL is the ldap:// endpoint sqi connects to.
	URL string
	// container is the docker container name, empty when the directory was
	// supplied externally via SQI_TEST_LDAP_URL.
	container string
}

// startDirectory returns a directory to test against.
//
// SQI_TEST_LDAP_URL takes precedence: point it at any directory (including a
// real AD) that already holds the fixture tree, and no container is started.
// Otherwise a throwaway OpenLDAP container is started, seeded, and removed via
// t.Cleanup. The test skips — it does not fail — when Docker is unavailable,
// so a developer without Docker is never blocked.
func startDirectory(t *testing.T) *directory {
	t.Helper()

	if url := os.Getenv("SQI_TEST_LDAP_URL"); url != "" {
		t.Logf("using externally provided directory at %s (SQI_TEST_LDAP_URL)", url)
		return &directory{URL: url}
	}

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("skipping LDAP integration test: docker not found on PATH " +
			"(install Docker/colima/podman, or set SQI_TEST_LDAP_URL to an existing directory)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", "info").CombinedOutput(); err != nil {
		t.Skipf("skipping LDAP integration test: docker daemon not responding: %v\n%s", err, out)
	}

	port := freePort(t)
	name := fmt.Sprintf("sqi-ldap-it-%d", port)

	// --rm so an aborted run (SIGINT, panic) does not leak a container; the
	// explicit remove in Cleanup then becomes a no-op rather than the only
	// thing standing between this test and a pile of orphans.
	runCtx, runCancel := context.WithTimeout(context.Background(), ldapReadyTimeout)
	defer runCancel()
	out, err := exec.CommandContext(
		runCtx, "docker", "run", "-d", "--rm",
		"--name", name,
		"-p", fmt.Sprintf("127.0.0.1:%d:389", port),
		"-e", "LDAP_ORGANISATION=Example",
		"-e", "LDAP_DOMAIN=example.com",
		"-e", "LDAP_ADMIN_PASSWORD="+ldapAdminPW,
		"-e", "LDAP_CONFIG_PASSWORD="+ldapConfigPW,
		ldapImage,
	).CombinedOutput()
	if err != nil {
		t.Skipf("skipping LDAP integration test: cannot start %s: %v\n%s", ldapImage, err, out)
	}

	d := &directory{URL: fmt.Sprintf("ldap://127.0.0.1:%d", port), container: name}
	t.Cleanup(func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer rmCancel()
		_ = exec.CommandContext(rmCtx, "docker", "rm", "-f", name).Run() //nolint:errcheck // best-effort teardown
	})

	d.waitReady(t)
	d.seed(t)
	return d
}

// waitReady polls until the directory answers a bind, or fails the test.
// Polling beats a fixed sleep: the container is ready in ~5s locally but can
// take far longer on a cold CI runner, and a sleep tuned for one is either
// flaky or wasteful on the other.
func (d *directory) waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(ldapReadyTimeout)
	var last []byte
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		out, err := exec.CommandContext(
			ctx, "docker", "exec", d.container,
			"ldapwhoami", "-x", "-H", "ldap://localhost", "-D", ldapAdminDN, "-w", ldapAdminPW,
		).CombinedOutput()
		cancel()
		if err == nil {
			return
		}
		last = out
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("directory did not become ready within %s: %s", ldapReadyTimeout, last)
}

// ldapExec runs an LDAP client command inside the container, feeding stdin.
//
// Deliberately in-container rather than on the host: the host may have no
// OpenLDAP client tools at all (common on Linux CI), or a differently-versioned
// set, and macOS ships an Apple-patched build whose behavior differs. Running
// them inside the image makes the fixture identical everywhere.
func (d *directory) ldapExec(t *testing.T, stdin string, args ...string) {
	t.Helper()
	if d.container == "" {
		t.Skip("skipping: fixture mutation requires the managed container (unset SQI_TEST_LDAP_URL)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	full := append([]string{"exec", "-i", d.container}, args...)
	cmd := exec.CommandContext(ctx, "docker", full...)
	cmd.Stdin = strings.NewReader(stdin)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v failed: %v\n%s", args, err, out)
	}
}

// seedLDIF is the fixture tree.
//
// Groups are groupOfUniqueNames/uniqueMember, NOT groupOfNames/member, because
// that is what this image configures its memberof overlay for
// (olcMemberOfGroupOC). Seeding groupOfNames here would leave memberOf empty on
// every entry — every user would silently authenticate and land on
// default_role, which is exactly the misconfiguration docs/auth.md warns about.
// sqi itself is indifferent: it reads whatever DNs memberOf holds.
//
// alice → Farm Admins (maps to admin)
// bob   → Artists     (maps to user)
// carol → no group    (falls through to default_role)
const seedLDIF = `
dn: ou=people,dc=example,dc=com
objectClass: organizationalUnit
ou: people

dn: ou=groups,dc=example,dc=com
objectClass: organizationalUnit
ou: groups

dn: cn=sqi-svc,dc=example,dc=com
objectClass: inetOrgPerson
cn: sqi-svc
sn: Service
userPassword: svcpass

dn: uid=alice,ou=people,dc=example,dc=com
objectClass: inetOrgPerson
uid: alice
cn: Alice Anderson
sn: Anderson
displayName: Alice Anderson
userPassword: alicepass

dn: uid=bob,ou=people,dc=example,dc=com
objectClass: inetOrgPerson
uid: bob
cn: Bob Brown
sn: Brown
displayName: Bob Brown
userPassword: bobpass

dn: uid=carol,ou=people,dc=example,dc=com
objectClass: inetOrgPerson
uid: carol
cn: Carol Clark
sn: Clark
displayName: Carol Clark
userPassword: carolpass

dn: cn=Farm Admins,ou=groups,dc=example,dc=com
objectClass: groupOfUniqueNames
cn: Farm Admins
uniqueMember: uid=alice,ou=people,dc=example,dc=com

dn: cn=Artists,ou=groups,dc=example,dc=com
objectClass: groupOfUniqueNames
cn: Artists
uniqueMember: uid=bob,ou=people,dc=example,dc=com
`

// aclLDIF widens the image's default ACL so search-then-bind can work.
//
// osixia ships `to * by self read by <admin> write by * none`: only an entry
// itself and the directory admin may read anything. That is enough for
// template bind — sqi binds AS the user, then reads that user's own entry — but
// a service account cannot see anyone else, so every search returns "no such
// object" and every login fails. A real deployment grants its service account
// read rights; this makes the fixture match.
//
// Anonymous read is granted too, because TestLDAP_AnonymousSearchBind covers a
// genuinely supported deployment. userPassword stays protected by the first
// rule — `by anonymous auth` permits binding against it and nothing else, so
// no ACL here ever exposes a credential.
//
// The peercred rule is carried over verbatim: it is how the container's own
// tooling administers the database over the local socket, and dropping it
// would break the image in ways unrelated to what is under test.
const aclLDIF = `
dn: olcDatabase={1}mdb,cn=config
changetype: modify
replace: olcAccess
olcAccess: {0}to * by dn.exact=gidNumber=0+uidNumber=0,cn=peercred,cn=external,cn=auth manage by * break
olcAccess: {1}to attrs=userPassword,shadowLastChange by self write by dn="cn=admin,dc=example,dc=com" write by anonymous auth by * none
olcAccess: {2}to * by self read by dn="cn=admin,dc=example,dc=com" write by dn.exact="cn=sqi-svc,dc=example,dc=com" read by users read by anonymous read
`

func (d *directory) seed(t *testing.T) {
	t.Helper()
	d.ldapExec(t, aclLDIF, "ldapmodify", "-x", "-H", "ldap://localhost", "-D", ldapConfigDN, "-w", ldapConfigPW)
	d.ldapExec(t, seedLDIF, "ldapadd", "-x", "-H", "ldap://localhost", "-D", ldapAdminDN, "-w", ldapAdminPW)

	// Assert the fixture is usable before any sqi code runs, and assert it as
	// the SERVICE ACCOUNT rather than as admin — admin can read everything by
	// definition, so an admin-run check would pass against an ACL that denies
	// the service account and leave search-then-bind failing for reasons that
	// look like an sqi bug. This single search covers both fixture hazards:
	// whether the service account may read other entries, and whether the
	// memberof overlay is populating at all.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(
		ctx, "docker", "exec", d.container,
		"ldapsearch", "-x", "-LLL", "-H", "ldap://localhost",
		"-D", ldapSvcDN, "-w", ldapSvcPW,
		"-b", ldapBaseDN, "(uid=alice)", "memberOf",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("fixture verification search (as the service account) failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), ldapGroupAdmins) {
		t.Fatalf("fixture is broken: the service account cannot see alice's memberOf for %q.\n"+
			"Either the ACL denies it read access to other entries, or the memberof overlay "+
			"is not populating. Every role assertion below would be meaningless.\ngot:\n%s",
			ldapGroupAdmins, out)
	}
}

// ── Server fixture ────────────────────────────────────────────────────────────

// baseLDAPConfig is the shared half of every mode's configuration: the two
// group→role rules and a default. Each test supplies the bind-mode fields.
func baseLDAPConfig(url string) config.LDAPConfig {
	return config.LDAPConfig{
		Enabled:         true,
		URL:             url,
		Timeout:         10 * time.Second,
		UsernameAttr:    "uid",
		DisplayNameAttr: "displayName",
		RoleSource:      "directory",
		DefaultRole:     "read-only",
		RoleMap: []config.RoleMappingConfig{
			{Group: ldapGroupAdmins, Role: "admin"},
			{Group: ldapGroupArtists, Role: "user"},
		},
	}
}

// searchBindConfig configures search-then-bind with a service account.
func searchBindConfig(url string) config.LDAPConfig {
	c := baseLDAPConfig(url)
	c.BindDN = ldapSvcDN
	c.BindPassword = ldapSvcPW
	c.BaseDN = ldapBaseDN
	c.UserFilter = "(uid=%s)"
	return c
}

// templateBindConfig configures template bind: no service account at all, so
// group resolution must run on the user's own authenticated connection.
func templateBindConfig(url string) config.LDAPConfig {
	c := baseLDAPConfig(url)
	c.UserDNTemplate = "uid=%s,ou=people,dc=example,dc=com"
	return c
}

// startLDAPServer boots a full sqi-server with auth and the given LDAP config.
//
// Self-contained rather than an option on startServer, following the
// startLoadServer precedent: this file is behind a build tag and the shared
// harness is not, so keeping the variant local avoids changing a helper that
// every untagged test depends on.
func startLDAPServer(t *testing.T, ldapCfg config.LDAPConfig) *testServer {
	t.Helper()

	httpAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	natsAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	tmpDir := t.TempDir()

	cfg := server.Config{
		HTTPAddr:           httpAddr,
		CORSOrigins:        []string{"*"},
		NATSAddr:           natsAddr,
		NATSDataDir:        tmpDir + "/nats",
		NATSMaxStoreMB:     64,
		SQLitePath:         tmpDir + "/sqi-ldap.db",
		CheckpointInterval: time.Minute,
		Scheduler: scheduler.Config{
			AssignInterval:         100 * time.Millisecond,
			AssignBatchSize:        10,
			AssignWorkers:          2,
			WorkerTimeout:          30 * time.Second,
			HeartbeatSweepInterval: 15 * time.Second,
		},
		DiscoveryEnabled: false,

		AuthEnabled:      true,
		AuthSessionTTL:   time.Hour,
		AuthCookieName:   "sqi_session",
		AuthCookieSecure: "false",
		AuthLDAP:         ldapCfg,
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	srv := server.New(cfg, logger, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	select {
	case err := <-done:
		cancel()
		t.Fatalf("server exited during startup: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	ts := &testServer{HTTPAddr: httpAddr, NATSAddr: natsAddr, cancel: cancel, done: done}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	})

	if !waitForTCP(t, httpAddr, 20*time.Second) {
		t.Fatal("HTTP server did not start listening")
	}
	if !waitForReadyz(t, httpAddr, 30*time.Second) {
		t.Fatal("server did not become ready")
	}
	return ts
}

// ── Login helper ──────────────────────────────────────────────────────────────

// loginResult is the subset of the login response these tests assert on.
type loginResult struct {
	Status      int
	Username    string `json:"username"`
	Role        string `json:"role"`
	AuthSource  string `json:"auth_source"`
	DisplayName string `json:"display_name"`
}

// login posts credentials to /auth/login and returns the decoded result.
func login(t *testing.T, ts *testServer, username, password string) loginResult {
	t.Helper()
	body, err := json.Marshal(map[string]string{"username": username, "password": password})
	if err != nil {
		t.Fatalf("marshal login body: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// apiURL does not prepend the API prefix — callers pass the full path.
	// A bare "/auth/login" reaches the embedded-UI catch-all, which answers
	// GET and returns 405 for POST rather than an obvious 404.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		apiURL(ts, "/api/v1/auth/login"), bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build login request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /auth/login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // test cleanup

	out := loginResult{Status: resp.StatusCode}
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode login response: %v", err)
		}
	}
	return out
}

// assertLogin logs in and asserts the account was provisioned from the
// directory with the expected role.
func assertLogin(t *testing.T, ts *testServer, username, password, wantRole string) {
	t.Helper()
	got := login(t, ts, username, password)
	if got.Status != http.StatusOK {
		t.Fatalf("login %q: got HTTP %d, want 200", username, got.Status)
	}
	if got.Role != wantRole {
		t.Errorf("login %q: role = %q, want %q (group→role mapping over the real wire)", username, got.Role, wantRole)
	}
	if got.AuthSource != "ldap" {
		t.Errorf("login %q: auth_source = %q, want ldap", username, got.AuthSource)
	}
}

// assertRejected asserts a login fails with 401 and no session.
func assertRejected(t *testing.T, ts *testServer, username, password, why string) {
	t.Helper()
	if got := login(t, ts, username, password); got.Status != http.StatusUnauthorized {
		t.Errorf("%s: login %q got HTTP %d, want 401", why, username, got.Status)
	}
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestLDAP_SearchThenBind covers the service-account mode end to end: bind as
// the service account, search for the user, re-bind as that user with their
// own password. That third step is the actual authentication — if it were ever
// dropped, any password would succeed, which is why the rejection cases below
// matter as much as the success ones.
func TestLDAP_SearchThenBind(t *testing.T) {
	d := startDirectory(t)
	ts := startLDAPServer(t, searchBindConfig(d.URL))

	t.Run("group maps to admin", func(t *testing.T) {
		assertLogin(t, ts, "alice", "alicepass", "admin")
	})
	t.Run("group maps to user", func(t *testing.T) {
		assertLogin(t, ts, "bob", "bobpass", "user")
	})
	t.Run("no group falls through to default_role", func(t *testing.T) {
		// Also proves memberOf is genuinely being read: if the attribute were
		// never returned, alice and bob would land here too.
		assertLogin(t, ts, "carol", "carolpass", "read-only")
	})
	t.Run("display name comes from the directory", func(t *testing.T) {
		if got := login(t, ts, "alice", "alicepass"); got.DisplayName != "Alice Anderson" {
			t.Errorf("display_name = %q, want %q", got.DisplayName, "Alice Anderson")
		}
	})
	t.Run("wrong password rejected", func(t *testing.T) {
		assertRejected(t, ts, "alice", "wrongpass", "wrong password")
	})
	t.Run("unknown user rejected", func(t *testing.T) {
		assertRejected(t, ts, "nosuchuser", "whatever", "unknown user")
	})
	t.Run("empty password rejected", func(t *testing.T) {
		// A real server treats a bind with an empty password as an
		// *unauthenticated* bind and returns success. Only a real server
		// exercises that; the guard lives in the verifier.
		assertRejected(t, ts, "alice", "", "empty password")
	})
	t.Run("filter injection rejected", func(t *testing.T) {
		// Unescaped, this rewrites "(uid=%s)" into a filter matching every
		// entry in the base DN — an authentication bypass. The real server is
		// what proves the escaping reaches the wire intact.
		assertRejected(t, ts, "*)(uid=*", "whatever", "filter injection")
	})
}

// TestLDAP_TemplateBind covers the no-service-account mode: sqi formats the
// user's DN and binds as them directly, then reads memberOf from their own
// entry on that same authenticated connection.
func TestLDAP_TemplateBind(t *testing.T) {
	d := startDirectory(t)
	ts := startLDAPServer(t, templateBindConfig(d.URL))

	t.Run("group maps to admin", func(t *testing.T) {
		assertLogin(t, ts, "alice", "alicepass", "admin")
	})
	t.Run("group maps to user", func(t *testing.T) {
		assertLogin(t, ts, "bob", "bobpass", "user")
	})
	t.Run("no group falls through to default_role", func(t *testing.T) {
		assertLogin(t, ts, "carol", "carolpass", "read-only")
	})
	t.Run("wrong password rejected", func(t *testing.T) {
		assertRejected(t, ts, "alice", "wrongpass", "wrong password")
	})
	t.Run("unknown user rejected", func(t *testing.T) {
		// No entry exists at the templated DN, so the bind fails outright.
		assertRejected(t, ts, "nosuchuser", "whatever", "unknown user")
	})
	t.Run("empty password rejected", func(t *testing.T) {
		assertRejected(t, ts, "alice", "", "empty password")
	})
	t.Run("dn injection rejected", func(t *testing.T) {
		// Unescaped, the comma grafts extra RDNs onto the template and binds
		// as a different entry entirely.
		assertRejected(t, ts, "x,ou=admins", "whatever", "DN injection")
	})
}

// TestLDAP_AnonymousSearchBind covers search-then-bind with no bind_dn: the
// search runs on an anonymous connection, then sqi binds as the user. This is
// a supported deployment (documented in docs/auth.md) that was briefly broken —
// it reached Bind("", ""), which go-ldap rejects client-side, so every login
// failed as "directory unavailable".
func TestLDAP_AnonymousSearchBind(t *testing.T) {
	d := startDirectory(t)
	cfg := searchBindConfig(d.URL)
	cfg.BindDN = ""
	cfg.BindPassword = ""
	ts := startLDAPServer(t, cfg)

	assertLogin(t, ts, "bob", "bobpass", "user")
	assertRejected(t, ts, "bob", "wrongpass", "wrong password over anonymous search")
}

// TestLDAP_NestedGroupExpansionFallsBackOnNonAD is the guard for the bug that
// justified this whole file.
//
// nested_groups uses LDAP_MATCHING_RULE_IN_CHAIN (1.2.840.113556.1.4.1941),
// which is Active-Directory-only. OpenLDAP does not reject it: it rewrites the
// filter to "(?=undefined)" and returns SUCCESS with zero entries. An earlier
// revision treated that as "the user has no groups" and let the empty result
// replace the real memberOf values, silently demoting every user — admins
// included — to default_role, with no error anywhere.
//
// The fix keeps the flat memberOf values when the nested search returns empty
// but the flat list was not, and logs a WARN. bob must therefore still resolve
// to "user" here, not "read-only".
//
// No fake reproduced this, because the failure is a property of how a real
// non-AD server answers an unsupported matching rule.
func TestLDAP_NestedGroupExpansionFallsBackOnNonAD(t *testing.T) {
	d := startDirectory(t)
	cfg := searchBindConfig(d.URL)
	cfg.NestedGroups = true
	ts := startLDAPServer(t, cfg)

	got := login(t, ts, "bob", "bobpass")
	if got.Status != http.StatusOK {
		t.Fatalf("login: got HTTP %d, want 200", got.Status)
	}
	if got.Role == "read-only" {
		t.Fatal("bob was demoted to default_role: the nested-group expansion " +
			"returned an empty result and it replaced his flat memberOf groups. " +
			"This is the silent-demotion regression this test exists to catch.")
	}
	if got.Role != "user" {
		t.Errorf("role = %q, want user (flat memberOf must survive the failed expansion)", got.Role)
	}
}

// TestLDAP_RoleResyncFromDirectory proves role_source=directory recomputes a
// user's role from live group membership on every login — driven by an actual
// modification to the directory, not a fixture swap.
func TestLDAP_RoleResyncFromDirectory(t *testing.T) {
	d := startDirectory(t)
	ts := startLDAPServer(t, searchBindConfig(d.URL))

	assertLogin(t, ts, "alice", "alicepass", "admin")

	// Farm Admins would be left with no uniqueMember, which the schema
	// forbids, so park the service account there first.
	d.ldapExec(t, `
dn: cn=Farm Admins,ou=groups,dc=example,dc=com
changetype: modify
add: uniqueMember
uniqueMember: cn=sqi-svc,dc=example,dc=com
`, "ldapmodify", "-x", "-H", "ldap://localhost", "-D", ldapAdminDN, "-w", ldapAdminPW)

	d.ldapExec(t, `
dn: cn=Farm Admins,ou=groups,dc=example,dc=com
changetype: modify
delete: uniqueMember
uniqueMember: uid=alice,ou=people,dc=example,dc=com

dn: cn=Artists,ou=groups,dc=example,dc=com
changetype: modify
add: uniqueMember
uniqueMember: uid=alice,ou=people,dc=example,dc=com
`, "ldapmodify", "-x", "-H", "ldap://localhost", "-D", ldapAdminDN, "-w", ldapAdminPW)

	got := login(t, ts, "alice", "alicepass")
	if got.Status != http.StatusOK {
		t.Fatalf("login after group move: got HTTP %d, want 200", got.Status)
	}
	if got.Role != "user" {
		t.Errorf("role = %q, want user: role_source=directory must recompute from live group membership", got.Role)
	}
}
