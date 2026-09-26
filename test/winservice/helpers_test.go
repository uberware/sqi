// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build winservice && windows

package winservice

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var binDir string // built once by TestMain, in a path containing a space

// TestMain checks elevation before anything else: an unelevated run builds
// nothing, creates nothing and runs no test.
func TestMain(m *testing.M) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		fmt.Println("winservice: not elevated — the service suite needs an elevated (Administrator) shell")
		os.Exit(0)
	}
	os.Exit(runSuite(m))
}

// runSuite builds both binaries and runs the tests. It is split from TestMain
// so the binary directory is removed on every path, a failed build included.
func runSuite(m *testing.M) int {
	// Under %ProgramData% so a --user service account can execute it, and
	// with a space in the path to exercise ImagePath quoting.
	dir, err := os.MkdirTemp(os.Getenv("ProgramData"), "sqi winservice bin ")
	if err != nil {
		fmt.Println(err)
		return 1
	}
	defer os.RemoveAll(dir) // best-effort cleanup
	binDir = dir
	// go test runs a test binary in its package directory, test/winservice.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		fmt.Println(err)
		return 1
	}
	// One build loads the package graph once and links both binaries into dir.
	cmd := exec.CommandContext(context.Background(), "go", "build",
		"-o", dir+string(os.PathSeparator), "./cmd/sqi-server", "./cmd/sqi-worker")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Printf("build: %v\n%s", err, out)
		return 1
	}
	return m.Run()
}

func bin(name string) string { return filepath.Join(binDir, name+".exe") }

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// uniqueName is prefix plus 8 random hex digits, so a service or account left
// behind by a crashed run never collides with this one.
func uniqueName(t *testing.T, prefix string) string {
	t.Helper()
	return prefix + "-" + randomHex(t, 4)
}

// testDir is under %ProgramData% (not the admin's %TEMP%) so a service running
// as another account can reach it, and contains a space.
func testDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(os.Getenv("ProgramData"), "sqi winservice ") //nolint:usetesting // t.TempDir is under the admin's profile, unreachable by a --user service
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// freePorts returns n distinct free loopback ports. Every listener stays open
// until all n are chosen, so the same port cannot be handed out twice.
func freePorts(t *testing.T, n int) []int {
	t.Helper()
	ports := make([]int, 0, n)
	for range n {
		l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		addr, ok := l.Addr().(*net.TCPAddr)
		if !ok {
			t.Fatalf("listener address %T is not TCP", l.Addr())
		}
		ports = append(ports, addr.Port)
	}
	return ports
}

// hostPort is 127.0.0.1:port.
func hostPort(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }

// run runs exe and returns its combined output. A test body passes
// t.Context(); a cleanup must pass a context that outlives the test, because
// t.Context() is canceled just before cleanups run.
func run(ctx context.Context, stdin, exe string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, exe, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func mustRun(t *testing.T, exe string, args ...string) string {
	t.Helper()
	out, err := run(t.Context(), "", exe, args...)
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", filepath.Base(exe), args, err, out)
	}
	return out
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The field prefixes `service status` prints (printStatus in
// internal/winsvc/command_windows.go); each field ends its own line.
const (
	statusState     = "state:      "
	statusExitCodes = "exit codes: "
	statusAccount   = "account:    "
	statusStartType = "start type: "
)

// serviceNotFound is the error `service status` and `service uninstall`
// report for a name the SCM does not know (winsvc.ErrServiceNotFound).
const serviceNotFound = "service not found"

// serverFixture is a sqi-server service installed and started on free ports.
type serverFixture struct {
	name, dir, cfg, httpURL, natsURL string
}

// startServer installs and starts a sqi-server service on free ports, and
// removes it (and its default log) when the test ends.
func startServer(t *testing.T) serverFixture {
	t.Helper()
	dir := testDir(t)
	ports := freePorts(t, 2)
	cfg := filepath.Join(dir, "sqi-server.yaml")
	writeFile(t, cfg, serverYAML(ports[0], ports[1]))
	name := uniqueName(t, "sqi-test-server")
	cleanupService(t, bin("sqi-server"), name)
	mustRun(t, bin("sqi-server"), "service", "install", "--name", name, "--config", cfg, "--start")
	f := serverFixture{
		name: name, dir: dir, cfg: cfg,
		httpURL: "http://" + hostPort(ports[0]),
		natsURL: "nats://" + hostPort(ports[1]),
	}
	waitHTTP(t, f.httpURL+"/healthz")
	return f
}

func serverYAML(httpPort, natsPort int) string {
	return "http:\n  addr: " + strconv.Quote(hostPort(httpPort)) +
		"\nnats:\n  addr: " + strconv.Quote(hostPort(natsPort)) +
		"\ndiscovery:\n  enabled: false\n"
}

func workerYAML(t *testing.T, natsURL, dataDir string) string {
	t.Helper()
	return "nats:\n  url: " + strconv.Quote(natsURL) +
		"\ndiscovery:\n  enable_mdns: false\nworker:\n  data_dir: " + strconv.Quote(dataDir) +
		"\nmetrics:\n  addr: " + strconv.Quote(hostPort(freePorts(t, 1)[0])) + "\n"
}

// cleanupService uninstalls name (stopping it first) and removes its default
// log when the test ends. Call it before installing, so a half-finished
// install is removed too. An uninstall that fails for any reason but the
// service already being gone is reported, since it leaves a service behind.
func cleanupService(t *testing.T, exe, name string) {
	t.Helper()
	ctx := context.WithoutCancel(t.Context())
	t.Cleanup(func() {
		if out, err := run(ctx, "", exe, "service", "uninstall", "--name", name); err != nil &&
			!strings.Contains(out, serviceNotFound) {
			t.Errorf("cleanup: uninstall %s: %v\n%s", name, err, out)
		}
		os.Remove(defaultLogPath(name))
	})
}

// logDir is the log directory every sqi service shares by default
// (winsvc.DefaultLogPath).
func logDir() string { return filepath.Join(os.Getenv("ProgramData"), "sqi", "logs") }

func defaultLogPath(name string) string { return filepath.Join(logDir(), name+".log") }

// waitServiceState polls `service status` until the service is in state, and
// returns that status output.
func waitServiceState(t *testing.T, exe, name, state string, within time.Duration) string {
	t.Helper()
	var last string
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		out, err := run(t.Context(), "", exe, "service", "status", "--name", name)
		last = out
		if err == nil && strings.Contains(out, statusState+state+"\n") {
			return out
		}
	}
	t.Fatalf("service %s never reached state %q within %v; last status:\n%s", name, state, within, last)
	return ""
}

func waitHTTP(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if code, err := getStatus(t.Context(), url); err == nil && code == http.StatusOK {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s never answered 200", url)
}

func getStatus(ctx context.Context, url string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

func workerStatus(t *testing.T, httpURL, workerID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpURL+"/api/v1/workers", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var list struct {
		Items []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return ""
	}
	for _, w := range list.Items {
		if w.ID == workerID {
			return w.Status
		}
	}
	return ""
}

func waitWorkerStatus(t *testing.T, httpURL, workerID, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if workerStatus(t, httpURL, workerID) == want {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("worker %s never became %q within %v (last %q)", workerID, want, within, workerStatus(t, httpURL, workerID))
}

// readWorkerID waits for the worker to persist its ID. The file is written
// with os.WriteFile, which creates it empty first, so an empty read is retried.
func readWorkerID(t *testing.T, dataDir string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(filepath.Join(dataDir, "worker.id")); err == nil {
			if id := strings.TrimSpace(string(b)); id != "" {
				return id
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("worker.id never written")
	return ""
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// ── Throwaway local account ─────────────────────────────────────────────────
//
// The account is created and removed through the Win32 APIs rather than
// `net user`, so its password never appears on a command line (which process
// auditing can record). Its removal also undoes what `service install --user`
// and running the service leave keyed to its SID — the "Log on as a service"
// right, the ACE on the shared log directory, the profile — because deleting
// an account removes none of them, and this suite also runs on developer
// machines (scripts/test-isolation-windows.ps1 does the same for its accounts).

var (
	modNetapi32                = windows.NewLazySystemDLL("netapi32.dll")
	procNetUserAdd             = modNetapi32.NewProc("NetUserAdd")
	procNetUserDel             = modNetapi32.NewProc("NetUserDel")
	modUserenv                 = windows.NewLazySystemDLL("userenv.dll")
	procDeleteProfileW         = modUserenv.NewProc("DeleteProfileW")
	modAdvapi32                = windows.NewLazySystemDLL("advapi32.dll")
	procLsaOpenPolicy          = modAdvapi32.NewProc("LsaOpenPolicy")
	procLsaRemoveAccountRights = modAdvapi32.NewProc("LsaRemoveAccountRights")
	procLsaClose               = modAdvapi32.NewProc("LsaClose")
)

const (
	userPrivUser       = 1       // USER_PRIV_USER
	ufScript           = 0x0001  // UF_SCRIPT
	ufNormalAccount    = 0x0200  // UF_NORMAL_ACCOUNT
	ufDontExpirePasswd = 0x10000 // UF_DONT_EXPIRE_PASSWD
	nerrUserNotFound   = 2221    // NERR_UserNotFound

	policyCreateAccount      = 0x00000010 // POLICY_CREATE_ACCOUNT
	policyLookupNames        = 0x00000800 // POLICY_LOOKUP_NAMES
	statusObjectNameNotFound = 0xC0000034 // STATUS_OBJECT_NAME_NOT_FOUND: the SID holds no rights
)

// userInfo1 is USER_INFO_1 (lmaccess.h).
type userInfo1 struct {
	name        *uint16
	password    *uint16
	passwordAge uint32
	priv        uint32
	homeDir     *uint16
	comment     *uint16
	flags       uint32
	scriptPath  *uint16
}

// lsaObjectAttributes is LSA_OBJECT_ATTRIBUTES (ntsecapi.h).
type lsaObjectAttributes struct {
	Length                   uint32
	RootDirectory            windows.Handle
	ObjectName               uintptr
	Attributes               uint32
	SecurityDescriptor       uintptr
	SecurityQualityOfService uintptr
}

// throwawayAccount creates a local standard user with a random password, and
// registers its removal even if the test fails midway. Call it before the test
// registers anything that uses the account, so that — cleanups running in
// reverse — its services and directories are gone before the account is. The
// password must never be logged.
func throwawayAccount(t *testing.T) (account, password string) {
	t.Helper()
	account = uniqueName(t, "sqisvc") // 15 characters; a local account name allows 20
	// All four character classes, so any password complexity policy accepts it.
	password = "Aa1!" + randomHex(t, 16)
	if err := netUserAdd(account, password); err != nil {
		t.Fatalf("create account %s: %v", account, err)
	}
	ctx := context.WithoutCancel(t.Context())
	var sid *windows.SID // set below; nil until then, when only the account is removed
	t.Cleanup(func() { removeAccount(ctx, t, account, sid) })
	host, err := windows.ComputerName()
	if err != nil {
		t.Fatal(err)
	}
	if sid, _, _, err = windows.LookupSID("", host+`\`+account); err != nil {
		t.Fatalf("look up account %s: %v", account, err)
	}
	return account, password
}

func netUserAdd(account, password string) error {
	n, err := windows.UTF16PtrFromString(account)
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(password)
	if err != nil {
		return err
	}
	ui := userInfo1{name: n, password: p, priv: userPrivUser, flags: ufScript | ufNormalAccount | ufDontExpirePasswd}
	var parmErr uint32
	//nolint:errcheck // LazyProc.Call's error is GetLastError; NetUserAdd returns its NET_API_STATUS
	st, _, _ := procNetUserAdd.Call(0, 1, uintptr(unsafe.Pointer(&ui)), uintptr(unsafe.Pointer(&parmErr)))
	if uint32(st) != 0 {
		return fmt.Errorf("NetUserAdd: NET_API_STATUS %d (parameter %d)", uint32(st), parmErr)
	}
	return nil
}

func netUserDel(account string) error {
	n, err := windows.UTF16PtrFromString(account)
	if err != nil {
		return err
	}
	//nolint:errcheck // LazyProc.Call's error is GetLastError; NetUserDel returns its NET_API_STATUS
	st, _, _ := procNetUserDel.Call(0, uintptr(unsafe.Pointer(n)))
	if s := uint32(st); s != 0 && s != nerrUserNotFound {
		return fmt.Errorf("NetUserDel: NET_API_STATUS %d", s)
	}
	return nil
}

// removeAccount undoes throwawayAccount and what the service left keyed to the
// account's SID (nil when the lookup never succeeded).
func removeAccount(ctx context.Context, t *testing.T, account string, sid *windows.SID) {
	t.Helper()
	if sid != nil {
		removeLogDirGrant(ctx, t, sid.String())
		if err := revokeAllRights(sid); err != nil {
			t.Errorf("cleanup: revoke the rights of %s: %v", account, err)
		}
		deleteProfile(t, account, sid.String())
	}
	if err := netUserDel(account); err != nil {
		t.Errorf("cleanup: delete account %s: %v", account, err)
	}
}

// removeLogDirGrant removes the inheritable Modify ACE `service install
// --user` adds to the shared log directory (winsvc.EnsureLogDir), or the
// full-control one when that install created the directory.
func removeLogDirGrant(ctx context.Context, t *testing.T, sid string) {
	t.Helper()
	dir := logDir()
	if _, err := os.Stat(dir); err != nil {
		return
	}
	if out, err := run(ctx, "", "icacls", dir, "/remove:g", "*"+sid); err != nil {
		t.Errorf("cleanup: remove %s from the ACL of %s: %v\n%s", sid, dir, err, out)
	}
}

// revokeAllRights removes every LSA right sid holds — "Log on as a service",
// granted by `service install --user` — and with it the SID's LSA account
// object. The account is this test's own, so it holds nothing else.
func revokeAllRights(sid *windows.SID) error {
	var attrs lsaObjectAttributes
	attrs.Length = uint32(unsafe.Sizeof(attrs))
	var policy windows.Handle
	//nolint:errcheck // LazyProc.Call's error is GetLastError, unrelated to the NTSTATUS return
	st, _, _ := procLsaOpenPolicy.Call(0, uintptr(unsafe.Pointer(&attrs)),
		policyCreateAccount|policyLookupNames, uintptr(unsafe.Pointer(&policy)))
	if uint32(st) != 0 {
		return fmt.Errorf("LsaOpenPolicy: NTSTATUS %#x", uint32(st))
	}
	defer procLsaClose.Call(uintptr(policy)) //nolint:errcheck // handle cleanup; nothing to do on failure
	// AllRights TRUE: UserRights and CountOfRights are ignored.
	//nolint:errcheck // LazyProc.Call's error is GetLastError, unrelated to the NTSTATUS return
	st, _, _ = procLsaRemoveAccountRights.Call(uintptr(policy), uintptr(unsafe.Pointer(sid)), 1, 0, 0)
	if s := uint32(st); s != 0 && s != statusObjectNameNotFound {
		return fmt.Errorf("LsaRemoveAccountRights: NTSTATUS %#x", s)
	}
	return nil
}

// deleteProfile removes the profile the SCM loaded to run the service as the
// account (its directory under the profiles root, and its ProfileList entry).
// The hive can stay loaded briefly after the service stops, so a failure is
// retried; a profile that was never created is not an error. One that cannot
// be removed is only reported: Windows sometimes holds a hive until reboot.
func deleteProfile(t *testing.T, account, sid string) {
	t.Helper()
	p, err := windows.UTF16PtrFromString(sid)
	if err != nil {
		t.Errorf("cleanup: %v", err)
		return
	}
	var last error
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(time.Second) {
		r1, _, e1 := procDeleteProfileW.Call(uintptr(unsafe.Pointer(p)), 0, 0)
		if r1 != 0 || errors.Is(e1, windows.ERROR_FILE_NOT_FOUND) || errors.Is(e1, windows.ERROR_PATH_NOT_FOUND) {
			return
		}
		last = e1
		if time.Now().After(deadline) {
			break
		}
	}
	t.Logf("warning: cleanup could not delete the profile of %s (%s): %v; remove it by hand", account, sid, last)
}
